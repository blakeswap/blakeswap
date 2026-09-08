package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type settlementCheckpointBackend struct {
	chain.Backend
	hash string
}

func (b *settlementCheckpointBackend) BlockHash(context.Context, uint32) (string, error) {
	return b.hash, nil
}
func TestRestoredTowerNetworkGuardAfterKnownReorg(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, reorg := range []bool{false, true} {
			label := string(sell) + "/outage"
			if reorg {
				label = string(sell) + "/known-reorg"
			}
			t.Run(label, func(t *testing.T) {
				e, s, b, _ := isolatedFixtureSell(t, "maker", sell)
				tower := e.ownTower()
				s.Protection = &tower
				target := s.Short
				job, err := e.makeJob(s, target, "refund", nil, target.RefundHeight+protocol.RefundDelay(chain.Regtest))
				if err != nil {
					t.Fatal(err)
				}
				state := &TowerJob{Job: job, FundingSeen: true}
				e.s.Swaps = map[string]*Swap{}
				e.s.TowerJobs = map[string]*TowerJob{job.ID: state}
				markRestored(t, e)
				cb := &settlementCheckpointBackend{Backend: b, hash: "prior-history"}
				e.nodes[target.Chain] = cb
				if err = e.refreshRecoveryCheckpoint(context.Background(), target.Chain); err != nil {
					t.Fatal(err)
				}
				tx, err := contract.Parse(job.Templates[0])
				if err != nil {
					t.Fatal(err)
				}
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				all[target.Chain][chain.OutpointKey(target.TxID, target.Vout)] = chain.Observation{Tx: tx, TxID: tx.TxHash().String(), Confirmations: 6}
				if err = e.advanceTower(context.Background(), all); err != nil {
					t.Fatal(err)
				}
				e.reconcileRecovery(all, all)
				if state.Confirmed != 6 || e.CanChangeNetwork() != nil {
					t.Fatal("positive initial settlement failed", state, e.CanChangeNetwork())
				}
				if err = e.save(); err != nil {
					t.Fatal(err)
				}
				if reorg {
					cb.hash = "competing-history"
				}
				if err = e.refreshRecoveryCheckpoint(context.Background(), target.Chain); err != nil {
					t.Fatal(err)
				}
				delete(all, target.Chain) // Target catch-up is incomplete after the proven fork change.
				if err = e.advanceTower(context.Background(), all); err != nil {
					t.Fatal(err)
				}
				e.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, all)
				live, offline := settlementNetworkGuards(t, e)
				t.Logf("reorg=%v checkpoint=%s confirmed=%d state=%s liveGuard=%v storedGuard=%v targetError=%s", reorg, e.recoveryCheckpoints[target.Chain].Hash, state.Confirmed, e.s.Recovery.Status.State, live, offline, state.Error)
				if reorg && (live == nil || offline == nil) {
					t.Error("known reorg with unfinished target scan allowed disabling monitoring")
				}
				if !reorg && (live != nil || offline != nil) {
					t.Error("ordinary incomplete scan demoted established terminal outcome")
				}
				var saved State
				if _, err := e.vault.Load(&saved); err != nil {
					t.Fatal(err)
				}
				e.s = saved
				e.recoveryCheckpoints, e.recoveryReconciled = nil, nil
				if reorg && e.CanChangeNetwork() == nil {
					t.Fatal("restart erased settlement hold")
				}
				e.chainFresh[target.Chain.Other()] = false
				all = map[chain.ID]map[string]chain.Observation{target.Chain: {chain.OutpointKey(target.TxID, target.Vout): {Tx: tx, TxID: tx.TxHash().String(), Confirmations: 6}}}
				if err := e.refreshRecoveryCheckpoint(context.Background(), target.Chain); err != nil {
					t.Fatal(err)
				}
				if err := e.advanceTower(context.Background(), all); err != nil {
					t.Fatal(err)
				}
				e.reconcileRecovery(all, all)
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				live, offline = settlementNetworkGuards(t, e)
				if live != nil || offline != nil || e.s.Recovery.Status.State == "ready" {
					t.Fatal("positive target evidence did not independently clear hold during peer outage", live, offline, e.s.Recovery.Status)
				}
			})
		}
	}
}

func settlementNetworkGuards(t *testing.T, e *Engine) (error, error) {
	t.Helper()
	// No save here: a later failing Tick must not erase the checkpoint hold.
	live := e.CanChangeNetwork()
	dir := t.TempDir()
	password := filepath.Join(dir, "vault.password")
	if err := os.WriteFile(password, []byte("receive-test-password"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.vault.Backup(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	return live, CheckStoredNetwork(Config{DataDir: dir, PasswordFile: password})
}
func TestRestoredOwnerNetworkGuardAfterKnownReorg(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, refund := range []bool{false, true} {
			for _, reorg := range []bool{false, true} {
				label := role + "/completed"
				if refund {
					label = role + "/refunded"
				}
				if reorg {
					label += "/known-reorg"
				} else {
					label += "/outage"
				}
				t.Run(label, func(t *testing.T) {
					e, s, b, secret := isolatedFixture(t, role)
					markRestored(t, e)
					target := s.Short
					if role == "maker" {
						target = s.Long
					}
					cb := &settlementCheckpointBackend{Backend: b, hash: "prior-history"}
					e.nodes[target.Chain] = cb
					if err := e.refreshRecoveryCheckpoint(context.Background(), target.Chain); err != nil {
						t.Fatal(err)
					}
					all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
					for _, c := range []contract.HTLC{s.Long, s.Short} {
						all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, refund, secret)
					}
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
					e.reconcileRecovery(all, nil)
					if e.CanChangeNetwork() != nil {
						t.Fatal("positive settled owner remained active", s.Stage, e.CanChangeNetwork())
					}
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					if reorg {
						cb.hash = "competing-history"
					}
					if err := e.refreshRecoveryCheckpoint(context.Background(), target.Chain); err != nil {
						t.Fatal(err)
					}
					delete(all, target.Chain)
					e.chainFresh[target.Chain] = false // incomplete local spend scan marks the chain stale
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Log(err)
					}
					e.reconcileRecovery(all, nil)
					live, offline := settlementNetworkGuards(t, e)
					t.Logf("reorg=%v stage=%s recovery=%s liveGuard=%v storedGuard=%v", reorg, s.Stage, e.s.Recovery.Status.State, live, offline)
					if reorg && (live == nil || offline == nil) {
						t.Error("known reorg with incomplete owner scan allowed disabling monitoring")
					}
					if !reorg && (live != nil || offline != nil) {
						t.Error("ordinary scan uncertainty demoted established terminal history")
					}
					e.chainFresh[target.Chain] = true
					all[target.Chain] = map[string]chain.Observation{chain.OutpointKey(target.TxID, target.Vout): recoverySpend(t, e, s, target, refund, secret)}
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
					e.reconcileRecovery(all, nil)
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					live, offline = settlementNetworkGuards(t, e)
					if live != nil || offline != nil {
						t.Fatal("fresh owner settlement did not clear hold", live, offline)
					}
				})
			}
		}
	}
}
func TestRestoredSendNetworkGuardAfterKnownReorg(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		label := "unchanged-checkpoint"
		if reorg {
			label = "known-reorg"
		}
		t.Run(label, func(t *testing.T) {
			e, b, p := sendFixture(t)
			b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
			raw, _ := json.Marshal(p)
			if _, err := e.sendCoins(context.Background(), raw); err != nil {
				t.Fatal(err)
			}
			send := e.s.Sends[p.ID]
			markRestored(t, e)
			cb := &checkpointSendBackend{sendBackend: b, hash: "prior-history"}
			e.nodes[send.Chain] = cb
			if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
				t.Fatal(err)
			}
			b.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
				return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
			}
			e.advanceSends(context.Background())
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			e.reconcileRecovery(all, nil)
			if !e.recoverySends[send.ID] || e.CanChangeNetwork() != nil {
				t.Fatal("initial send not settled", send, e.CanChangeNetwork())
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			// This is also the second checkpoint read in Tick, after its only send pass.
			if reorg {
				cb.hash = "competing-history"
			}
			if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
				t.Fatal(err)
			}
			e.reconcileRecovery(all, nil)
			live, offline := settlementNetworkGuards(t, e)
			t.Logf("reorg=%v count=%d recoveryProof=%v recovery=%s liveGuard=%v storedGuard=%v", reorg, send.Confirmations, e.recoverySends[send.ID], e.s.Recovery.Status.State, live, offline)
			if reorg && (live == nil || offline == nil) {
				t.Error("positively invalidated send proof allowed disabling monitoring")
			}
			if !reorg && (live != nil || offline != nil) {
				t.Error("unchanged payment proof became an active obligation")
			}
			e.advanceSends(context.Background())
			e.reconcileRecovery(all, nil)
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			live, offline = settlementNetworkGuards(t, e)
			if live != nil || offline != nil {
				t.Fatal("fresh payment confirmation did not clear hold", live, offline)
			}
		})
	}
}

// Checkpoint failure modes differ: only positive canonical contradictions revoke
// terminal authority, and the revocation must survive a later read failure.
func TestRestoredSettlementCheckpointHoldsPersistBeforeLaterFailures(t *testing.T) {
	for _, change := range []string{"rewind", "ancestor-fork", "mixed-tip", "outage", "empty", "source-generation"} {
		t.Run(change, func(t *testing.T) {
			e, backend, _ := sendFixture(t)
			e.s.Sends = map[string]*WalletSend{"settled": {PublicSend: PublicSend{ID: "settled", Chain: chain.BTC, Confirmations: 6}}}
			markRestored(t, e)
			before, err := BackupFingerprint(e.s)
			if err != nil {
				t.Fatal(err)
			}
			b := &checkpointSendBackend{sendBackend: backend, hash: "test-canonical-tip"}
			e.nodes[chain.BTC] = b
			expectHold, expectError := true, false
			switch change {
			case "rewind":
				e.heights[chain.BTC]--
				b.hash = "earlier-tip"
			case "ancestor-fork":
				e.heights[chain.BTC]++
				b.hash = "competing-tip"
			case "mixed-tip":
				calls := 0
				b.onHash = func() string {
					calls++
					if calls == 1 {
						return "new-tip"
					}
					return "test-canonical-tip"
				}
				expectError = true
			case "outage":
				e.nodes[chain.BTC] = &failedSettlementCheckpoint{Backend: backend}
				expectHold, expectError = false, true
			case "empty":
				b.hash = ""
				expectHold, expectError = false, true
			case "source-generation":
				e.chainGeneration[chain.BTC]++
				expectHold = false
			}
			err = e.refreshRecoveryCheckpoint(context.Background(), chain.BTC)
			if (err != nil) != expectError {
				t.Fatal("checkpoint error", err)
			}
			live, offline := settlementNetworkGuards(t, e)
			if (live != nil) != expectHold || (offline != nil) != expectHold {
				t.Fatal("live/stored checkpoint authority", change, live, offline)
			}
			var saved State
			if _, err = e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if err = PrepareRecovery(&saved, time.Now().Unix(), false); err != nil {
				t.Fatal(err)
			}
			if (canChangeNetwork(saved) != nil) != expectHold {
				t.Fatal("re-export/import erased contradiction hold")
			}
			after, err := BackupFingerprint(e.s)
			if err != nil {
				t.Fatal(err)
			}
			if expectHold && before == after {
				t.Fatal("durable settlement hold omitted from backup freshness")
			}
		})
	}
}

type failedSettlementCheckpoint struct{ chain.Backend }

func (*failedSettlementCheckpoint) BlockHash(context.Context, uint32) (string, error) {
	return "", errors.New("checkpoint unavailable")
}

func TestRestoredSettlementInvalidationPreservesUnfundedFinalDecisions(t *testing.T) {
	for _, stage := range []string{"rejected", "expired before acceptance", "expired before funding", "expired before maker funding"} {
		t.Run(stage, func(t *testing.T) {
			role := "taker"
			if stage == "expired before maker funding" {
				role = "maker"
			}
			source, original := recoveryUnfundedFixture(t, role, stage, 0)
			sourceDigest := protocol.Digest(source.s)
			e := installUnfundedNetworkRecovery(t, source)
			s := e.s.Swaps[original.ID]
			if s == nil || !e.recoverySwapOwnInactive(s) {
				t.Fatal("import lacks the original current-format irreversible decision")
			}
			e.nodes[chain.BTC] = &settlementCheckpointBackend{Backend: e.nodes[chain.BTC], hash: "competing-history"}
			if err := e.refreshRecoveryCheckpoint(context.Background(), chain.BTC); err != nil {
				t.Fatal(err)
			}
			live, offline := settlementNetworkGuards(t, e)
			if live != nil || offline != nil || s.Stage != stage {
				t.Fatal("reorg revoked irreversible unfunded decision", live, offline, s.Stage)
			}
			if protocol.Digest(source.s) != sourceDigest {
				t.Fatal("import/reorg mutated the original wallet checkpoint")
			}
		})
	}
}

// Install an exported current checkpoint in a separate encrypted destination.
// In particular, do not overwrite a live engine's prior conservation snapshot
// to model an old import, or clear its already consumed authorization history.
func installUnfundedNetworkRecovery(t *testing.T, source *Engine) *Engine {
	t.Helper()
	raw, err := json.Marshal(source.s)
	if err != nil {
		t.Fatal(err)
	}
	var imported State
	if err := json.Unmarshal(raw, &imported); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRecovery(&imported, 100, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	password := []byte("receive-test-password")
	if err := storage.Initialize(path, password, imported); err != nil {
		t.Fatal(err)
	}
	v, saved, err := openCurrentStateVault(path, password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	e := &Engine{Config: source.Config, s: saved, vault: v, keys: source.keys, identity: source.identity,
		nodes: maps.Clone(source.nodes), watch: maps.Clone(source.watch), addresses: maps.Clone(source.addresses), scripts: maps.Clone(source.scripts),
		heights: maps.Clone(source.heights), clocks: maps.Clone(source.clocks), balances: map[chain.ID]int64{},
		chainFresh: map[chain.ID]bool{chain.BTC: true, chain.Blake: true}, chainObserved: maps.Clone(source.chainObserved), chainGeneration: maps.Clone(source.chainGeneration), chainErrors: map[chain.ID]string{},
		recoveryCheckpoints: map[chain.ID]recoveryCheckpoint{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if e.heights[id] == 0 {
			e.heights[id] = 200
		}
		e.recoveryCheckpoints[id] = recoveryCheckpoint{Height: e.heights[id], Hash: "test-canonical-tip", Generation: e.chainGeneration[id]}
	}
	// Establish this installation's original complete checkpoint before the
	// test changes chain evidence; future previous-checkpoint guards stay active.
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRestoredSettlementHoldDurabilityFailureStopsExecution(t *testing.T) {
	e, b, _ := sendFixture(t)
	e.s.Sends = map[string]*WalletSend{"settled": {PublicSend: PublicSend{ID: "settled", Chain: chain.BTC, Confirmations: 6}}}
	markRestored(t, e)
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	e.nodes[chain.BTC] = &checkpointSendBackend{sendBackend: b, hash: "competing-history"}
	if err := e.refreshRecoveryCheckpoint(context.Background(), chain.BTC); err == nil || e.fatal == nil || e.CanChangeNetwork() == nil {
		t.Fatal("failed hold persistence retained execution authority", err, e.fatal)
	}
	if err := e.tickProtocol(context.Background()); err != e.fatal {
		t.Fatal("tick ignored durability failure", err)
	}
}
