package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
)

func TestActivityVisitorIncludesColdRowsWithoutReactivationAndCancels(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Activities = map[string]Activity{
		"receive/cold": {Version: 1, ID: "receive/cold", Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Amount: 3456, Status: "confirmed"},
		"receive/hot":  {Version: 1, ID: "receive/hot", Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Amount: 1234, Status: "confirmed"},
	}
	if err := e.stageArchive("activities", "receive/cold"); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(e.s)
	seen := map[string]bool{}
	_, err := e.VisitActivities(context.Background(), func(row Activity, cold bool) error { seen[row.ID] = cold; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen["receive/cold"] || seen["receive/hot"] {
		t.Fatal("incomplete visitor", seen)
	}
	if _, active := e.s.Activities["receive/cold"]; active || BackupSemanticToken(e.s) != token {
		t.Fatal("report reactivated or changed recovery data")
	}
	stopped := errors.New("stop report")
	if _, err := e.VisitActivities(context.Background(), func(Activity, bool) error { return stopped }); !errors.Is(err, stopped) {
		t.Fatal("callback cancellation lost", err)
	}
	// A completed canceled reader must release its pinned transaction so normal
	// persistence remains available and shutdown cannot wait on a leaked reader.
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
}
