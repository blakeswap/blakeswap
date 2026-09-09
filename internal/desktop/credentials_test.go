package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type isolatedCredentialStore struct {
	mu     sync.Mutex
	values map[credential.Key][]byte
	err    error
}

func newIsolatedCredentialStore() *isolatedCredentialStore {
	return &isolatedCredentialStore{values: map[credential.Key][]byte{}}
}
func (s *isolatedCredentialStore) Get(ctx context.Context, key credential.Key) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	p, ok := s.values[key]
	if !ok {
		return nil, credential.ErrMissing
	}
	return append([]byte(nil), p...), nil
}
func (s *isolatedCredentialStore) Create(ctx context.Context, key credential.Key, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.err != nil {
		return s.err
	}
	if _, ok := s.values[key]; ok {
		return credential.ErrExists
	}
	s.values[key] = append([]byte(nil), p...)
	return nil
}
func (s *isolatedCredentialStore) Delete(ctx context.Context, key credential.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.err != nil {
		return s.err
	}
	clear(s.values[key])
	delete(s.values, key)
	return nil
}

func nativeManager(t *testing.T) (*Manager, *isolatedCredentialStore) {
	t.Helper()
	m := setupManager(t)
	store := newIsolatedCredentialStore()
	var err error
	m.credentials, err = openProfileCredentials(context.Background(), m.root, store)
	if err != nil {
		t.Fatal(err)
	}
	m.authority, err = authorization.New("isolated native test session", nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}
func noPasswordFile(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, "vault.password")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("desktop created plaintext credential", err)
	}
}

func TestNativeDesktopMigrationPreservesEveryNetworkAndRejectsFallback(t *testing.T) {
	m := installedManager(t)
	root := filepath.Join(m.root, "wallets", "alice")
	seed, password, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	states := map[chain.Network]daemon.State{}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		state := daemon.State{Version: daemon.StateVersion, Network: network, Mnemonic: seed, TradeReceipts: map[string]*daemon.TradeReceipt{"accepted": {Digest: "exact saved acceptance", Result: daemon.ConfirmTradeResult{ID: "accepted", State: "accepted"}}}, ReceiveIndexes: map[chain.ID]uint32{chain.BTC: 17, chain.Blake: 29}}
		states[network] = state
		if err := saveVault(filepath.Join(root, string(network), "state.db"), password, state); err != nil {
			t.Fatal(err)
		}
	}
	store := newIsolatedCredentialStore()
	m.credentials, err = openProfileCredentials(context.Background(), m.root, store)
	if err != nil {
		t.Fatal(err)
	}
	noPasswordFile(t, root)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		gotSeed, p, err := m.readMaster(root)
		if err != nil || gotSeed != seed {
			t.Fatal("migration changed seed", err)
		}
		v, err := storage.Open(filepath.Join(root, string(network), "state.db"), p)
		clear(p)
		if err != nil {
			t.Fatal(err)
		}
		var state daemon.State
		_, err = v.Load(&state)
		v.Close()
		if err != nil || !reflect.DeepEqual(state, states[network]) {
			t.Fatal("migration changed pending state", err)
		}
	}
	if _, err = openProfileCredentials(context.Background(), m.root, store); err != nil {
		t.Fatal("helper restart", err)
	}
	// Even if an old file reappears, native failures must not select it.
	if err := os.WriteFile(filepath.Join(root, "vault.password"), password, 0600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{credential.ErrLocked, credential.ErrDenied, credential.ErrMissing} {
		store.err = failure
		if _, p, err := m.readMaster(root); !errors.Is(err, failure) {
			clear(p)
			t.Fatal("native read fell back", err)
		}
		if _, err := openProfileCredentials(context.Background(), m.root, store); !errors.Is(err, failure) {
			t.Fatal("restart fell back", err)
		}
	}
}

func TestNativeNewWalletAndSeparateInstallationPortableRestore(t *testing.T) {
	m, store := nativeManager(t)
	request := &pb.PrepareFirstWalletRequest{Name: "Native", Revision: m.settings.Revision}
	first, err := m.prepareFirstWallet(nativeConsent(t, m, "onboarding.prepare", request), request)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(m.root, "wallets", "alice")
	noPasswordFile(t, root)
	m.settings.OnboardingStage = ""
	if err := saveSettings(m.root, m.settings); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	if _, err := m.exportPortable(nativeConsent(t, m, "backup.export", &pb.ExportPortableBackupRequest{Path: path, Password: "isolated portable password"}), "alice", path, "isolated portable password", false); err != nil {
		t.Fatal(err)
	}
	other, otherStore := nativeManager(t)
	restore := &pb.PrepareFirstWalletRequest{Name: "Restored", Revision: other.settings.Revision, BackupPath: path, BackupPassword: "isolated portable password"}
	restored, err := other.prepareFirstWallet(nativeConsent(t, other, "onboarding.prepare", restore), restore)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Settings.OnboardingStage != "connect" {
		t.Fatal("restore stage changed")
	}
	otherRoot := filepath.Join(other.root, "wallets", "alice")
	noPasswordFile(t, otherRoot)
	seed, p, err := other.readMaster(otherRoot)
	if err != nil || seed != first.Recovery.Mnemonic {
		t.Fatal("restored different identity", err)
	}
	defer clear(p)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		v, err := storage.Open(filepath.Join(otherRoot, string(network), "state.db"), p)
		if err != nil {
			t.Fatal(err)
		}
		var s daemon.State
		_, err = v.Load(&s)
		v.Close()
		if err != nil || s.Recovery == nil {
			t.Fatal("native restore bypassed recovery hold", err)
		}
	}
	if other.credentials.installation == m.credentials.installation || len(store.values) != 1 || len(otherStore.values) != 1 {
		t.Fatal("restore reused original installation credentials")
	}
	for k := range store.values {
		if _, ok := otherStore.values[k]; ok {
			t.Fatal("portable archive carried original credential reference")
		}
	}
}

func TestNativeInterruptedImportKeepsPublishedCredentialAndInstallationHold(t *testing.T) {
	m, store := nativeManager(t)
	request := &pb.PrepareFirstWalletRequest{Name: "Native", Revision: m.settings.Revision}
	if _, err := m.prepareFirstWallet(nativeConsent(t, m, "onboarding.prepare", request), request); err != nil {
		t.Fatal(err)
	}
	m.settings.OnboardingStage = ""
	if err := saveSettings(m.root, m.settings); err != nil {
		t.Fatal(err)
	}
	manifest := portableManifest(t)
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	if err := storage.WritePortable(context.Background(), path, []byte("isolated archive password"), manifest); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(m.root, "settings.json")
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(settings, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = m.importPortable(nativeConsent(t, m, "backup.import", &pb.ImportBackupRequest{Path: path, Password: "isolated archive password", Name: "Recovered", Revision: m.settings.Revision}), portableImportRequest{Path: path, Password: "isolated archive password", Name: "Recovered", Revision: m.settings.Revision}); err == nil {
		t.Fatal("publication failure missed")
	}
	if m.unpublishedInstalls.Load() != 1 || len(store.values) != 2 {
		t.Fatal("durable unpublished installation lost credential or hold")
	}
	if err = os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(settings, raw, 0600); err != nil {
		t.Fatal(err)
	}
	credentials, err := openProfileCredentials(context.Background(), m.root, store)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := loadSettingsWithReader(m.root, credentials.readMaster)
	if err != nil || len(recovered.Wallets) != 2 {
		t.Fatal("restart did not authenticate and publish recovered profile", err)
	}
	for _, w := range recovered.Wallets {
		noPasswordFile(t, filepath.Join(m.root, "wallets", w.Id))
	}
}

func nativeConsent(t *testing.T, m *Manager, method string, input any) context.Context {
	return authorization.WithGrant(withProfile(context.Background(), "alice"), nativeGrantID(t, m, method, input))
}
func nativeGrantID(t *testing.T, m *Manager, method string, input any) string {
	t.Helper()
	params, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(params)
	raw, err := json.Marshal(consentRequest{Profile: "alice", Method: method, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	result, code := m.brokerHandler(context.Background(), "consent.prepare", raw)
	if code != "" {
		t.Fatal("private prepare", code)
	}
	challenge := result.(authorization.Challenge)
	approved, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, code = m.brokerHandler(context.Background(), "consent.approve", approved); code != "" {
		t.Fatal("private approve", code)
	}
	return challenge.ID
}
