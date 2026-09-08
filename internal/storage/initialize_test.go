package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInitializePublishesCompleteStateOrKeepsFinalAbsent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	password := []byte("disposable-initialization-credential")
	initial := map[string]any{"version": 3, "retained": "initial identity"}
	interrupted := errors.New("interrupted before atomic publication")
	err := initialize(path, password, initial, func(private string) error {
		v, err := OpenReadOnly(private, password)
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		var state struct {
			Version  int
			Retained string
		}
		if _, err := v.Load(&state); err != nil || state.Version != 3 || state.Retained != "initial identity" {
			t.Fatal("private state is not complete before publication")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial state exposed at final path")
		}
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted initialization stranded a final empty vault")
	}
	// A failed first encoding cannot publish even an authenticated empty state.
	if err := Initialize(path, password, map[string]any{"invalid": make(chan int)}); err == nil {
		t.Fatal("invalid initial state was published")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed first save created the final vault")
	}
	if err := Initialize(path, password, initial); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Initialize(path, password, map[string]any{"version": 3, "retained": "replacement"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("initializer did not refuse existing destination: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("second initializer changed an existing vault")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.db" {
		t.Fatalf("returned initialization did not remove its private staging: %v", err)
	}
}
