package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestRestoredCompletedClaimRetriesAfterReorg(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, change := range []string{"evicted", "depth-lost", "target-outage", "incomplete-target", "peer-outage"} {
			t.Run(role+"/"+change, func(t *testing.T) {
				e, s, b, secret := isolatedFixture(t, role)
				markRestored(t, e)
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				for _, c := range []contract.HTLC{s.Long, s.Short} {
					all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, false, secret)
				}
				if err := e.advanceSwap(context.Background(), s, all); err != nil || s.Stage != "completed" {
					t.Fatal("confirmed fixture", err, s.Stage)
				}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				var reloaded State
				if _, err := e.vault.Load(&reloaded); err != nil {
					t.Fatal(err)
				}
				e.s = reloaded
				s = e.s.Swaps[s.ID]
				target, peer := s.Short, s.Long
				if role == "maker" {
					target, peer = s.Long, s.Short
				}
				point := chain.OutpointKey(target.TxID, target.Vout)
				expectRetry := change == "evicted" || change == "depth-lost"
				switch change {
				case "evicted":
					delete(all[target.Chain], point)
				case "depth-lost":
					o := all[target.Chain][point]
					o.Confirmations = 0
					all[target.Chain][point] = o
				case "target-outage":
					e.chainFresh[target.Chain] = false
					delete(all, target.Chain)
				case "incomplete-target":
					all[target.Chain] = nil
				case "peer-outage":
					e.chainFresh[peer.Chain] = false
					delete(all, peer.Chain)
				}
				calls := 0
				b.broadcast = func(raw string) (string, error) {
					calls++
					tx, err := contract.Parse(raw)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := contract.ExtractSecret(target, tx); !ok || tx.TxOut[0].Value != target.Amount-protocol.RescueFees[0] {
						t.Fatal("retry changed signed recovery authorization")
					}
					return tx.TxHash().String(), nil
				}
				for cycle := 0; cycle < 2; cycle++ {
					_ = e.advanceSwap(context.Background(), s, all)
				}
				if expectRetry {
					if calls != 1 || terminalSwapStage(s.Stage) {
						t.Fatal("fresh contradictory outcome failed to retry claim", calls, s.Stage)
					}
				} else if calls != 0 || s.Stage != "completed" {
					t.Fatal("unavailable observation invented active obligation", calls, s.Stage)
				}
				if !s.SecretObserved || !s.IncomingClaimSeen || e.recoveryOwnerPolicy(s, true) == nil {
					t.Fatal("reorg erased permanent witness/refund hold")
				}
			})
		}
	}
}

func TestRestoredConfirmedPaymentStopsBeforeUnavailableSibling(t *testing.T) {
	for _, confirmedIndex := range []int{0, 1} {
		for _, sourceChanges := range []bool{false, true} {
			name := map[int]string{0: "original", 1: "replacement"}[confirmedIndex]
			if sourceChanges {
				name += "/source-changes"
			}
			t.Run(name, func(t *testing.T) {
				e, b, p := sendFixture(t)
				p.MaxFee = 20000
				b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
				raw, _ := json.Marshal(p)
				sent, err := e.sendCoins(context.Background(), raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = e.bumpSend(context.Background(), BumpRequest{ID: p.ID, Kind: "send", Fee: 6500, ExpectedTxID: sent.TxID}); err != nil {
					t.Fatal(err)
				}
				send := e.s.Sends[p.ID]
				markRestored(t, e)
				confirmed := send.History[confirmedIndex]
				calls := 0
				b.transaction = func(ctx context.Context, id string) (chain.Transaction, error) {
					calls++
					if id == confirmed.TxID {
						if sourceChanges {
							e.chainFresh[send.Chain] = false
						}
						return chain.Transaction{TxID: id, Hex: confirmed.Raw, Height: 190, Confirmations: 311}, nil
					}
					<-ctx.Done()
					return chain.Transaction{}, ctx.Err()
				}
				for cycle := 0; cycle < 4; cycle++ {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
					e.advanceSends(ctx)
					cancel()
					e.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil)
					if e.recoveryTradingReady() == nil {
						break
					}
				}
				if sourceChanges {
					if e.recoverySends[send.ID] || e.recoveryTradingReady() == nil {
						t.Fatal("source change accepted stale payment proof")
					}
					return
				}
				if !e.recoverySends[send.ID] || e.recoveryTradingReady() != nil || send.TxID != confirmed.TxID || calls > 2 {
					t.Fatal("confirmed payment blocked by replaced sibling", send.ObserveCursor, calls, e.s.Recovery.Status)
				}
				e.clearRecoveryPayments(send.Chain)
				if e.recoveryTradingReady() == nil {
					t.Fatal("canonical invalidation retained payment readiness")
				}
			})
		}
	}
}

func TestRestoredTowerRefundObservesOutcomeWithoutPublishing(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, s, b, _ := isolatedFixtureSell(t, "maker", sell)
			tower := e.ownTower()
			s.Protection = &tower
			target := s.Short
			job, err := e.makeJob(s, target, "refund", nil, target.RefundHeight+protocol.RefundDelay(chain.Regtest))
			if err != nil {
				t.Fatal(err)
			}
			if err = job.Validate(e.ownTower().Scripts, job.BPS); err != nil {
				t.Fatal(err)
			}
			state := &TowerJob{Job: job, FundingSeen: true}
			e.s.Swaps = map[string]*Swap{}
			e.s.TowerJobs = map[string]*TowerJob{job.ID: state}
			markRestored(t, e)
			tx, err := contract.Parse(job.Templates[0])
			if err != nil {
				t.Fatal(err)
			}
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			all[target.Chain][chain.OutpointKey(target.TxID, target.Vout)] = chain.Observation{Tx: tx, TxID: tx.TxHash().String(), Confirmations: 6}
			b.broadcast = func(string) (string, error) { t.Fatal("restored tower refund published"); return "", nil }
			e.nodes[target.Chain] = b
			for _, peerFresh := range []bool{false, true} {
				e.chainFresh[s.Long.Chain] = peerFresh
				if err = e.advanceTower(context.Background(), all); err != nil {
					t.Fatal(err)
				}
				if state.Confirmed != 6 || state.Broadcast != tx.TxHash().String() || e.CanChangeNetwork() != nil {
					t.Fatal("confirmed restored job stayed active", state.Confirmed, state.Error, e.CanChangeNetwork())
				}
			}
			e.reconcileRecovery(all, all)
			if err = e.recoveryTradingReady(); err != nil {
				t.Fatal("positively settled original job did not reconcile", err)
			}
			e.syncActivity()
			activity, found := e.s.Activities[activityID("tower", job.ID)]
			if !found || activity.TxID != tx.TxHash().String() || activity.LocalStatus != "confirmed" {
				t.Fatal("restored confirmed bounty absent from history", activity)
			}
			delete(all[target.Chain], chain.OutpointKey(target.TxID, target.Vout))
			if err = e.advanceTower(context.Background(), all); err != nil {
				t.Fatal(err)
			}
			e.reconcileRecovery(all, all)
			if state.Confirmed != 0 || state.LastAttempt != 0 || state.Error == "" || e.CanChangeNetwork() == nil || e.recoveryTradingReady() == nil {
				t.Fatal("reorg failed to reopen held tower obligation", state, e.s.Recovery.Status)
			}
		})
	}
}

func TestRestoredRefundedHistorySurvivesOutageButReopensOnReorg(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, reorg := range []bool{false, true} {
			t.Run(role+"/reorg="+map[bool]string{true: "yes", false: "no"}[reorg], func(t *testing.T) {
				e, s, b, _ := isolatedFixture(t, role)
				markRestored(t, e)
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				for _, c := range []contract.HTLC{s.Long, s.Short} {
					all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, true, nil)
				}
				if err := e.advanceSwap(context.Background(), s, all); err != nil || s.Stage != "refunded" {
					t.Fatal("refunded fixture", err, s.Stage)
				}
				e.chainFresh[s.Long.Chain] = false
				delete(all, s.Long.Chain)
				if reorg {
					delete(all[s.Short.Chain], chain.OutpointKey(s.Short.TxID, s.Short.Vout))
				}
				b.broadcast = func(string) (string, error) { t.Fatal("refund published during peer outage"); return "", nil }
				_ = e.advanceSwap(context.Background(), s, all)
				if !reorg && s.Stage != "refunded" {
					t.Fatal("outage invented an active restored obligation", s.Stage)
				}
				if reorg && terminalSwapStage(s.Stage) {
					t.Fatal("fresh removed outcome stayed terminal", s.Stage)
				}
			})
		}
	}
}

func TestRestoredObservedClaimRequiresCompleteTargetSnapshot(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			markRestored(t, e)
			target, observe := s.Short, s.Long
			if role == "maker" {
				target, observe = s.Long, s.Short
			}
			all := map[chain.ID]map[string]chain.Observation{
				target.Chain:  nil,
				observe.Chain: {chain.OutpointKey(observe.TxID, observe.Vout): recoverySpend(t, e, s, observe, false, secret)},
			}
			b.broadcast = func(string) (string, error) { t.Fatal("incomplete target authorized publication"); return "", nil }
			if err := e.advanceSwap(context.Background(), s, all); err == nil || !s.SecretObserved || s.SelfClaim != "" {
				t.Fatal("incomplete target map lost witness or authorized claim", err, s.Stage)
			}
		})
	}
}

func TestStrategyRestoredPositiveSettlementReleasesExposure(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, refund := range []bool{false, true} {
			name := role + "/claim"
			if refund {
				name = role + "/refund"
			}
			t.Run(name, func(t *testing.T) {
				e, s, _, secret := isolatedFixture(t, role)
				markRestored(t, e)
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				for _, c := range []contract.HTLC{s.Long, s.Short} {
					all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, refund, secret)
				}
				for cycle := 0; cycle < 2; cycle++ {
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
					e.reconcileRecovery(all, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}})
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
				}
				if !strategySettled(s, e.Config.Network.Confirmations()) || e.s.Recovery.Status.State != "ready" {
					t.Fatalf("fixture failed: stage=%s recovery=%+v", s.Stage, e.s.Recovery.Status)
				}
				if err := e.recoveryTradingReady(); err != nil {
					t.Fatal("recovery gate", err)
				}
				u, _, n := e.strategyUsage(StrategyConfig{}, "")
				if n != 0 || u[chain.BTC].Exposure != 0 || u[chain.Blake].Exposure != 0 {
					t.Fatalf("fresh dual-spend %s remains charged as active strategy exposure after recovery ready: count=%d BTC=%d Blake=%d verified=%v", s.Stage, n, u[chain.BTC].Exposure, u[chain.Blake].Exposure, e.strategyVerifiedSwaps[s.ID])
				}
			})
		}
	}
}
