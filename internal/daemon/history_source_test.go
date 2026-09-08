package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
)

func offlineHistoryEngine(t *testing.T, generation uint64) (*Engine, *activityArchiveBackend) {
	t.Helper()
	e, _ := receiveEngine(t)
	e.Config.Name, e.Config.Mode = "history", "trader"
	e.s.Activities = map[string]Activity{}
	for _, id := range []string{"cold", "hot"} {
		e.s.Activities[id] = Activity{Version: 1, ID: id, Network: chain.Regtest, Chain: chain.BTC, Kind: "receive", Status: "confirmed", Generation: 1, CreatedAt: 1}
	}
	if err := e.stageArchive("activities", "cold"); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	backend := &activityArchiveBackend{generation: generation}
	e.nodes[chain.BTC] = backend
	e.chainGeneration[chain.BTC], e.chainFresh[chain.BTC] = 0, false
	return e, backend
}

func TestHistoryStableOfflineSourceRetainsQueriesAndFrozenPages(t *testing.T) {
	for _, generation := range []uint64{0, 2} {
		e, backend := offlineHistoryEngine(t, generation)
		q := ActivityQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Limit: 1}
		first := durableActivityQuery(t, e, q)
		if first.Total != 2 || first.Records[0].Status != "unknown" || len(first.Records[0].History) == 0 || first.Records[0].History[0].Status != "confirmed" {
			t.Fatal("offline history lost evidence or asserted a current observation", first)
		}
		for _, method := range []string{"activity.export", "market.list"} {
			raw, _ := json.Marshal(q)
			if method == "market.list" {
				raw, _ = json.Marshal(MarketQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Owner: "mine", Status: "all", Limit: 1})
			}
			if _, err := e.historyCommand(context.Background(), Request{Method: method, Params: raw}); err != nil {
				t.Fatal("stable offline query failed", method, generation, err)
			}
		}
		// A completed result owns its original projection. Reconnection between
		// requests does not rewrite the frozen page or prevent reading it.
		backend.generation = 1
		e.chainGeneration[chain.BTC], e.chainFresh[chain.BTC] = 1, true
		q.Snapshot, q.Cursor = first.Snapshot, first.NextCursor
		last := durableActivityQuery(t, e, q)
		if len(last.Records) != 1 || last.Records[0].Status != "unknown" || last.Snapshot != first.Snapshot {
			t.Fatal("reconnection rewrote the frozen offline result", last)
		}
	}
}

type reconnectingHistorySource struct {
	*activityArchiveBackend
	reads int
}

func (b *reconnectingHistorySource) Generation() uint64 {
	b.reads++
	if b.reads > 1 {
		return b.generation + 1
	}
	return b.generation
}

func TestHistorySourceReconnectionDiscardsUnpublishedResults(t *testing.T) {
	for _, method := range []string{"activity.list", "activity.export", "market.list"} {
		t.Run(method, func(t *testing.T) {
			e, backend := offlineHistoryEngine(t, 0)
			e.nodes[chain.BTC] = &reconnectingHistorySource{activityArchiveBackend: backend}
			raw, _ := json.Marshal(ActivityQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Limit: 1})
			if method == "market.list" {
				raw, _ = json.Marshal(MarketQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Owner: "mine", Status: "all", Limit: 1})
			}
			result, err := e.historyCommand(context.Background(), Request{Method: method, Params: raw})
			if err == nil || !strings.Contains(err.Error(), "source changed") || result != nil {
				t.Fatal("late source selection escaped the completion fence", result, err)
			}
			if historyScratch(t, e) != 0 || len(e.activitySnapshots) != 0 {
				t.Fatal("rejected result retained its private key or files")
			}
		})
	}
}

func TestActivityVisitorSeparatesStableOfflineSourceFromReconnection(t *testing.T) {
	for _, change := range []bool{false, true} {
		e, backend := offlineHistoryEngine(t, 0)
		seen := map[string]bool{}
		_, err := e.VisitActivities(context.Background(), func(row Activity, cold bool) error {
			if row.Status != "unknown" || row.ArchiveVerified {
				t.Fatal("offline scan invented a current proof", row)
			}
			seen[row.ID] = cold
			if change && len(seen) == 2 {
				backend.generation = 1
			}
			return nil
		})
		if change {
			if err == nil || !strings.Contains(err.Error(), "source changed") {
				t.Fatal("visitor accepted a source change during its last callback", err)
			}
		} else if err != nil {
			t.Fatal("stable offline visitor failed", err)
		}
		if len(seen) != 2 || !seen["cold"] || seen["hot"] {
			t.Fatal("visitor lost hot/cold coverage", seen)
		}
	}
}
