package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

func activityBudgetFixture(keys ...string) (*Engine, []string) {
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{Activities: map[string]Activity{}}, nodes: map[chain.ID]chain.Backend{}}
	var ids []string
	for i, key := range keys {
		id := fmt.Sprintf("%064x", i+1)
		ids = append(ids, id)
		e.s.Activities[key] = Activity{ID: key, Kind: "swap_claim", Chain: chain.BTC, TxID: id, Variants: []string{id}, Status: "observed", Principal: 500000, Fee: 2000, FeeKnown: true, FeePayer: "wallet", Movement: true}
	}
	return e, ids
}

func budgetObservation(id string) chain.HistoryTransaction {
	return chain.HistoryTransaction{Transaction: chain.Transaction{TxID: id, Confirmations: 2, Height: 20, BlockHash: "current"}, Source: "healthy", Generation: 1}
}

func TestActivityBudgetEventuallyObservesEveryHealthyRow(t *testing.T) {
	keys := []string{"receive/a", "receive/b", "swap/z/claim"}
	e, ids := activityBudgetFixture(keys...)
	calls := map[string]int{}
	e.nodes[chain.BTC] = activityBackend{observe: func(ctx context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
		calls[id]++
		delay := 60 * time.Millisecond
		if id == ids[2] {
			delay = 120 * time.Millisecond
		}
		select {
		case <-time.After(delay):
			return budgetObservation(id), nil
		case <-ctx.Done():
			return chain.HistoryTransaction{Source: "healthy"}, ctx.Err()
		}
	}}
	for range 12 {
		e.observeActivityChain(context.Background(), chain.BTC)
	}
	for _, key := range keys {
		if a := e.s.Activities[key]; a.Status != "confirmed" {
			t.Fatalf("healthy row starved: key=%s status=%s calls=%v cursor=%s", key, a.Status, calls, e.activityCursors[chain.BTC])
		}
	}
}

func TestActivityBudgetSlowRowCannotMonopolizeHealthyRows(t *testing.T) {
	for _, slowKey := range []string{"a", "b"} {
		t.Run(slowKey, func(t *testing.T) {
			e, ids := activityBudgetFixture("a", "b", "c")
			slow := ids[0]
			if slowKey == "b" {
				slow = ids[1]
			}
			calls := map[string]int{}
			var deadline time.Time
			e.nodes[chain.BTC] = activityBackend{observe: func(ctx context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
				calls[id]++
				d, ok := ctx.Deadline()
				if !ok || time.Until(d) > 200*time.Millisecond {
					t.Fatal("observation exceeded the existing 200ms budget")
				}
				if deadline.IsZero() {
					deadline = d
				} else if !deadline.Equal(d) {
					t.Fatal("pass extended its deadline between rows")
				}
				if id == slow {
					<-ctx.Done()
					return chain.HistoryTransaction{Source: "unavailable"}, ctx.Err()
				}
				return budgetObservation(id), nil
			}}
			for range 5 {
				deadline = time.Time{}
				e.observeActivityChain(context.Background(), chain.BTC)
			}
			for _, key := range []string{"a", "b", "c"} {
				a := e.s.Activities[key]
				if key == slowKey {
					if a.Status != "unknown" {
						t.Fatal("unavailable full-slice observation retained a current outcome", a.Status)
					}
				} else if a.Status != "confirmed" || calls[a.TxID] == 0 {
					t.Fatalf("slow row blocked healthy row %s: status=%s calls=%v", key, a.Status, calls)
				}
			}
		})
	}
}

func TestActivityBudgetAttemptLimitAndVariantProgress(t *testing.T) {
	t.Run("eight attempts", func(t *testing.T) {
		keys := make([]string, 17)
		for i := range keys {
			keys[i] = fmt.Sprintf("row/%02d", i)
		}
		e, _ := activityBudgetFixture(keys...)
		calls := map[string]int{}
		e.nodes[chain.BTC] = activityBackend{observe: func(_ context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
			calls[id]++
			return budgetObservation(id), nil
		}}
		for pass := 0; pass < 3; pass++ {
			before := 0
			for _, n := range calls {
				before += n
			}
			e.observeActivityChain(context.Background(), chain.BTC)
			after := 0
			for _, n := range calls {
				after += n
			}
			if after-before != 8 {
				t.Fatal("pass changed the eight-attempt bound", after-before)
			}
		}
		if len(calls) != len(keys) {
			t.Fatal("bounded passes did not cover all rows", len(calls))
		}
	})
	t.Run("exact variant retry", func(t *testing.T) {
		e, ids := activityBudgetFixture("a", "b", "c")
		a := e.s.Activities["a"]
		a.Variants = ids
		e.s.Activities = map[string]Activity{"a": a}
		var calls []string
		e.nodes[chain.BTC] = activityBackend{observe: func(ctx context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
			calls = append(calls, id)
			delay := 60 * time.Millisecond
			if id == ids[2] {
				delay = 120 * time.Millisecond
			}
			select {
			case <-time.After(delay):
				if id == ids[2] {
					return budgetObservation(id), nil
				}
				return chain.HistoryTransaction{Source: "healthy"}, errors.New("superseded variant not found")
			case <-ctx.Done():
				return chain.HistoryTransaction{Source: "healthy"}, ctx.Err()
			}
		}}
		e.observeActivityChain(context.Background(), chain.BTC)
		if e.activityCursors[chain.BTC] != "a" || e.activityVariants[chain.BTC] != 2 {
			t.Fatal("cut-off variant lost its next full slice")
		}
		before := len(calls)
		e.observeActivityChain(context.Background(), chain.BTC)
		if calls[before] != ids[2] || e.s.Activities["a"].TxID != ids[2] || e.s.Activities["a"].Status != "confirmed" {
			t.Fatal("confirmed replacement variant starved or attributed to an earlier variant")
		}
	})
}

func TestActivityBudgetCutoffPreservesAgeButNotStaleAuthority(t *testing.T) {
	for _, mode := range []string{"fresh", "expired", "changed source", "contradicted", "unknown", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			e, ids := activityBudgetFixture("a", "b")
			old := ActivityObservation{Sequence: 1, TxID: ids[1], Status: "confirmed", Confirmations: 2, Height: 20, BlockHash: "current", ObservedAt: time.Now().Unix() - 60, Source: "healthy", Generation: 1}
			if mode == "expired" {
				old.ObservedAt -= 120
			}
			if mode != "unknown" {
				a := e.s.Activities["b"]
				a.Observations = []ActivityObservation{old}
				e.putActivity(a, true)
			}
			prior := e.s.Activities["b"]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			generation := uint64(1)
			if mode == "changed source" {
				generation = 2
			}
			e.nodes[chain.BTC] = activityGenerationBackend{generation: generation, activityBackend: activityBackend{observe: func(ctx context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
				if id == ids[0] {
					return budgetObservation(id), nil
				}
				if mode == "canceled" {
					cancel()
				}
				<-ctx.Done()
				return chain.HistoryTransaction{Source: "healthy", Generation: generation, PreviousBlockChanged: mode == "contradicted"}, ctx.Err()
			}}}
			e.observeActivityChain(ctx, chain.BTC)
			got := e.s.Activities["b"]
			if mode == "contradicted" {
				if got.Status != "orphaned" || got.Confirmations != 0 {
					t.Fatal("explicit block contradiction was hidden by local timeout", got.Status)
				}
			} else if !reflect.DeepEqual(got.Observations, prior.Observations) || got.ObservedAt != prior.ObservedAt {
				t.Fatal("cut-off read refreshed or replaced the previous observation")
			}
			// Exercise the same scalar projection used by the durable visitor;
			// this observer-budget fixture deliberately has no vault.
			projected := e.projectStrategyActivity(e.s.Activities["b"])

			if mode == "fresh" || mode == "canceled" {
				if projected.Status != "confirmed" || projected.ObservedAt != old.ObservedAt {
					t.Fatal("unchanged current proof lost its original provenance")
				}
			} else if projected.Status == "confirmed" || projected.Status == "confirming" {
				t.Fatal("cut-off read upgraded expired, unknown or contradicted evidence", mode, projected.Status)
			}
		})
	}
}

func TestActivityBudgetPartialWitnessSurvivesRestart(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			incoming, own := s.Short, s.Long
			if role == "maker" {
				incoming, own = s.Long, s.Short
			}
			key, _ := e.swapKey(incoming.Chain, s.ID)
			claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			s.SelfClaim, s.SelfClaims = contract.Hex(claim), []string{contract.Hex(claim)}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			record := chain.Transaction{TxID: claim.TxHash().String(), Hex: contract.Hex(claim)}
			claimID := "swap/" + s.ID + "/claim"
			claimActivity := e.s.Activities[claimID]
			if claimActivity.TxID != record.TxID {
				t.Fatal("fixture did not retain the signed claim")
			}
			firstID := strings.Repeat("a", 64)
			e.s.Activities = map[string]Activity{
				"receive/a": {ID: "receive/a", Chain: incoming.Chain, TxID: firstID, Variants: []string{firstID}},
				claimID:     claimActivity,
			}
			calls := 0
			e.nodes[incoming.Chain] = activityBackend{observe: func(ctx context.Context, id string, _ uint32, _ string) (chain.HistoryTransaction, error) {
				calls++
				if id == firstID {
					return budgetObservation(id), nil
				}
				<-ctx.Done()
				return chain.HistoryTransaction{Source: "admitted-source", Transaction: record}, ctx.Err()
			}}
			e.observeActivityChain(context.Background(), incoming.Chain)
			if calls != 2 || e.fatal != nil {
				t.Fatal("partial witness fixture did not reach the deferred second row", calls, e.fatal)
			}
			// No extra save: the valid witness must already have been persisted
			// before the observer's remainder-budget branch returned.
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			s = e.s.Swaps[s.ID]
			e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
			e.nodes[chain.BTC], e.nodes[chain.Blake] = &recoveryClockBackend{Backend: b}, &recoveryClockBackend{Backend: b}
			e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
			if !s.SecretObserved || !s.IncomingClaimSeen || e.checkRefundAcceleration(context.Background(), s, own) == nil {
				t.Fatal("partial observation lost the durable claim witness or allowed accelerated refund")
			}
		})
	}
}
