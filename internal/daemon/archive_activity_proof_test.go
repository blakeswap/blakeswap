package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
)

type activityArchiveBackend struct {
	*receiveBackend
	generation uint64
	hashes     map[uint32]string
	failure    error
	afterRead  func(uint32)
	reads      int
}

func (b *activityArchiveBackend) Generation() uint64 { return b.generation }
func (b *activityArchiveBackend) BlockHash(ctx context.Context, height uint32) (string, error) {
	b.reads++
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if b.failure != nil {
		return "", b.failure
	}
	hash := b.hashes[height]
	if b.afterRead != nil {
		b.afterRead(height)
	}
	return hash, nil
}

func TestActivityArchiveRequiresSelectedCanonicalInclusion(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, mode := range []string{"current", "old-block", "unknown", "no-height", "no-hash", "wrong-tx", "old-generation", "future-time", "insufficient-depth", "unavailable", "generation-during-check", "fork-during-check"} {
			t.Run(string(id)+"/"+mode, func(t *testing.T) {
				e, backends := receiveEngine(t)
				backend := &activityArchiveBackend{receiveBackend: backends[id], generation: 1, hashes: map[uint32]string{1: "included-block", 200: "current-tip"}}
				e.nodes[id] = backend
				e.heights[id] = 200
				e.archiveCurrent = map[chain.ID]recoveryCheckpoint{id: {Height: 200, Hash: "current-tip", Generation: 1}}
				now := time.Now().Unix()
				a := Activity{Version: 1, ID: "receive/first-cold-row", Network: chain.Regtest, Kind: "receive", Chain: id, TxID: "history-tx", Variants: []string{"history-tx"}, Status: "confirmed", Confirmations: 200, BlockHash: "included-block", ObservedAt: now, Generation: 1, Observations: []ActivityObservation{{TxID: "history-tx", Status: "confirmed", Confirmations: 200, Height: 1, BlockHash: "included-block", ObservedAt: now, Generation: 1, Source: "fixture"}}}
				switch mode {
				case "old-block":
					backend.hashes[1] = "competing-block"
				case "unknown":
					a.Observations[0].Status = "unknown"
				case "no-height":
					a.Observations[0].Height = 0
				case "no-hash":
					a.Observations[0].BlockHash = ""
				case "wrong-tx":
					a.Observations[0].TxID = "other-variant"
				case "old-generation":
					a.Observations[0].Generation = 2
				case "future-time":
					a.Observations[0].ObservedAt = now + 60
				case "insufficient-depth":
					a.Observations[0].Height = 100
				case "unavailable":
					backend.failure = errors.New("offline fixture")
				case "generation-during-check":
					backend.afterRead = func(height uint32) {
						if height == 1 {
							backend.generation++
						}
					}
				case "fork-during-check":
					backend.afterRead = func(height uint32) {
						if height == 1 {
							backend.hashes[200] = "competing-tip"
						}
					}
				}
				e.s.Activities = map[string]Activity{a.ID: a}
				remaining := 64
				err := e.compactActivity(context.Background(), &remaining, map[chain.ID]bool{id: true})
				wantErr := mode == "unavailable" || mode == "generation-during-check" || mode == "fork-during-check"
				if (err != nil) != wantErr {
					t.Fatal("wrong verification diagnostic", err)
				}
				_, active := e.s.Activities[a.ID]
				if active == (mode == "current") {
					t.Fatal("wrong activity ownership after canonical verification", mode, active)
				}
				if mode == "current" {
					if e.s.Capacity.Anchors[id].Hash != "current-tip" || backend.reads != 2 {
						t.Fatal("archive did not bind its inclusion and final tip")
					}
				} else if e.s.Capacity != nil && e.s.Capacity.Anchors[id].Hash != "" {
					t.Fatal("unverified row acquired an archive anchor")
				}
				if mode == "old-block" {
					got := e.s.Activities[a.ID]
					if got.Status != "orphaned" || got.Confirmations != 0 || len(got.History) != 1 || got.History[0].Status != "confirmed" {
						t.Fatal("known contradiction lost current/audit distinction", got.Status, got.History)
					}
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					retained := false
					for _, outcome := range saved.Activities[a.ID].History {
						retained = retained || outcome.Status == "confirmed" && outcome.BlockHash == "included-block" && outcome.TxID == "history-tx"
					}
					if saved.Activities[a.ID].Status != "orphaned" || saved.Capacity.Archived.Kinds["activities"] != 0 || !retained {
						t.Fatal("saved checkpoint lost the active contradiction or prior audit outcome")
					}
				}
			})
		}
	}
}

func TestActivityArchiveVerificationAttemptsAreBounded(t *testing.T) {
	e, backends := receiveEngine(t)
	backend := &activityArchiveBackend{receiveBackend: backends[chain.BTC], generation: 1, failure: errors.New("offline fixture")}
	e.nodes[chain.BTC] = backend
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{chain.BTC: {Height: 200, Hash: "current-tip", Generation: 1}}
	e.s.Activities = map[string]Activity{}
	for i := range 1000 {
		id := string(rune(0x1000 + i))
		e.s.Activities[id] = Activity{Version: 1, ID: id, Kind: "receive", Network: chain.Regtest, Chain: chain.BTC, TxID: "tx", Status: "confirmed", Confirmations: 200, BlockHash: "included-block", Observations: []ActivityObservation{{TxID: "tx", Status: "confirmed", Confirmations: 200, Height: 1, BlockHash: "included-block", ObservedAt: time.Now().Unix(), Generation: 1}}}
	}
	remaining := 64
	if err := e.compactActivity(context.Background(), &remaining, map[chain.ID]bool{chain.BTC: true}); err == nil {
		t.Fatal("missing unavailable verification diagnostic")
	}
	if remaining != 0 || backend.reads != 64 || len(e.s.Activities) != 1000 {
		t.Fatal("unavailable history monopolized compaction", remaining, backend.reads)
	}
}
