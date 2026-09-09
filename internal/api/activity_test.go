package api

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

func TestActivityAPIExactAmountsScopeAndProvenance(t *testing.T) {
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		var q daemon.ActivityQuery
		if err := json.Unmarshal(r.Params, &q); err != nil {
			t.Fatal(err)
		}
		if q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" || q.Chain != chain.Blake || q.Snapshot != "snapshot" || q.Cursor != 1 || q.Limit != 2 {
			t.Fatal(q)
		}
		if r.Method == "activity.export" {
			return daemon.ActivityExport{Snapshot: q.Snapshot, Total: 3, CSV: "id,amount_sats\nold,9007199254740993\n"}, nil
		}
		if r.Method != "activity.list" {
			t.Fatal(r.Method)
		}
		return daemon.ActivityPage{Snapshot: q.Snapshot, Total: 3, Records: []daemon.Activity{{Version: 1, ID: "old", GroupID: "send/one", Wallet: "alice", Network: chain.Regtest, Kind: "receive", Chain: chain.Blake, Direction: "internal", Classification: "change", Amount: 9007199254740993, Principal: 9007199254740993, FeeKnown: false, CreatedSource: "unknown", RecordedAt: 123, Source: "opaque", Generation: 9, RelatedIDs: []string{"send/one"}, History: []daemon.ActivityOutcome{{Status: "confirmed", Amount: 9007199254740993}}}}, Index: map[chain.ID]daemon.ActivityIndex{chain.Blake: {CompletedPass: 123, Source: "opaque", Generation: 9}}}, nil
	}}
	query := &pb.ActivityQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", Chain: "blake", Snapshot: "snapshot", Cursor: 1, Limit: 2}
	page, err := service.ListActivity(context.Background(), query)
	if err != nil || len(page.GetRecords()) != 1 {
		t.Fatal(page, err)
	}
	a := page.Records[0]
	if a.Amount != 9007199254740993 || a.GroupId != "send/one" || a.Classification != "change" || a.Movement || a.CreatedAt != 0 || a.CreatedSource != "unknown" || a.Generation != 9 || a.History[0].Amount != a.Amount {
		t.Fatal(a)
	}
	exported, err := service.ExportActivity(context.Background(), query)
	if err != nil || exported.Snapshot != "snapshot" || exported.Csv != "id,amount_sats\nold,9007199254740993\n" {
		t.Fatal(exported, err)
	}
}
