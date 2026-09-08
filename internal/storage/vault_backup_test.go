package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingVaultBackupCreatesIndependentAuthenticatedDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	password := []byte("existing backup regression password")
	state := map[string]string{"proof": "retained private recovery material"}
	if err := Initialize(path, password, state); err != nil {
		t.Fatal(err)
	}
	v, err := OpenExisting(path, password)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	cold := ArchiveRecord{Kind: "seen", ID: "durable-message", Data: []byte(`"exact-message-digest"`)}
	if _, err := v.CommitArchive(state, ArchiveBatch{Put: []ArchiveRecord{cold}}, 0); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, replace := range []bool{false, true} {
		label := "new"
		if replace {
			label = "replace-existing"
		}
		t.Run(label, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "backup.db")
			if replace {
				if err := os.WriteFile(target, bytes.Repeat([]byte("old unrelated bytes"), 100000), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := v.Backup(target); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0600 || info.Size() > int64(len(sourceBefore)) {
				t.Fatal("backup destination permission or stale trailing bytes", info, err)
			}
			contents, err := os.ReadFile(target)
			if err != nil || bytes.Contains(contents, []byte(state["proof"])) || bytes.Contains(contents, cold.Data) {
				t.Fatal("backup exposed private plaintext", err)
			}
			backup, err := OpenReadOnly(target, password)
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			var got map[string]string
			if _, err := backup.Load(&got); err != nil || got["proof"] != state["proof"] {
				t.Fatal("backup changed active state", got, err)
			}
			retained, found, err := backup.ReadArchive(cold.Kind, cold.ID)
			if err != nil || !found || !bytes.Equal(retained.Data, cold.Data) {
				t.Fatal("backup lost authenticated cold record", err)
			}
			stats, err := backup.ArchiveStats()
			if err != nil || stats.Count != 1 {
				t.Fatal("backup lost authenticated archive statistics", stats, err)
			}
		})
	}
	sourceAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatal("backup mutated the source", err)
	}
	if other, err := OpenExisting(path, password); err == nil {
		other.Close()
		t.Fatal("backup released the source writer lock")
	}
}

func TestExistingVaultBackupFailurePreservesSourceAndOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	password := []byte("existing backup failure password")
	if err := Initialize(path, password, map[string]int{"revision": 1}); err != nil {
		t.Fatal(err)
	}
	v, err := OpenExisting(path, password)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.Backup(filepath.Join(t.TempDir(), "missing-parent", "backup.db")); err == nil {
		t.Fatal("missing destination directory accepted")
	}
	if err := v.Save(map[string]int{"revision": 2}); err != nil {
		t.Fatal("backup error broke source writer", err)
	}
	target := filepath.Join(t.TempDir(), "valid.db")
	if err := v.Backup(target); err != nil {
		t.Fatal("backup did not recover from destination error", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	closedTarget := filepath.Join(t.TempDir(), "must-not-exist.db")
	if err := v.Backup(closedTarget); err == nil {
		t.Fatal("closed source accepted backup")
	}
	if _, err := os.Stat(closedTarget); !os.IsNotExist(err) {
		t.Fatal("closed source created a destination", err)
	}
	backup, err := OpenReadOnly(target, password)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var got map[string]int
	if _, err := backup.Load(&got); err != nil || got["revision"] != 2 {
		t.Fatal("backup after failed destination lost latest state", got, err)
	}
}
