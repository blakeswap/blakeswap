package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/storage"
)

type testStore struct {
	values          map[Key][]byte
	err             error
	createReplyLost bool
	creates         int
}

func (s *testStore) Get(_ context.Context, k Key) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[k]
	if !ok {
		return nil, ErrMissing
	}
	return append([]byte(nil), v...), nil
}
func (s *testStore) Create(_ context.Context, k Key, v []byte) error {
	if s.err != nil {
		return s.err
	}
	if _, ok := s.values[k]; ok {
		return ErrExists
	}
	s.values[k] = append([]byte(nil), v...)
	s.creates++
	if s.createReplyLost {
		return errors.New("synthetic lost create acknowledgement")
	}
	return nil
}
func (s *testStore) Delete(_ context.Context, k Key) error {
	clear(s.values[k])
	delete(s.values, k)
	return nil
}

func migrationVaults(t *testing.T, root string, password []byte) func([]byte) (string, error) {
	t.Helper()
	paths := []string{"master.db", "regtest/state.db", "testnet/state.db", "mainnet/state.db"}
	want := map[string]string{"identity": "isolated generated fixture", "pending": "retained signed obligation bytes", "commitment": "permanent"}
	for _, name := range paths {
		v, err := storage.Open(filepath.Join(root, name), password)
		if err != nil {
			t.Fatal(err)
		}
		if err = v.Save(want); err != nil {
			t.Fatal(err)
		}
		if err = v.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return func(password []byte) (string, error) {
		for _, name := range paths {
			v, err := storage.Open(filepath.Join(root, name), password)
			if err != nil {
				return "", err
			}
			var got map[string]string
			_, err = v.Load(&got)
			closeErr := v.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
			if !reflect.DeepEqual(got, want) {
				return "", errors.New("migration changed identity or pending state")
			}
		}
		return strings.Repeat("a", 64), nil
	}
}

func TestMigrationEveryDurableBoundaryPreservesVaultsAndResumes(t *testing.T) {
	for _, stage := range []string{"discovered", "created", "stored", "verified", "active", "file_removed", "removed"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			password := []byte("isolated original file credential")
			path := filepath.Join(root, "vault.password")
			if err := os.WriteFile(path, password, 0600); err != nil {
				t.Fatal(err)
			}
			verify := migrationVaults(t, root, password)
			store := &testStore{values: map[Key][]byte{}}
			stop := errors.New("simulated interruption")
			profiles := Profiles{Store: store, After: func(s string) error {
				if s == stage {
					return stop
				}
				return nil
			}}
			key := Key{Installation: "installation", Profile: "alice"}
			if _, err := profiles.Migrate(context.Background(), root, key, verify); !errors.Is(err, stop) {
				t.Fatal("missing interruption", err)
			}
			_, statErr := os.Stat(path)
			shouldRemain := stage != "file_removed" && stage != "removed"
			if shouldRemain && statErr != nil || !shouldRemain && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("plaintext removed at wrong boundary", stage, statErr)
			}
			if _, err := verify(password); err != nil {
				t.Fatal(err)
			}
			profiles.After = nil
			got, err := profiles.Migrate(context.Background(), root, key, verify)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(password) {
				t.Fatal("migration changed credential")
			}
			clear(got)
			journal, err := ReadJournal(root)
			if err != nil || journal.Phase != "removed" {
				t.Fatal("migration did not finish", journal.Phase, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("plaintext remains", err)
			}
			if store.creates != 1 {
				t.Fatal("migration replaced/recreated a Keychain item", store.creates)
			}
		})
	}
}

func TestMigrationAmbiguousCreateConflictAndNativeDenial(t *testing.T) {
	for _, mode := range []string{"lost acknowledgement", "conflicting item", "locked", "denied", "missing after active", "wrong after active"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			password := []byte("isolated original migration credential")
			path := filepath.Join(root, "vault.password")
			if err := os.WriteFile(path, password, 0600); err != nil {
				t.Fatal(err)
			}
			store := &testStore{values: map[Key][]byte{}, createReplyLost: mode == "lost acknowledgement"}
			key := Key{Installation: "installation", Profile: "alice"}
			profiles := Profiles{Store: store}
			verify := func(got []byte) (string, error) {
				if string(got) != string(password) {
					return "", ErrConflict
				}
				return strings.Repeat("a", 64), nil
			}
			if mode == "conflicting item" {
				j, err := profiles.begin(root, key, "legacy")
				if err != nil {
					t.Fatal(err)
				}
				store.values[j.Key] = []byte("different existing credential")
			}
			if mode == "locked" {
				store.err = ErrLocked
			}
			if mode == "denied" {
				store.err = ErrDenied
			}
			if mode == "missing after active" || mode == "wrong after active" {
				profiles.After = func(s string) error {
					if s == "active" {
						return errors.New("stop before file removal")
					}
					return nil
				}
				if _, err := profiles.Migrate(context.Background(), root, key, verify); err == nil {
					t.Fatal("active boundary not interrupted")
				}
				profiles.After = nil
				j, err := ReadJournal(root)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "missing after active" {
					delete(store.values, j.Key)
				} else {
					store.values[j.Key] = []byte("wrong active credential")
				}
			}
			got, err := profiles.Migrate(context.Background(), root, key, verify)
			defer clear(got)
			if mode == "lost acknowledgement" {
				if err != nil || string(got) != string(password) || store.creates != 1 {
					t.Fatal("ambiguous create failed to reconcile", err)
				}
			} else {
				if err == nil {
					t.Fatal("native failure selected legacy credential", mode)
				}
				if actual, err := os.ReadFile(path); err != nil || string(actual) != string(password) {
					t.Fatal("failure lost original credential", err)
				}
			}
		})
	}
}

func TestNewProfileNeverWritesPlaintextAndBindsInitializationIdentity(t *testing.T) {
	for _, stage := range []string{"discovered", "created", "stored", "verified", "active"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			store := &testStore{values: map[Key][]byte{}}
			profiles := Profiles{Store: store, After: func(s string) error {
				if s == stage {
					return errors.New("interrupted initialization")
				}
				return nil
			}}
			key := Key{Installation: "installation", Profile: "alice"}
			initialize := func(password []byte) error {
				v, err := storage.Open(filepath.Join(root, "master.db"), password)
				if err != nil {
					return err
				}
				defer v.Close()
				return v.Save(map[string]string{"identity": strings.Repeat("a", 64), "pending": "preserved"})
			}
			verify := func(password []byte) (string, error) {
				v, err := storage.Open(filepath.Join(root, "master.db"), password)
				if err != nil {
					return "", err
				}
				defer v.Close()
				var got map[string]string
				_, err = v.Load(&got)
				return got["identity"], err
			}
			if _, err := profiles.Initialize(context.Background(), root, key, strings.Repeat("a", 64), initialize, verify); err == nil {
				t.Fatal("interruption missed")
			}
			if _, err := os.Stat(filepath.Join(root, "vault.password")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("new profile wrote plaintext", err)
			}
			profiles.After = nil
			if stage != "discovered" {
				called := false
				if _, err := profiles.Initialize(context.Background(), root, key, strings.Repeat("b", 64), func([]byte) error { called = true; return nil }, verify); !errors.Is(err, ErrConflict) || called {
					t.Fatal("stale retry changed initialization identity", err)
				}
			}
			password, err := profiles.Initialize(context.Background(), root, key, strings.Repeat("a", 64), initialize, verify)
			if err != nil {
				t.Fatal(err)
			}
			clear(password)
			j, err := ReadJournal(root)
			if err != nil || j.Phase != "active" || j.Origin != "new" {
				t.Fatal(j, err)
			}
			delete(store.values, j.Key)
			called := false
			if _, err := profiles.Initialize(context.Background(), root, key, strings.Repeat("a", 64), func([]byte) error { called = true; return nil }, verify); !errors.Is(err, ErrMissing) || called {
				t.Fatal("missing active item recreated a wallet", err)
			}
		})
	}
}

func TestJournalRejectsMalformedOrPublicMetadata(t *testing.T) {
	valid := Journal{Version: 1, Key: Key{Installation: "installation", Profile: "alice", Record: strings.Repeat("a", 64)}, Origin: "legacy", Phase: "active", Identity: strings.Repeat("b", 64)}
	for _, mode := range []string{"public", "symlink", "oversized", "non-fingerprint identity", "missing identity", "invalid record", "unknown phase"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			j := valid
			switch mode {
			case "non-fingerprint identity":
				j.Identity = "synthetic words must never be accepted as an identity"
			case "missing identity":
				j.Identity = ""
			case "invalid record":
				j.Key.Record = strings.Repeat("z", 64)
			case "unknown phase":
				j.Phase = "unrecognized"
			}
			if err := writeJournal(root, j); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, JournalName)
			switch mode {
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte(strings.Repeat(" ", 8193)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadJournal(root); err == nil {
				t.Fatal("accepted invalid credential journal")
			}
		})
	}
}

func TestProfileCancellationNeverReturnsCredentialOrPrematurelyActivates(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		for _, boundary := range []string{"verification", "verified", "active"} {
			t.Run(fmt.Sprintf("new=%t/%s", fresh, boundary), func(t *testing.T) {
				root := t.TempDir()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := &testStore{values: map[Key][]byte{}}
				profiles := Profiles{Store: store, After: func(stage string) error {
					if stage == boundary {
						cancel()
					}
					return nil
				}}
				identity := strings.Repeat("a", 64)
				verify := func([]byte) (string, error) {
					if boundary == "verification" {
						cancel()
					}
					return identity, nil
				}
				key := Key{Installation: "installation", Profile: "alice"}
				var got []byte
				var err error
				if fresh {
					got, err = profiles.Initialize(ctx, root, key, identity, func([]byte) error { return nil }, verify)
				} else {
					if err = os.WriteFile(filepath.Join(root, "vault.password"), []byte("isolated migration credential"), 0600); err != nil {
						t.Fatal(err)
					}
					got, err = profiles.Migrate(ctx, root, key, verify)
				}
				if !errors.Is(err, context.Canceled) || got != nil {
					clear(got)
					t.Fatal("cancellation returned credential or wrong error", err)
				}
				j, err := ReadJournal(root)
				if err != nil {
					t.Fatal(err)
				}
				if boundary != "active" && j.Phase == "active" {
					t.Fatal("cancelled verification activated credential")
				}
				if !fresh {
					if _, err := os.Stat(filepath.Join(root, "vault.password")); err != nil {
						t.Fatal("cancelled migration removed original file", err)
					}
				}
			})
		}
	}
}

type sourceTestStore struct {
	Store
	get func(context.Context, Key) ([]byte, error)
}

func (s sourceTestStore) Get(ctx context.Context, key Key) ([]byte, error) { return s.get(ctx, key) }

func TestProfileSourceOwnsErrorsAndDoesNotReuseOrphanAccounts(t *testing.T) {
	key := Key{Installation: "installation", Profile: "alice"}
	identity := strings.Repeat("a", 64)
	store := &testStore{values: map[Key][]byte{}}
	profiles := Profiles{Store: store}
	var first Key
	for i := 0; i < 2; i++ {
		root := t.TempDir()
		password, err := profiles.Initialize(context.Background(), root, key, identity, func([]byte) error { return nil }, func([]byte) (string, error) { return identity, nil })
		clear(password)
		if err != nil {
			t.Fatal(err)
		}
		j, err := ReadJournal(root)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = j.Key
		} else if first == j.Key {
			t.Fatal("new profile reused an orphan's account")
		}
		for _, mode := range []string{"denied with bytes", "short value", "cancelled reply"} {
			t.Run(fmt.Sprintf("%d/%s", i, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				raw := []byte("isolated provider reply")
				p := Profiles{Store: sourceTestStore{get: func(context.Context, Key) ([]byte, error) {
					switch mode {
					case "denied with bytes":
						return raw, ErrDenied
					case "short value":
						raw = []byte("short")
					case "cancelled reply":
						cancel()
					}
					return raw, nil
				}}}
				source, err := p.Source(root, key)
				if err != nil {
					t.Fatal(err)
				}
				got, err := source.Acquire(ctx)
				if err == nil || got != nil {
					clear(got)
					t.Fatal("invalid native source reply accepted")
				}
				for _, b := range raw {
					if b != 0 {
						t.Fatal("failed provider reply retained credential bytes")
					}
				}
			})
		}
	}
	if store.creates != 2 {
		t.Fatal("new profile did not own a distinct item")
	}
}
