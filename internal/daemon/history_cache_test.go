package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
)

type cancelHistoryAtFence struct {
	chain.Backend
	reads  int
	cancel context.CancelFunc
}

func (s *cancelHistoryAtFence) Generation() uint64 {
	s.reads++
	if s.reads > 1 {
		s.cancel()
	}
	return 1
}

func TestHistoryCachePublishesEvictionOnlyAfterSuccessfulFinalFence(t *testing.T) {
	for _, method := range []string{"fills.list", "activity.list"} {
		for _, failure := range []string{"provider changed", "canceled at final fence"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				e, fill, _ := fillHistoryFixture(t, 3)
				activity := ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest"}
				var saved []Request
				var ids []string
				for i := 0; i < 4; i++ {
					var request Request
					var id string
					if i%2 == 0 {
						page := queryFills(t, e, fill)
						q := fill
						q.Revision, id = page.Revision, page.Revision
						request.Method, request.Params = "fills.list", mustHistoryJSON(t, q)
					} else {
						page := durableActivityQuery(t, e, activity)
						q := activity
						q.Snapshot, id = page.Snapshot, page.Snapshot
						request.Method, request.Params = "activity.list", mustHistoryJSON(t, q)
					}
					saved, ids = append(saved, request), append(ids, id)
				}
				if historyScratch(t, e) != 4 {
					t.Fatal("fixture lacks four published encrypted results")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if failure == "provider changed" {
					e.nodes[chain.BTC] = &reconnectingHistorySource{activityArchiveBackend: &activityArchiveBackend{generation: 1}}
				} else {
					e.nodes[chain.BTC] = &cancelHistoryAtFence{cancel: cancel}
				}
				fresh := Request{Method: method, Params: mustHistoryJSON(t, fill)}
				if method == "activity.list" {
					fresh.Params = mustHistoryJSON(t, activity)
				}
				if result, err := e.Command(ctx, fresh); err == nil || result != nil {
					t.Fatal("failed final fence published a fifth result", err)
				}
				delete(e.nodes, chain.BTC)
				if historyScratch(t, e) != 4 || len(e.activitySnapshots) != 4 {
					t.Fatal("rejected query destroyed an unexpired revision or leaked new files")
				}
				for _, prior := range saved {
					if _, err := e.Command(context.Background(), prior); err != nil {
						t.Fatal("rejected fifth query broke a prior mixed-kind page", err)
					}
				}
				// A bad later page is also an error, not cache replacement.
				bad := fill
				bad.Revision, bad.Offset = ids[0], 999
				if _, err := e.Command(context.Background(), Request{Method: "fills.list", Params: mustHistoryJSON(t, bad)}); err == nil {
					t.Fatal("out-of-range saved page succeeded")
				}
				if historyScratch(t, e) != 4 {
					t.Fatal("failed saved page changed cache ownership")
				}
				if _, err := e.Command(context.Background(), fresh); err != nil {
					t.Fatal(err)
				}
				if _, ok := e.activitySnapshots[ids[0]]; ok || len(e.activitySnapshots) != 4 || historyScratch(t, e) != 4 {
					t.Fatal("successful fifth query did not retire exactly the oldest revision")
				}
				for _, prior := range saved[1:] {
					if _, err := e.Command(context.Background(), prior); err != nil {
						t.Fatal("successful replacement evicted more than one result", err)
					}
				}
				e.nodes = nil
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				if historyScratch(t, e) != 0 {
					t.Fatal("Close retained mixed fill/activity result keys or files")
				}
			})
		}
	}
}

func mustHistoryJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
