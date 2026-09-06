package daemon

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
)

func activityQueryFixture() *Engine {
	return &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{Activities: map[string]Activity{
		"old": {Version: 1, ID: "old", Wallet: "alice", Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Amount: 100001, Movement: true, CreatedAt: 100, CreatedSource: "local", Status: "confirmed"},
		"new": {Version: 1, ID: "new", Wallet: "alice", Network: chain.Regtest, Kind: "send", Chain: chain.Blake, Amount: 200003, Movement: true, CreatedAt: 200, CreatedSource: "local", Status: "mempool"},
	}, ActivityRevision: 2}}
}
func queryActivity(t *testing.T, e *Engine, q ActivityQuery) ActivityPage {
	t.Helper()
	raw, _ := json.Marshal(q)
	page, err := e.activityPage(raw)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
func TestActivityPaginationFreezesUpdatesAndBindsFilters(t *testing.T) {
	e := activityQueryFixture()
	q := ActivityQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", Limit: 1}
	first := queryActivity(t, e, q)
	if first.Total != 2 || first.Records[0].ID != "new" || first.NextCursor != 1 {
		t.Fatal(first)
	}
	e.s.Activities["latest"] = Activity{Version: 1, ID: "latest", CreatedAt: 300}
	old := e.s.Activities["old"]
	old.Status = "orphaned"
	e.s.Activities["old"] = old
	e.s.ActivityRevision++
	q.Snapshot = first.Snapshot
	q.Cursor = first.NextCursor
	second := queryActivity(t, e, q)
	if second.Total != 2 || second.Records[0].ID != "old" || second.Records[0].Status != "confirmed" || second.NextCursor != 0 {
		t.Fatal("live changes changed a frozen page", second)
	}
	q.Chain = chain.BTC
	raw, _ := json.Marshal(q)
	if _, err := e.activityPage(raw); err == nil {
		t.Fatal("snapshot reused with changed scope")
	}
	q.Snapshot = ""
	q.Cursor = 0
	filtered := queryActivity(t, e, q)
	if filtered.Total != 1 || filtered.Records[0].Status != "orphaned" {
		t.Fatal(filtered)
	}
	q.ExpectedWallet = "bob"
	raw, _ = json.Marshal(q)
	if _, err := e.activityPage(raw); err == nil {
		t.Fatal("snapshot crossed wallet")
	}
}
func TestActivityCSVExactSafeAndPrivate(t *testing.T) {
	e := activityQueryFixture()
	e.s.Mnemonic = "private recovery words"
	e.s.Swaps = map[string]*Swap{"private": {Secret: "private-preimage", LongFunding: "private-signed-raw"}}
	row := e.s.Activities["old"]
	row.Amount = 9007199254740993
	row.Fee = -123
	row.FeeKnown = true
	row.Label = "  =HYPERLINK(\"unsafe\")\nline, two"
	row.CreatedAt = 0
	row.CreatedSource = "unknown"
	row.RecordedAt = 500
	e.s.Activities["old"] = row
	q := ActivityQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", Chain: chain.BTC}
	raw, _ := json.Marshal(q)
	result, err := e.exportActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(result.CSV)).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatal(records, err)
	}
	values := map[string]string{}
	for i, name := range records[0] {
		values[name] = records[1][i]
	}
	if values["amount_sats"] != "9007199254740993" || values["fee_sats"] != "-123" || values["label"] != "'"+row.Label || values["created_at_utc"] != "" || values["created_time_source"] != "unknown" {
		t.Fatal(values)
	}
	for _, secret := range []string{e.s.Mnemonic, "private-preimage", "private-signed-raw"} {
		if strings.Contains(result.CSV, secret) {
			t.Fatal("private recovery field exported")
		}
	}
	for _, prefix := range []string{"=formula", "+formula", "-formula", "@formula", "\tformula", "\rformula", " \n@formula"} {
		if !strings.HasPrefix(csvText(prefix), "'") {
			t.Fatal("formula prefix not escaped", prefix)
		}
	}
}

func TestActivitySnapshotEvictionIsOldestFirst(t *testing.T) {
	e := activityQueryFixture()
	q := ActivityQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest"}
	var snapshots []string
	for i := 0; i < 5; i++ {
		snapshots = append(snapshots, queryActivity(t, e, q).Snapshot)
	}
	q.Snapshot = snapshots[0]
	raw, _ := json.Marshal(q)
	if _, err := e.activityPage(raw); err == nil {
		t.Fatal("oldest snapshot survived bounded FIFO eviction")
	}
	for _, id := range snapshots[1:] {
		q.Snapshot = id
		if got := queryActivity(t, e, q); got.Total != 2 {
			t.Fatal("newer snapshot was evicted", got)
		}
	}
}

func TestActivityLifecycleHistoryRetainsLocalTimeAndDoesNotGrowOnPolling(t *testing.T) {
	e := activityQueryFixture()
	order := Activity{ID: "order/id", Kind: "order", Status: "open", LocalStatus: "open"}
	e.putActivity(order, true)
	old := e.s.Activities[order.ID]
	if old.CreatedAt != 0 || old.CreatedSource != "unknown" {
		t.Fatal("migration invented creation time", old)
	}
	order.Status, order.LocalStatus = "cancelled", "cancelled"
	e.putActivity(order, false)
	for i := 0; i < 10; i++ {
		e.putActivity(order, false)
	}
	got := e.s.Activities[order.ID]
	if len(got.History) != 1 || got.History[0].Status != "open" || got.History[0].ObservedAt != old.UpdatedAt || got.History[0].Source != "local_state" {
		t.Fatal("local transition lost its observed time or polling duplicated it", got)
	}
}
