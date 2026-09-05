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
