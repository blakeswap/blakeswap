package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

func cutoverCredentialProfile(t *testing.T, root, id string, version int) (string, []byte) {
	t.Helper()
	profile := filepath.Join(root, "wallets", id)
	if err := os.MkdirAll(filepath.Join(profile, "regtest"), 0700); err != nil {
		t.Fatal(err)
	}
	seed, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	password := []byte("disposable-credential-cutover-test")
	if err := writePrivate(filepath.Join(profile, "vault.password"), password); err != nil {
		t.Fatal(err)
	}
	if err := storage.Initialize(filepath.Join(profile, "master.db"), password, map[string]string{"mnemonic": seed}); err != nil {
		t.Fatal(err)
	}
	// An untyped writer prepares the deliberately incompatible authenticated
	// source; current typed State writers must never relabel that source.
	if err := storage.Initialize(filepath.Join(profile, "regtest", "state.db"), password, map[string]any{"version": version, "network": "regtest", "mnemonic": seed}); err != nil {
		t.Fatal(err)
	}
	return profile, password
}

func cutoverFileDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		result[relative] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCredentialCutoverRefusesAllProfilesBeforeJournalOrItemWrites(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := t.TempDir()
			cutoverCredentialProfile(t, root, "alice", daemon.StateVersion)
			cutoverCredentialProfile(t, root, "zulu", version)
			store := newIsolatedCredentialStore()
			before := cutoverFileDigests(t, root)
			if _, err := openProfileCredentials(context.Background(), root, store); err == nil {
				t.Fatal("incompatible profile proceeded to credential migration")
			}
			if !reflect.DeepEqual(before, cutoverFileDigests(t, root)) || len(store.values) != 0 {
				t.Fatal("format refusal changed source files, credential journal or native items")
			}
		})
	}
}

func TestCredentialCutoverCurrentMigrationThenNativeLegacyRefusalPreservesBytes(t *testing.T) {
	root := t.TempDir()
	profile, password := cutoverCredentialProfile(t, root, "alice", daemon.StateVersion)
	store := newIsolatedCredentialStore()
	if _, err := openProfileCredentials(context.Background(), root, store); err != nil {
		t.Fatal(err)
	}
	if len(store.values) != 1 {
		t.Fatal("current format did not create one native credential")
	}
	if _, err := os.Stat(filepath.Join(profile, "vault.password")); !os.IsNotExist(err) {
		t.Fatal("verified current profile did not finish its supported credential transition")
	}
	path := filepath.Join(profile, string(chain.Regtest), "state.db")
	v, err := storage.Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if _, err := v.Load(&state); err != nil {
		t.Fatal(err)
	}
	state["version"] = 2
	if err := v.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	before := cutoverFileDigests(t, root)
	for key, value := range store.values {
		copy := append([]byte(nil), value...)
		if _, err := openProfileCredentials(context.Background(), root, store); err == nil {
			t.Fatal("native credential activated an incompatible network state")
		}
		if !reflect.DeepEqual(before, cutoverFileDigests(t, root)) || len(store.values) != 1 || !reflect.DeepEqual(copy, store.values[key]) {
			t.Fatal("native refusal changed vault, journal, or exact item bytes")
		}
	}
}

func TestCredentialCutoverReadonlyPreflightHasBoundedWriterWait(t *testing.T) {
	root := t.TempDir()
	profile, password := cutoverCredentialProfile(t, root, "alice", daemon.StateVersion)
	v, err := storage.Open(filepath.Join(profile, "regtest", "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	before := cutoverFileDigests(t, root)
	store := newIsolatedCredentialStore()
	start := time.Now()
	if _, err := openProfileCredentials(context.Background(), root, store); err == nil {
		t.Fatal("credential preflight ignored another vault writer")
	}
	if time.Since(start) > 2*time.Second || !reflect.DeepEqual(before, cutoverFileDigests(t, root)) || len(store.values) != 0 {
		t.Fatal("writer contention was unbounded or changed credential state")
	}
}
