package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestHistoryVisitorDoesNotPinSettlementWriter(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Activities = map[string]Activity{"receive/hot": {Version: 1, ID: "receive/hot", Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Amount: 1000}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		_, err := e.VisitActivities(context.Background(), func(Activity, bool) error { close(entered); <-release; return nil })
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never reached callback")
	}
	writeDone := make(chan error, 1)
	go func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.s.ActivityError = strings.Repeat("x", 8<<20)
		writeDone <- e.save()
	}()
	// Require completed persistence before releasing the consumer. A runnable
	// mmap/dereference frame is normal file growth, not evidence of a read lock.
	writeFinished := false
	var writeErr error
	select {
	case writeErr = <-writeDone:
		writeFinished = true
	case <-time.After(5 * time.Second):
	}
	// Always release and join before asserting: this negative probe owns every
	// handle and must not strand a real writer behind its diagnostic callback.
	close(release)
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader failed to release")
	}
	if !writeFinished {
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer failed to resume after reader released")
		}
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if !writeFinished {
		t.Fatal("settlement Save did not complete while VisitActivities callback remained blocked")
	}
}
