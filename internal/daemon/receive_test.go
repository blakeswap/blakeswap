package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

type receiveBackend struct {
	chain.Backend
	used                       map[string]bool
	coins                      []chain.UTXO
	observed                   []string
	queried                    []string
	historyError, observeError error
}

func (b *receiveBackend) Observe(_ context.Context, _ string, addresses []string) (chain.Backend, error) {
	if b.observeError != nil {
		return nil, b.observeError
	}
	b.observed = append(b.observed, addresses...)
	return b, nil
}
func (b *receiveBackend) ConfirmedReceived(_ context.Context, address string) (bool, error) {
	return b.used[address], b.historyError
}
func (b *receiveBackend) Unspent(_ context.Context, addresses []string) ([]chain.UTXO, error) {
	b.queried = addresses
	return b.coins, nil
}
func (b *receiveBackend) Height(context.Context) (uint32, error) { return 200, nil }

func receiveEngine(t *testing.T) (*Engine, map[chain.ID]*receiveBackend) {
	t.Helper()
	mnemonic, _ := wallet.NewMnemonic()
	keys, _ := wallet.FromMnemonic(mnemonic)
	v, err := storage.Open(filepath.Join(t.TempDir(), "state.db"), []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	e := &Engine{Config: Config{Network: chain.Regtest}, keys: keys, identity: nostr.Generate(), vault: v,
		s: State{Version: 1, Mnemonic: mnemonic}, nodes: map[chain.ID]chain.Backend{}, watch: map[chain.ID]chain.Backend{},
		addresses: map[chain.ID]string{}, scripts: map[chain.ID][]byte{}, heights: map[chain.ID]uint32{}, clocks: map[chain.ID]uint32{}, balances: map[chain.ID]int64{}}
	backends := map[chain.ID]*receiveBackend{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		b := &receiveBackend{used: map[string]bool{}}
		e.nodes[id] = b
		backends[id] = b
		if err := e.loadReceiveAddresses(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	return e, backends
}

func TestReceiveRotationRetainsFundsAndPersistsAcrossRestart(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			e, backends := receiveEngine(t)
			b := backends[id]
			ctx := context.Background()
			original, other := e.addresses[id], e.addresses[id.Other()]
			b.coins = []chain.UTXO{{Amount: 1000000, Script: hex.EncodeToString(e.scripts[id]), Confirmations: 0}}
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			if e.addresses[id] != original {
				t.Fatal("mempool rotated address")
			}
			b.used[original] = true
			b.coins[0].Confirmations = 1
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			replacement := e.addresses[id]
			if replacement == original || replacement == "" || e.addresses[id.Other()] != other {
				t.Fatal("wrong chain rotation")
			}
			if len(b.queried) != 2 || b.queried[0] != original || e.balances[id] != 0 {
				t.Fatal("old balance lost")
			}
			b.coins[0].Confirmations = e.Config.Network.Confirmations()
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			if e.balances[id] != 1000000 {
				t.Fatal("old confirmed balance lost")
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if saved.ReceiveIndexes[id] != 1 {
				t.Fatal("rotation not durable")
			}
			e.s = saved
			if err := e.loadReceiveAddresses(ctx, id); err != nil {
				t.Fatal(err)
			}
			delete(b.used, original)
			b.coins = nil // A reorg/spend must never roll the index back.
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			if e.addresses[id] != replacement {
				t.Fatal("reused after restart or reorg")
			}
			b.used[original] = true // Late payment to an old address doesn't consume the unused current one.
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			if e.addresses[id] != replacement {
				t.Fatal("old receipt caused repeated rotation")
			}
			b.used[replacement] = true
			if err := e.refreshChain(ctx, id); err != nil {
				t.Fatal(err)
			}
			if e.addresses[id] == replacement || e.s.ReceiveIndexes[id] != 2 {
				t.Fatal("second receipt did not rotate")
			}
		})
	}
}

func TestReceiveHistoryRecoveryIncludesSpentAddresses(t *testing.T) {
	e, backends := receiveEngine(t)
	ctx := context.Background()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		// Missing indexes represent a legacy state or phrase restore. Even emptied
		// addresses in the confirmed history must be traversed before receiving.
		for i := uint32(0); i < 3; i++ {
			a, err := e.deriveReceive(id, i)
			if err != nil {
				t.Fatal(err)
			}
			backends[id].used[a.address] = true
		}
		if err := e.refreshChain(ctx, id); err != nil {
			t.Fatal(err)
		}
		want, _ := e.deriveReceive(id, 3)
		if e.addresses[id] != want.address || len(e.receiveBook[id]) != 4 {
			t.Fatal("recovery stopped at spent receipt")
		}
	}
}

func TestReceiveRotationFailsClosedAndRetries(t *testing.T) {
	e, backends := receiveEngine(t)
	id := chain.BTC
	b := backends[id]
	ctx := context.Background()
	old := e.addresses[id]
	b.historyError = errors.New("invalid inclusion proof")
	if e.refreshChain(ctx, id) == nil || e.s.ReceiveIndexes[id] != 0 {
		t.Fatal("advanced on failed history")
	}
	b.historyError = nil
	b.used[old] = true
	b.observeError = errors.New("import interrupted")
	if e.refreshChain(ctx, id) == nil || e.s.ReceiveIndexes[id] != 0 || e.addresses[id] != "" {
		t.Fatal("published used or unwatched address")
	}
	b.observeError = nil
	if err := e.refreshChain(ctx, id); err != nil {
		t.Fatal(err)
	}
	if e.s.ReceiveIndexes[id] != 1 || e.addresses[id] == "" {
		t.Fatal("did not retry")
	}
	b.used[e.addresses[id]] = true
	e.vault.Close()
	if e.refreshChain(ctx, id) == nil || e.fatal == nil || e.addresses[id] != "" {
		t.Fatal("published address after durability failure")
	}
}

func TestRealReceiveRotationSpendsMultipleHistoricalAddresses(t *testing.T) {
	h := newHarness(t, 0)
	e := h.engines["maker"]
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		first := e.addresses[id]
		h.command("maker", "regtest.faucet", map[string]any{"chain": id, "amount": 2000000})
		if e.addresses[id] != first {
			t.Fatal("rotated before confirmation")
		}
		h.mine(id, 1)
		if err := e.refreshChain(h.ctx, id); err != nil {
			t.Fatal(err)
		}
		current := e.addresses[id]
		if first == current {
			t.Fatal("real confirmed receipt did not rotate")
		}
		h.mine(id, 1) // Funding retains the existing two-confirmation policy.
		key, err := e.swapKey(id, "receive-spending-test")
		if err != nil {
			t.Fatal(err)
		}
		pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
		c := contract.HTLC{Chain: id, Hash: strings.Repeat("01", 32), ClaimKey: pub, RefundKey: pub, RefundHeight: e.heights[id] + 20, Amount: 101000000}
		tx, err := e.fund(h.ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if len(tx.TxIn) < 2 {
			t.Fatal("test did not select historical keys")
		}
		if _, err := h.nodes[id].Broadcast(h.ctx, contract.Hex(tx)); err != nil {
			t.Fatal("node rejected historical signatures", err)
		}
		h.mine(id, 2)
		if err := e.refreshChain(h.ctx, id); err != nil {
			t.Fatal(err)
		}
		if e.addresses[id] == current {
			t.Fatal("confirmed change did not rotate")
		}
		if e.balances[id] != 1000000-protocol.FundingFee {
			t.Fatal("lost change balance", e.balances[id])
		}
	}
	expected := map[chain.ID]string{chain.BTC: e.addresses[chain.BTC], chain.Blake: e.addresses[chain.Blake]}
	// Simulate restoring an older backup with no address counters after spending
	// all earlier receipts. Backend history must discover the funded change.
	e.s.ReceiveIndexes = nil
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	h.offline("maker")
	h.online("maker")
	for id, address := range expected {
		if h.engines["maker"].addresses[id] != address {
			t.Fatal("restoration reused spent address", id)
		}
	}
}
