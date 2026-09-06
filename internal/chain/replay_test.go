package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"testing"
)

type proofBackend struct {
	Backend
	txs   map[string]Transaction
	coins map[uint32]Transaction
}

func (b *proofBackend) Transaction(_ context.Context, id string) (Transaction, error) {
	if tx, ok := b.txs[id]; ok {
		return tx, nil
	}
	return Transaction{}, errors.New("missing transaction")
}
func (b *proofBackend) Coinbase(_ context.Context, height uint32) (Transaction, error) {
	if tx, ok := b.coins[height]; ok {
		return tx, nil
	}
	return Transaction{}, errors.New("missing coinbase")
}
func proofCoin(height uint32, tag byte) Transaction {
	tx := wire.NewMsgTx(2)
	script, _ := txscript.NewScriptBuilder().AddInt64(int64(height)).AddData([]byte{tag}).Script()
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: ^uint32(0)}, script, nil))
	tx.AddTxOut(wire.NewTxOut(100000000, []byte{txscript.OP_TRUE}))
	var raw bytes.Buffer
	_ = tx.Serialize(&raw)
	return Transaction{TxID: tx.TxHash().String(), Hex: hex.EncodeToString(raw.Bytes()), Height: height, Confirmations: 200}
}
func TestReplayProofRequiresDivergentPostForkCoinbase(t *testing.T) {
	network := Testnet
	height := network.ForkHeight() + 100
	coin := proofCoin(height, 1)
	other := proofCoin(height, 2)
	btc := &proofBackend{txs: map[string]Transaction{coin.TxID: coin}, coins: map[uint32]Transaction{height: coin}}
	blake := &proofBackend{coins: map[uint32]Transaction{height: other}}
	outpoint, _ := WireOutpoint(coin.TxID, 0)
	funding := wire.NewMsgTx(2)
	funding.AddTxIn(wire.NewTxIn(&outpoint, nil, nil))
	funding.AddTxOut(wire.NewTxOut(100000, []byte{txscript.OP_TRUE}))
	if err := ProveBTCExclusive(context.Background(), network, btc, blake, funding); err != nil {
		t.Fatal(err)
	}
	blake.coins[height] = coin
	if !errors.Is(ProveBTCExclusive(context.Background(), network, btc, blake, funding), ErrReplayUnsafe) {
		t.Fatal("shared coinbase accepted")
	}
	delete(blake.coins, height)
	if ProveBTCExclusive(context.Background(), network, btc, blake, funding) == nil {
		t.Fatal("missing on other chain accepted as proof")
	}
	blake.coins[height] = other
	fake := coin
	fake.Hex = other.Hex
	btc.txs[coin.TxID] = fake
	if ProveBTCExclusive(context.Background(), network, btc, blake, funding) == nil {
		t.Fatal("unbound transaction accepted")
	}
	old := proofCoin(network.ForkHeight()-1, 1)
	btc.txs = map[string]Transaction{old.TxID: old}
	oldPoint, _ := WireOutpoint(old.TxID, 0)
	funding.TxIn[0].PreviousOutPoint = oldPoint
	if !errors.Is(ProveBTCExclusive(context.Background(), network, btc, blake, funding), ErrReplayUnsafe) {
		t.Fatal("pre-fork coinbase accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(ProveBTCExclusive(ctx, network, btc, blake, funding), context.Canceled) {
		t.Fatal("proof ignored cancellation")
	}
}

func TestReplayProofMixedInputSetBoundsAndReorg(t *testing.T) {
	network := Testnet
	height := network.ForkHeight() + 100
	shared, exclusive, other := proofCoin(height, 1), proofCoin(height+1, 2), proofCoin(height+1, 3)
	btc := &proofBackend{txs: map[string]Transaction{shared.TxID: shared, exclusive.TxID: exclusive}, coins: map[uint32]Transaction{height: shared, height + 1: exclusive}}
	blake := &proofBackend{coins: map[uint32]Transaction{height: shared, height + 1: other}}
	tx := wire.NewMsgTx(2)
	for _, coin := range []Transaction{shared, exclusive} {
		point, _ := WireOutpoint(coin.TxID, 0)
		tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	}
	if err := ProveBTCExclusive(context.Background(), network, btc, blake, tx); err != nil {
		t.Fatal("one exclusive input must protect a mixed set", err)
	}
	// Replacing the observed opposite-chain block removes proof immediately.
	blake.coins[height+1] = exclusive
	if err := ProveBTCExclusive(context.Background(), network, btc, blake, tx); !errors.Is(err, ErrReplayUnsafe) {
		t.Fatal("stale evidence after reorg", err)
	}
	blake.coins[height+1] = other
	// A legitimate exclusive ancestor beyond the verifier's depth bound remains
	// not proven; exhaustion must never become positive proof.
	tip := exclusive
	for i := 0; i < 66; i++ {
		descendant := wire.NewMsgTx(2)
		point, _ := WireOutpoint(tip.TxID, 0)
		descendant.AddTxIn(wire.NewTxIn(&point, nil, nil))
		descendant.AddTxOut(wire.NewTxOut(int64(1000000-i), []byte{txscript.OP_TRUE}))
		var raw bytes.Buffer
		_ = descendant.Serialize(&raw)
		tip = Transaction{TxID: descendant.TxHash().String(), Hex: hex.EncodeToString(raw.Bytes()), Confirmations: 10}
		btc.txs[tip.TxID] = tip
	}
	deep := wire.NewMsgTx(2)
	point, _ := WireOutpoint(tip.TxID, 0)
	deep.AddTxIn(wire.NewTxIn(&point, nil, nil))
	if err := ProveBTCExclusive(context.Background(), network, btc, blake, deep); !errors.Is(err, ErrReplayUnsafe) {
		t.Fatal("bounded proof should remain unproven", err)
	}
	delete(btc.txs, tip.TxID)
	if err := ProveBTCExclusive(context.Background(), network, btc, blake, deep); err == nil || errors.Is(err, ErrReplayUnsafe) {
		t.Fatal("backend errors must differ from bounded rejection", err)
	}
}
