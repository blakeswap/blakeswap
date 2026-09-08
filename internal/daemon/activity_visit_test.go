package daemon

import (
	"context"
	"errors"
	"runtime"
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
	blocked := false
	writeFinished := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-writeDone:
			if err != nil {
				close(release)
				<-readDone
				t.Fatal(err)
			}
			writeFinished = true
		default:
		}
		if writeFinished {
			break
		}
		stacks := make([]byte, 1<<20)
		n := runtime.Stack(stacks, true)
		if strings.Contains(string(stacks[:n]), "go.etcd.io/bbolt.(*DB).mmap") {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
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
	if blocked {
		t.Fatal("settlement Save held Engine.mu while bbolt mmap waited for VisitActivities callback")
	}
}
