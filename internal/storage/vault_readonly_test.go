package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestReadOnlyVaultNeverCreatesOrWritesSource(t *testing.T) {
	root := t.TempDir()
	password := []byte("disposable-readonly-credential")
	missing := filepath.Join(root, "missing", "state.db")
	if v, err := OpenReadOnly(missing, password); err == nil {
		_ = v.Close()
		t.Fatal("read-only inspection created a vault")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only inspection created a directory")
	}
	path := filepath.Join(root, "state.db")
	v, err := Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Save(map[string]string{"retained": "authenticated"}); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := OpenReadOnly(path, []byte("another-disposable-password")); err == nil {
		_ = v.Close()
		t.Fatal("wrong password opened read-only vault")
	}
	v, err = OpenReadOnly(path, password)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]string
	if _, err := v.Load(&state); err != nil || state["retained"] != "authenticated" {
		t.Fatalf("read-only typed load: %v", err)
	}
	if err := v.Save(map[string]string{"retained": "changed"}); !errors.Is(err, bolt.ErrDatabaseReadOnly) {
		t.Fatalf("read-only write was not refused: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("authenticated read-only inspection changed source bytes: %v", err)
	}
}

func TestReadOnlyVaultWriterLockIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	password := []byte("disposable-readonly-lock-credential")
	writer, err := Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	start := time.Now()
	reader, err := OpenReadOnly(path, password)
	elapsed := time.Since(start)
	if reader != nil {
		_ = reader.Close()
	}
	if !errors.Is(err, bolt.ErrTimeout) || elapsed > 3*time.Second {
		t.Fatalf("read-only writer exclusion lacks bounded timeout: elapsed=%s err=%v", elapsed, err)
	}
}
