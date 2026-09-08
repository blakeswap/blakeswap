package storage

import (
	"bytes"
	"errors"
	"fmt"
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

func TestExistingWriterAuthenticatesWithoutOpeningWrites(t *testing.T) {
	for _, noFreelistSync := range []bool{false, true} {
		t.Run(fmt.Sprint(noFreelistSync), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			password := []byte("disposable-existing-writer-credential")
			v, err := Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			v.db.NoFreelistSync = noFreelistSync
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
			for _, credential := range [][]byte{[]byte("another-disposable-password"), password} {
				v, err = OpenExisting(path, credential)
				if bytes.Equal(credential, password) {
					if err != nil {
						t.Fatal(err)
					}
					var state map[string]string
					if _, err := v.Load(&state); err != nil || state["retained"] != "authenticated" {
						t.Fatalf("load: %v", err)
					}
				} else if err == nil {
					t.Fatal("wrong credential authenticated")
				}
				if v != nil {
					if err := v.Close(); err != nil {
						t.Fatal(err)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("opening changed existing source bytes: %v", err)
				}
			}
			v, err = OpenExisting(path, password)
			if err != nil {
				t.Fatal(err)
			}
			if v.db.NoFreelistSync {
				t.Fatal("normal commit durability not restored")
			}
			if err := v.Save(map[string]string{"retained": "explicitly changed"}); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			v, err = OpenReadOnly(path, password)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var state map[string]string
			if _, err := v.Load(&state); err != nil || state["retained"] != "explicitly changed" {
				t.Fatalf("authorized save: %v", err)
			}
		})
	}
}

func TestExistingWriterNeverInitializesAndLockIsBounded(t *testing.T) {
	root := t.TempDir()
	password := []byte("disposable-existing-lock-credential")
	for _, name := range []string{"missing/state.db", "empty.db", "arbitrary.db"} {
		path := filepath.Join(root, name)
		var before []byte
		if name != "missing/state.db" {
			if name == "arbitrary.db" {
				before = []byte("retained incompatible bytes")
			}
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if v, err := OpenExisting(path, password); err == nil {
			v.Close()
			t.Fatal("initialized unsupported existing file")
		}
		after, err := os.ReadFile(path)
		if name == "missing/state.db" {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created absent file")
			}
			if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created absent directory")
			}
		} else if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("changed rejected file: %v", err)
		}
	}
	path := filepath.Join(root, "locked.db")
	writer, err := Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	start := time.Now()
	v, err := OpenExisting(path, password)
	elapsed := time.Since(start)
	if v != nil {
		v.Close()
	}
	if !errors.Is(err, bolt.ErrTimeout) || elapsed > 3*time.Second {
		t.Fatalf("exclusive open not bounded: %s %v", elapsed, err)
	}
}
