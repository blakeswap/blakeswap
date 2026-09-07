package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestFrozenBackupPinsSemanticTokenAndCompleteArchive(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Seen = map[string]string{"sender:message": "exact-original"}
	if err := e.stageArchive("seen", "sender:message"); err != nil {
		t.Fatal(err)
	}
	view, release, err := e.FreezeBackup()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var pinned State
	stats, _, err := view.LoadState(&pinned)
	if err != nil {
		t.Fatal(err)
	}
	if err = pinned.ValidateArchiveCheckpoint(stats); err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(pinned)
	if token == "" || stats.Count != 1 {
		t.Fatal("pin omitted committed generation/archive")
	}
	// bbolt may wait for a read transaction before growing its mmap. A writer
	// therefore runs concurrently; the snapshot owner must release its pin
	// without acquiring Engine.mu before awaiting any writer.
	done := make(chan error, 1)
	go func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.s.ReceiveIndexes["btc"]++
		done <- e.save()
	}()
	count := 0
	if err = view.VisitArchive(context.Background(), func(record storage.ArchiveRecord) error {
		count++
		if string(record.Data) != "\"exact-original\"" {
			t.Fatal("pinned evidence changed")
		}
		return nil
	}); err != nil || count != 1 {
		t.Fatal("pin lost archive", err)
	}
	release()
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not resume after snapshot release")
	}
	if BackupSemanticToken(e.s) == token {
		t.Fatal("meaningful active change did not advance generation")
	}

	if err = e.RecordBackupSnapshot(strings.Repeat("a", 64), token, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if !e.Status().Backup.StateChanged {
		t.Fatal("late completed snapshot covered newer state")
	}
}

func TestInactiveArchiveTokenEstablishmentChecksActiveFingerprint(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Seen = map[string]string{"sender:message": "exact-original"}
	if err := e.stageArchive("seen", "sender:message"); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var state State
	if _, err := e.vault.Load(&state); err != nil {
		t.Fatal(err)
	}
	state.Capacity.SemanticToken = ""
	if changed, err := PrepareStoredBackupToken(&state); err != nil || !changed || BackupSemanticToken(state) == "" {
		t.Fatal("missing token not established", err)
	}
	original := BackupSemanticToken(state)
	if changed, err := PrepareStoredBackupToken(&state); err != nil || changed || BackupSemanticToken(state) != original {
		t.Fatal("unchanged state churned token", err)
	}
	state.Paused = !state.Paused
	if changed, err := PrepareStoredBackupToken(&state); err != nil || !changed || BackupSemanticToken(state) == original {
		t.Fatal("older writer's active change hidden", err)
	}
}
