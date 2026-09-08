package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func captureFixture(t *testing.T, count int) (*Vault, []ArchiveRecord) {
	t.Helper()
	v, err := Open(filepath.Join(t.TempDir(), "state.db"), []byte("capture private fixture password"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	records := make([]ArchiveRecord, count)
	for i := range records {
		records[i] = ArchiveRecord{Kind: "receipt", ID: fmt.Sprint(i), Data: json.RawMessage(`{"evidence":"retained"}`)}
	}
	if _, err := v.CommitArchive(map[string]string{"checkpoint": "original"}, ArchiveBatch{Put: records}, 0); err != nil {
		t.Fatal(err)
	}
	return v, records
}
func fallbackCapture(t *testing.T, v *Vault) *PageSnapshot {
	t.Helper()
	cloned, s, err := v.CaptureArchive("", false)
	if err != nil || cloned || s == nil {
		t.Fatal(cloned, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func TestArchiveCaptureFallbackAllowsWriterGrowthAndFrozenState(t *testing.T) {
	v, _ := captureFixture(t, 130)
	s := fallbackCapture(t, v)
	count := 0
	err := s.VisitArchive(context.Background(), func(record ArchiveRecord) error {
		count++
		if count == 1 {
			// A callback may write enough to force bbolt mmap growth. It must run
			// outside the source read transaction, while retaining the old state.
			done := make(chan error, 1)
			go func() { done <- v.Save(map[string]string{"checkpoint": string(bytes.Repeat([]byte("a"), 8<<20))}) }()
			select {
			case err := <-done:
				if err != nil {
					return err
				}
			case <-time.After(5 * time.Second):
				return errors.New("source writer blocked behind snapshot callback")
			}
		}
		return nil
	})
	if err != nil || count != 130 {
		t.Fatal(count, err)
	}
	var active map[string]string
	if _, _, err = s.LoadState(&active); err != nil || active["checkpoint"] != "original" {
		t.Fatal(active, err)
	}
	// The fence is complete. Later changes are legal and don't change copied data.
	if _, err = v.CommitArchive(struct{}{}, ArchiveBatch{Delete: []ArchiveKey{{Kind: "receipt", ID: "0"}}}, 0); err != nil {
		t.Fatal(err)
	}
}
func TestArchiveCaptureGenerationAndEOFFences(t *testing.T) {
	for _, n := range []int{1, 130} {
		for _, mode := range []string{"new", "aba", "cancel"} {
			t.Run(fmt.Sprintf("%d/%s", n, mode), func(t *testing.T) {
				v, records := captureFixture(t, n)
				s := fallbackCapture(t, v)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				first := true
				err := s.VisitArchive(ctx, func(ArchiveRecord) error {
					if !first {
						return nil
					}
					first = false
					if mode == "cancel" {
						cancel()
						return nil
					}
					if mode == "aba" {
						if _, err := v.CommitArchive(struct{}{}, ArchiveBatch{Delete: []ArchiveKey{{Kind: records[0].Kind, ID: records[0].ID}}}, 0); err != nil {
							return err
						}
						_, err := v.CommitArchive(struct{}{}, ArchiveBatch{Put: []ArchiveRecord{records[0]}}, 0)
						return err
					}
					_, err := v.CommitArchive(struct{}{}, ArchiveBatch{Put: []ArchiveRecord{{Kind: "receipt", ID: "new", Data: json.RawMessage(`{}`)}}}, 0)
					return err
				})
				expected := ErrArchiveChanged
				if mode == "cancel" {
					expected = context.Canceled
				}
				if !errors.Is(err, expected) {
					t.Fatalf("got %v, want %v", err, expected)
				}
			})
		}
	}
}
func TestArchiveCaptureIdempotentAndCorruptGeneration(t *testing.T) {
	v, records := captureFixture(t, 2)
	s := fallbackCapture(t, v)
	if _, err := v.CommitArchive(struct{}{}, ArchiveBatch{Put: records}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitArchive(context.Background(), func(ArchiveRecord) error { return nil }); err != nil {
		t.Fatal("idempotent archive invalidated snapshot", err)
	}
	if err := v.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Put(archiveGenerationKey, []byte("corrupt")) }); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitArchive(context.Background(), func(ArchiveRecord) error { return nil }); err == nil {
		t.Fatal("corrupt generation accepted")
	}
	if _, _, err := v.CaptureArchive("", false); err == nil {
		t.Fatal("corrupt generation replaced during capture")
	}
}
func TestArchiveCaptureCloneIndependentCommittedView(t *testing.T) {
	v, _ := captureFixture(t, 130)
	path := filepath.Join(t.TempDir(), "clone.db")
	cloned, page, err := v.CaptureArchive(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !cloned {
		_ = page.Close()
		t.Skip("filesystem clone unavailable; forced fallback covered separately")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if _, err := v.CommitArchive(map[string]string{"checkpoint": "later"}, ArchiveBatch{Delete: []ArchiveKey{{Kind: "receipt", ID: "0"}}}, 0); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(path, []byte("capture private fixture password"))
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var active map[string]string
	records, stats, err := copied.LoadComplete(&active, 0)
	if err != nil || len(records) != 130 || stats.Count != 130 || active["checkpoint"] != "original" {
		t.Fatal(len(records), stats, active, err)
	}
	// The clone API may never overwrite an existing destination.
	if _, _, err := v.CaptureArchive(path, true); err == nil {
		t.Fatal("existing destination overwritten")
	}
	if _, err := copied.Load(&active); err != nil || active["checkpoint"] != "original" {
		t.Fatal(active, err)
	}
}
