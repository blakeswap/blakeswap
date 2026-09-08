package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestStateCutoverRefusesLegacyAtEveryArchiveRecoveryBoundary(t *testing.T) {
	for _, version := range []int{0, 1, 2, 4} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			s := State{Version: version, Network: chain.Regtest, Mnemonic: "retained incompatible fixture"}
			before, _ := json.Marshal(s)
			checks := []func() error{
				func() error { return ValidateStateVersion(&s) },
				func() error { return s.ValidateArchiveCheckpoint(storage.ArchiveStats{}) },
				func() error { return ValidateArchiveState(s) },
				func() error { _, err := CompleteState(s); return err },
				func() error { _, _, err := s.VaultSnapshot(); return err },
				func() error { return PrepareRecovery(&s, time.Now().Unix(), false) },
				func() error { return PrepareStreamedRecovery(&s, storage.ArchiveStats{}, time.Now().Unix(), false) },
				func() error {
					_, err := ValidateArchiveRecordAgainstState(s, storage.ArchiveRecord{Kind: "seen", ID: "old", Data: json.RawMessage(`"digest"`)})
					return err
				},
			}
			for i, check := range checks {
				if err := check(); err == nil {
					t.Fatalf("boundary %d accepted state%d", i, version)
				}
				after, _ := json.Marshal(s)
				if !bytes.Equal(before, after) {
					t.Fatalf("boundary %d mutated rejected state", i)
				}
			}
		})
	}
}

func TestStateCutoverPreservesExistingVersionZeroAndLegacyVaults(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := t.TempDir()
			password := []byte("disposable-cutover-test-credential")
			passwordPath, path := filepath.Join(root, "password"), filepath.Join(root, "state.db")
			if err := os.WriteFile(passwordPath, password, 0600); err != nil {
				t.Fatal(err)
			}
			vault, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			// Construct old encoded data without calling the new State writer.
			if err := vault.Save(map[string]any{"version": version, "network": "regtest", "mnemonic": "must not be replaced", "seen": map[string]string{"preserve": "evidence"}}); err != nil {
				t.Fatal(err)
			}
			if err := vault.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{Network: chain.Regtest, Mode: "trader", Name: "cutover", DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // No network activity is necessary to reject an old format.
			if engine, err := Open(ctx, c); err == nil {
				_ = engine.Close()
				t.Fatal("incompatible existing state was initialized or opened")
			}
			if err := CheckStoredNetwork(c); err == nil {
				t.Fatal("offline network guard accepted incompatible state")
			}
			if _, err := LoadStoredActions(c); err == nil {
				t.Fatal("offline action reader accepted incompatible state")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected source vault was changed: %v", err)
			}
			got, err := os.ReadFile(passwordPath)
			if err != nil || !bytes.Equal(got, password) {
				t.Fatalf("rejected source credential was changed: %v", err)
			}
		})
	}
}

func TestStateCutoverFirstCompactionKeepsStateThree(t *testing.T) {
	e := &Engine{s: State{Version: StateVersion, Network: chain.Regtest}}
	record := storage.ArchiveRecord{Kind: "seen", ID: "retained", Data: json.RawMessage(`"digest"`)}
	if err := e.archiveDelta(record, true); err != nil || e.s.Version != StateVersion {
		t.Fatalf("first compaction downgraded state: version%d %v", e.s.Version, err)
	}
	if err := e.s.ValidateArchiveCheckpoint(e.s.Capacity.Archived); err != nil {
		t.Fatal(err)
	}
	old := &Engine{s: State{Version: 2, Network: chain.Regtest}}
	if err := old.archiveDelta(record, true); err == nil || old.s.Capacity != nil || old.s.Version != 2 {
		t.Fatal("compaction relabelled or modified an incompatible state")
	}
}

func TestStateCutoverOnlyAbsentFileInitializesNewState(t *testing.T) {
	root := t.TempDir()
	password := []byte("disposable-new-cutover-credential")
	passwordPath := filepath.Join(root, "password")
	if err := os.WriteFile(passwordPath, password, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Network: chain.Regtest, Mode: "trader", Name: "new-cutover", DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	engine, _ := Open(ctx, c) // Deliberately absent nodes cannot affect local initialization.
	if engine != nil {
		_ = engine.Close()
	}
	v, err := storage.OpenReadOnly(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var s State
	if _, err := v.Load(&s); err != nil || s.Version != StateVersion || s.Mnemonic == "" {
		t.Fatalf("new vault did not initialize State3: version=%d err=%v", s.Version, err)
	}
}

func TestStateCutoverOuterMarkerCannotUpgradeLegacyChild(t *testing.T) {
	for _, version := range []int{0, 1} {
		child := &Swap{ID: "legacy-child", Role: "maker", Request: protocol.Request{Version: version}, Terms: &protocol.Terms{Version: version}}
		s := State{Version: StateVersion, Network: chain.Regtest, Swaps: map[string]*Swap{child.ID: child}}
		before, _ := json.Marshal(s)
		for i, check := range []func() error{
			func() error { return ValidateProtocolState(&s) },
			func() error { return ValidateArchiveState(s) },
			func() error { return PrepareRecovery(&s, time.Now().Unix(), false) },
			func() error { return PrepareStreamedRecovery(&s, storage.ArchiveStats{}, time.Now().Unix(), false) },
			func() error { _, _, err := s.VaultSnapshot(); return err },
		} {
			if err := check(); err == nil {
				t.Fatalf("boundary%d interpreted a legacy child under State3", i)
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("incompatible embedded child was mutated")
			}
		}
		encoded, _ := json.Marshal(child)
		active := State{Version: StateVersion, Network: chain.Regtest}
		if _, err := ValidateArchiveRecordAgainstState(active, storage.ArchiveRecord{Kind: "swaps", ID: child.ID, Data: encoded}); err == nil {
			t.Fatal("cold legacy child bypassed state cutover")
		}
	}
}
