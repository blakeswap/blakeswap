package daemon

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func durableActivityQuery(t *testing.T, e *Engine, q ActivityQuery) ActivityPage {
	t.Helper()
	raw, _ := json.Marshal(q)
	result, err := e.historyCommand(context.Background(), Request{Method: "activity.list", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return result.(ActivityPage)
}
func historyScratch(t *testing.T, e *Engine) int {
	t.Helper()
	entries, err := os.ReadDir(e.vault.PrivateDirectory())
	if err != nil {
		t.Fatal(err)
	}
	count, indexes := 0, 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".history-result-") {
			continue
		}
		// Custody uses the same encrypted scratch cleanup namespace, but owns
		// an index.db rather than a published history result. Count it separately
		// and still reject abandoned duplicate indexes.
		_, err := os.Stat(filepath.Join(e.vault.PrivateDirectory(), entry.Name(), "index.db"))
		if err == nil {
			indexes++
		} else if os.IsNotExist(err) {
			count++
		} else {
			t.Fatal(err)
		}
	}
	wantIndexes := 0
	if !e.activityClosed && e.fillValidation != nil && e.fillValidation.inputs != nil {
		wantIndexes = 1
	}
	if indexes != wantIndexes {
		t.Fatalf("custody scratch count %d, want %d", indexes, wantIndexes)
	}
	return count
}
func TestHistoryRowsFreezeAcrossColdMutationAndAgreeWithCSV(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "history"
	e.Config.Mode = "trader"
	e.s.Activities = map[string]Activity{}
	for i := 0; i < 401; i++ {
		id := fmt.Sprintf("receive/%04d", i)
		e.s.Activities[id] = Activity{Version: 1, ID: id, Network: chain.Regtest, Chain: chain.BTC, Kind: "receive", Status: "confirmed", CreatedAt: 100 + int64(i/3), Amount: int64(i + 1), Movement: true}
		if i%2 == 0 {
			if err := e.stageArchive("activities", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Limit: 37}
	first := durableActivityQuery(t, e, q)
	if first.Total != 401 || len(first.Records) != 37 || first.Records[0].ID != "receive/0400" {
		t.Fatal("first cold/hot merge page", first.Total, len(first.Records))
	}

	if len(e.activitySnapshots[first.Snapshot].Page.Records) != 0 || e.activitySnapshots[first.Snapshot].Rows == nil {
		t.Fatal("query cached all plaintext records")
	}
	// Both an old cold row and a new active row change after the independent
	// result has been completely authenticated. Frozen pages keep original data.
	if _, err := e.activateArchived("activities", "receive/0000"); err != nil {
		t.Fatal(err)
	}
	old := e.s.Activities["receive/0000"]
	old.Amount = 999
	e.s.Activities[old.ID] = old
	e.s.Activities["receive/latest"] = Activity{Version: 1, ID: "receive/latest", Network: chain.Regtest, Chain: chain.BTC, Kind: "receive", CreatedAt: 999}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	q.Snapshot = first.Snapshot
	var ids []string
	for {
		page := durableActivityQuery(t, e, q)
		raw, _ := json.Marshal(q)
		result, err := e.historyCommand(context.Background(), Request{Method: "activity.export", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(strings.NewReader(result.(ActivityExport).CSV)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if q.Cursor == 0 {
			rows = rows[1:]
		}
		if len(rows) != len(page.Records) {
			t.Fatal("CSV/page record count differs")
		}
		for i, a := range page.Records {
			if rows[i][0] != a.ID {
				t.Fatal("CSV/page ordering differs")
			}
			ids = append(ids, a.ID)
			if a.ID == old.ID && a.Amount != 1 {
				t.Fatal("live archive mutation changed frozen value")
			}
		}
		if page.NextCursor == 0 {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(ids) != 401 || ids[len(ids)-1] != "receive/0000" {
		t.Fatal("lost boundary rows", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatal("timestamp/ID ties changed", i)
		}
	}
	q.Snapshot = ""
	q.Cursor = 0
	if got := durableActivityQuery(t, e, q); got.Total != 402 || got.Records[0].ID != "receive/latest" {
		t.Fatal("fresh view omitted later changes")
	}
	for i := 0; i < 4; i++ {
		_ = durableActivityQuery(t, e, q)
	}
	if historyScratch(t, e) != 4 {
		t.Fatal("eviction leaked or lost result files", historyScratch(t, e))
	}
	q.Snapshot = first.Snapshot
	raw, _ := json.Marshal(q)
	if _, err := e.historyCommand(context.Background(), Request{Method: "activity.list", Params: raw}); err == nil {
		t.Fatal("evicted snapshot survived")
	}
	e.nodes = nil // receiveEngine embeds no real backend Close resource.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if historyScratch(t, e) != 0 {
		t.Fatal("Close left query files")
	}
}

func TestHistoryQueriesSkipUnrelatedColdKindsAndCanceledResults(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "history"
	e.Config.Mode = "trader"
	e.s.Seen = map[string]string{}
	// A valid authenticated category which cannot decode as the typed Seen map
	// proves no unrelated all-State materialization occurs in these query paths.
	record := storage.ArchiveRecord{Kind: "seen", ID: "unrelated", Data: json.RawMessage(`{"retained":"not a string"}`)}
	if err := e.archiveDelta(record, true); err != nil {
		t.Fatal(err)
	}
	e.archivePuts = map[string]storage.ArchiveRecord{archiveMoveKey(record.Kind, record.ID): record}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Limit: 1}
	if got := durableActivityQuery(t, e, q); got.Total != 0 {
		t.Fatal("unexpected activity")
	}
	raw, _ := json.Marshal(q)
	if _, err := e.historyCommand(context.Background(), Request{Method: "activity.export", Params: raw}); err != nil {
		t.Fatal(err)
	}
	market, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Owner: "mine", Status: "all", Limit: 1})
	if _, err := e.historyCommand(context.Background(), Request{Method: "market.list", Params: market}); err != nil {
		t.Fatal(err)
	}
	before := historyScratch(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.historyCommand(ctx, Request{Method: "activity.list", Params: raw}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled query succeeded", err)
	}
	if historyScratch(t, e) != before {
		t.Fatal("canceled query left partial files")
	}
	// Expiry must remove the cached key and files before a replacement is used.
	for id, snapshot := range e.activitySnapshots {
		snapshot.Page.Expires = time.Now().Unix() - 1
		e.activitySnapshots[id] = snapshot
	}
	_ = durableActivityQuery(t, e, q)
	if historyScratch(t, e) != 1 {
		t.Fatal("expired results survived")
	}
	e.nodes = nil // receiveEngine embeds no real backend Close resource.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryQueuedCancellationDoesNotWaitForAnotherVisitor(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "history"
	e.Config.Mode = "trader"
	e.s.Activities = map[string]Activity{"one": {Version: 1, ID: "one", Network: chain.Regtest, Chain: chain.BTC}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.VisitActivities(context.Background(), func(Activity, bool) error { close(started); <-release; return nil })
		done <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(ActivityQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Limit: 1})
	result := make(chan error, 1)
	go func() { _, err := e.historyCommand(ctx, Request{Method: "activity.list", Params: raw}); result <- err }()
	cancel()
	var failure string
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			failure = fmt.Sprint("wrong cancellation:", err)
		}
	case <-time.After(time.Second):
		failure = "queued cancellation waited for the visitor"
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if failure != "" {
		t.Fatal(failure)
	}
	if historyScratch(t, e) != 0 {
		t.Fatal("canceled queued query left result files")
	}
}

func TestHistoryMarketColdMergePreservesExactFiltersTiesAndRevision(t *testing.T) {
	e, _ := tradeFixture(t, "maker")
	now := time.Now().Unix()
	retired := []string{}
	for _, status := range []string{"filled", "cancelled"} {
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			o := marketOffer(t, e, true, id, 100000, 200000, status, now+3600)
			retired = append(retired, o.ID)
		}
	}
	marketOffer(t, e, true, chain.BTC, 100000, 200000, "open", now+3600)
	marketOffer(t, e, false, chain.BTC, 9_999_999_999, 10_000_000_000, "open", now+300)
	marketOffer(t, e, false, chain.Blake, 9_999_999_998, 9_999_999_999, "open", now+300)
	marketOffer(t, e, false, chain.Blake, 100000, 200000, "open", now+3600)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	type queryCase struct {
		q    MarketQuery
		want MarketPage
	}
	var cases []queryCase
	for _, owner := range []string{"all", "mine", "others"} {
		for _, sortBy := range []string{"rate", "size", "expiry"} {
			for _, desc := range []bool{false, true} {
				for _, status := range []string{"all", "open", "filled", "cancelled"} {
					q := MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Owner: owner, Status: status, Sort: sortBy, Descending: desc, Limit: 500}
					raw, _ := json.Marshal(q)
					want, err := e.marketPage(raw)
					if err != nil {
						t.Fatal(err)
					}
					cases = append(cases, queryCase{q, want})
				}
			}
		}
	}
	for _, id := range retired {
		for _, kind := range []string{"offers", "order_records", "offer_towers"} {
			if err := e.stageArchive(kind, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		q := c.q
		q.Limit = 2
		var merged []MarketOrder
		var first MarketPage
		for {
			raw, _ := json.Marshal(q)
			result, err := e.historyCommand(context.Background(), Request{Method: "market.list", Params: raw})
			if err != nil {
				t.Fatal(q, err)
			}
			page := result.(MarketPage)
			if q.Offset == 0 {
				first = page
			}
			merged = append(merged, page.Records...)
			if !page.More {
				break
			}
			q.Offset = page.NextOffset
			q.Revision = page.Revision
		}
		if len(merged) == 0 {
			merged = []MarketOrder{}
		}
		if first.Total != c.want.Total || first.Revision != c.want.Revision || !reflect.DeepEqual(merged, c.want.Records) {
			t.Fatalf("cold market changed exact result for %+v: total %d/%d revision %s/%s", c.q, first.Total, c.want.Total, first.Revision, c.want.Revision)
		}
	}
	if historyScratch(t, e) != 0 {
		t.Fatal("market query retained temporary files")
	}
}

type changingHistorySource struct {
	*activityArchiveBackend
	once   sync.Once
	reads  int
	change func()
}

func (b *changingHistorySource) Generation() uint64 {
	b.reads++
	// The first read captures source identity under Engine.mu. Change the
	// wallet only during later row projection, outside that capture lock.
	if b.reads > 1 {
		b.once.Do(b.change)
	}
	return b.activityArchiveBackend.Generation()
}
func TestHistoryLateContextFailureDisposesUnpublishedResult(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "history"
	e.Config.Mode = "trader"
	e.s.Activities = map[string]Activity{"one": {Version: 1, ID: "one", Network: chain.Regtest, Chain: chain.BTC, Generation: 1}}
	if err := e.vault.Save(e.s); err != nil {
		t.Fatal(err)
	}
	e.chainGeneration[chain.BTC] = 1
	e.nodes[chain.BTC] = &changingHistorySource{activityArchiveBackend: &activityArchiveBackend{generation: 1}, change: func() { e.mu.Lock(); e.Config.Name = "other"; e.mu.Unlock() }}
	raw, _ := json.Marshal(ActivityQuery{ExpectedWallet: "history", ExpectedNetwork: "regtest", Limit: 1})
	if result, err := e.historyCommand(context.Background(), Request{Method: "activity.list", Params: raw}); err == nil || result != nil {
		t.Fatal("late wallet change escaped", err)
	}
	if historyScratch(t, e) != 0 || len(e.activitySnapshots) != 0 {
		t.Fatal("unpublished result retained key/files after context failure")
	}
}
