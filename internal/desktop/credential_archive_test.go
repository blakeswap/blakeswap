package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	bolt "go.etcd.io/bbolt"
)

func credentialSourceDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	if filepath.Ext(path) != ".db" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(data)
		return sha256.Sum256(data)
	}
	// Opening an existing vault commits bbolt bookkeeping even without a state
	// write. Compare every encrypted bucket key/value rather than page metadata.
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := sha256.New()
	enc := json.NewEncoder(h)
	err = db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, bucket *bolt.Bucket) error {
			return bucket.ForEach(func(key, value []byte) error {
				return enc.Encode([][]byte{name, key, value})
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return [32]byte(h.Sum(nil))
}

func TestNativeMigrationRejectsArchiveCheckpointBeforeActivation(t *testing.T) {
	m := captureManagerFixture(t, true)
	root := filepath.Join(m.root, "wallets", "alice")
	_, password, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	path := filepath.Join(root, "regtest", "state.db")
	v, err := storage.Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	var state daemon.State
	if _, err = v.Load(&state); err != nil {
		t.Fatal(err)
	}
	state.Capacity.Archived.Count++
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately construct an authenticated but inconsistent source; the raw
	// form bypasses the normal typed writer's checkpoint admission check.
	if err = v.Save(json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	before := map[string][32]byte{}
	for _, name := range []string{"master.db", "regtest/state.db", "vault.password"} {
		before[name] = credentialSourceDigest(t, filepath.Join(root, name))
	}
	// The state-format cutover now authenticates every source before even
	// creating the installation reference, migration journal or native item.
	// Keep the original encrypted-record comparison and require exact files too.
	beforeFiles := cutoverFileDigests(t, m.root)
	store := newIsolatedCredentialStore()
	if _, err = openProfileCredentials(context.Background(), m.root, store); err == nil {
		t.Fatal("inconsistent archived ownership activated a native credential")
	}
	if _, err := credential.ReadJournal(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid source reached journal creation", err)
	}
	if len(store.values) != 0 || !reflect.DeepEqual(beforeFiles, cutoverFileDigests(t, m.root)) {
		t.Fatal("archive preflight refusal changed files or native items")
	}
	for name, expected := range before {
		got := credentialSourceDigest(t, filepath.Join(root, name))
		if got != expected {
			t.Fatal("migration rewrote rejected source", name)
		}
	}
}

// All files and items here belong to generated isolated fixtures. Rekeying the
// fixture exercises provider bytes that a random hex credential would not cover.
func nativeArchiveCaptureFixture(t *testing.T, fallback bool) (*Manager, *isolatedCredentialStore, []byte, string) {
	t.Helper()
	m := captureManagerFixture(t, fallback)
	root := filepath.Join(m.root, "wallets", "alice")
	seed, old, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(old)
	path := filepath.Join(root, "regtest", "state.db")
	v, err := storage.Open(path, old)
	if err != nil {
		t.Fatal(err)
	}
	state, err := daemon.LoadCompleteState(v)
	closeErr := v.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	store := newIsolatedCredentialStore()
	m.credentials, err = openProfileCredentials(context.Background(), m.root, store)
	if err != nil {
		t.Fatal(err)
	}
	exact := []byte(" \t\x00isolated native archive credential\x00\n ")
	t.Cleanup(func() {
		clear(exact)
		for _, value := range store.values {
			clear(value)
		}
	})
	for name, value := range map[string]any{"master.db": struct {
		Mnemonic string `json:"mnemonic"`
	}{seed}, "regtest/state.db": state} {
		target := filepath.Join(root, name)
		staged := target + ".fixture"
		if err = saveVault(staged, exact, value); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(staged, target); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range store.values {
		clear(value)
		store.values[key] = bytes.Clone(exact)
	}
	if _, err = verifyProfile(root, exact); err != nil {
		t.Fatal(err)
	}
	noPasswordFile(t, root)
	return m, store, exact, seed
}

type captureCredentialReads struct {
	credential.Store
	returned [][]byte
}

func (s *captureCredentialReads) Get(ctx context.Context, key credential.Key) ([]byte, error) {
	value, err := s.Store.Get(ctx, key)
	s.returned = append(s.returned, value)
	return value, err
}

func TestNativeArchiveCaptureOwnsExactCredentialUntilClose(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "clone"
		if fallback {
			name = "fallback"
		}
		for _, cancelled := range []bool{false, true} {
			outcome := "success"
			if cancelled {
				outcome = "cancelled"
			}
			t.Run(name+"/"+outcome, func(t *testing.T) {
				m, store, exact, seed := nativeArchiveCaptureFixture(t, fallback)
				reads := &captureCredentialReads{Store: store}
				m.credentials.profiles.Store = reads
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				m.mu.Lock()
				capture, err := m.captureBackupLocked(ctx, "alice", false)
				m.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if len(reads.returned) == 0 {
					t.Fatal("capture did not use native provider")
				}
				for _, original := range reads.returned {
					if !bytes.Equal(original, make([]byte, len(original))) {
						t.Fatal("capture retained acquired provider reply")
					}
				}
				t.Cleanup(func() { capture.closeSources(); capture.staging.close() })
				if !fallback && !capture.sources[0].cloned {
					t.Skip("filesystem clone unavailable")
				}
				if fallback && capture.sources[0].page == nil {
					t.Fatal("fallback not selected")
				}
				keys := capture.staging.clonePasswords
				for i, key := range keys {
					if !bytes.Equal(key, exact) || &key[0] == &exact[0] {
						t.Fatal("capture changed or borrowed native bytes")
					}
					for j := 0; j < i; j++ {
						if &key[0] == &keys[j][0] {
							t.Fatal("sources share credential ownership")
						}
					}
				}
				store.err = credential.ErrDenied
				readCount := len(reads.returned)
				if cancelled {
					cancel()
				}
				snapshot, err := capture.materialize()
				if cancelled {
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancelled capture returned a snapshot", err)
					}
				} else {
					if err != nil {
						t.Fatal("capture reacquired denied provider", err)
					}
					state, err := snapshot.Wallets[0].networkState(chain.Regtest)
					if err != nil || state.Mnemonic != seed || len(state.Archive) != 130 {
						t.Fatal("capture changed identity or cold records", err)
					}
				}
				snapshot.close()
				if len(reads.returned) != readCount {
					t.Fatal("materialization reacquired source credential")
				}
				select {
				case <-capture.done:
				default:
					t.Fatal("capture retained a source lease")
				}
				if _, err = os.Stat(capture.staging.root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("private capture remains", err)
				}
				for _, key := range keys {
					if !bytes.Equal(key, make([]byte, len(key))) {
						t.Fatal("closed capture retained native credential")
					}
				}
				if !m.mu.TryLock() {
					t.Fatal("capture retained lifecycle lock")
				}
				m.mu.Unlock()
			})
		}
	}
}

func TestNativeStreamedArchiveRestoresWithSeparateCredentialProvider(t *testing.T) {
	m, sourceStore, _, seed := nativeArchiveCaptureFixture(t, true)
	capture := captureManager(t, m)
	sourceStore.err = credential.ErrLocked
	snapshot, err := capture.materialize()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.close()
	path := filepath.Join(t.TempDir(), "owned-stream.backup")
	password := "isolated portable archive password"
	if err = writeStreamManifest(context.Background(), path, []byte(password), snapshot); err != nil {
		t.Fatal(err)
	}
	other, otherStore := nativeManager(t)
	request := &pb.PrepareFirstWalletRequest{Name: "Restored archive", Revision: other.settings.Revision, BackupPath: path, BackupPassword: password}
	if _, err = other.prepareFirstWallet(nativeConsent(t, other, "onboarding.prepare", request), request); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(other.root, "wallets", "alice")
	noPasswordFile(t, root)
	gotSeed, key, err := other.readMaster(root)
	if err != nil || gotSeed != seed {
		t.Fatal("separate provider changed restored identity", err)
	}
	defer clear(key)
	state, err := readStateBackupBytes(other.root, filepath.Join(root, "regtest", "state.db"), key, portableVaultLimit)
	if err != nil || len(state.Archive) != 130 || state.Recovery == nil || state.Recovery.Status.State != "recovering" {
		t.Fatal("streamed native install lost archive or recovery hold", err)
	}
	if other.credentials.installation == m.credentials.installation || len(otherStore.values) != 1 {
		t.Fatal("restore reused source credential authority")
	}
	for k := range sourceStore.values {
		if _, exists := otherStore.values[k]; exists {
			t.Fatal("portable archive carried source credential reference")
		}
	}
	if _, err = openProfileCredentials(context.Background(), other.root, otherStore); err != nil {
		t.Fatal("restored archive failed native restart", err)
	}
}
