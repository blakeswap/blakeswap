package daemon

import (
	"context"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/btcsuite/btcd/wire"
	"testing"
)

type archiveForkBackend struct {
	*receiveBackend
	hash    string
	failure bool
}

func (b *archiveForkBackend) BlockHash(context.Context, uint32) (string, error) {
	if b.failure {
		return "", errors.New("offline fixture")
	}
	return b.hash, nil
}

func TestArchiveBoundaryReorgPersistsHoldAndRequiresPositivePaymentProof(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			e, backends := receiveEngine(t)
			backend := &archiveForkBackend{receiveBackend: backends[id], hash: "test-canonical-tip"}
			e.nodes[id] = backend
			e.heights[id] = 200
			if err := e.refreshArchiveCheckpoint(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{Sequence: wire.MaxTxInSequenceNum})
			tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: e.scripts[id]})
			raw := contract.Hex(tx)
			txid := tx.TxHash().String()
			send := &WalletSend{PublicSend: PublicSend{ID: "old-settled-send", Chain: id, TxID: txid, Confirmations: 200, State: "confirmed"}, Raw: raw, History: []SignedVariant{{PublicVariant: PublicVariant{TxID: txid}, Raw: raw}}}
			e.s.Sends = map[string]*WalletSend{send.ID: send}
			e.noteArchivePayment(send, txid, chain.Transaction{TxID: txid, Hex: raw, Height: 1, Confirmations: 200})
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := e.compactArchive(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if e.s.Sends[send.ID] != nil || e.s.Capacity.Archived.Kinds["sends"] != 1 {
				t.Fatal("positive deep payment did not archive")
			}
			// An unavailable old anchor preserves history; absence is not a reorg.
			backend.failure = true
			if err := e.refreshArchiveCheckpoint(context.Background(), id); err == nil {
				t.Fatal("fixture did not exercise outage")
			}
			if e.s.Capacity.Reactivating || canChangeNetwork(e.s) != nil {
				t.Fatal("ordinary outage manufactured an obligation")
			}
			backend.failure = false
			backend.hash = "different-canonical-fork"
			if err := e.refreshArchiveCheckpoint(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if !saved.Capacity.Reactivating || canChangeNetwork(saved) == nil {
				t.Fatal("known contradiction did not durably hold monitoring")
			}
			// Simulated reopen begins from the last complete persisted checkpoint.
			e.s = saved
			e.semanticParts = nil
			e.archiveCurrent = nil
			if err := e.reactivateArchive(); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if e.s.Sends[send.ID] == nil || !e.s.Capacity.Invalidated["send/"+send.ID] || canChangeNetwork(e.s) == nil {
				t.Fatal("reactivation lost recovery identity/hold")
			}
			e.reconcileArchiveHolds(nil, nil)
			if !e.s.Capacity.Invalidated["send/"+send.ID] {
				t.Fatal("old count cleared known reorg hold")
			}
			if err := e.refreshArchiveCheckpoint(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			e.noteArchivePayment(e.s.Sends[send.ID], txid, chain.Transaction{TxID: txid, Hex: raw, Height: 2, Confirmations: 199})
			e.reconcileArchiveHolds(nil, nil)
			if len(e.s.Capacity.Invalidated) != 0 || e.s.Capacity.Reactivating || canChangeNetwork(e.s) != nil {
				t.Fatal("positive current payment did not clear its own hold")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := e.BackupSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			complete, err := CompleteState(snapshot)
			if err != nil || complete.Sends[send.ID].Raw != raw {
				t.Fatal("cross-boundary reorg lost exact signed payment", err)
			}
		})
	}
}
