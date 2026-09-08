package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestNativeCredentialFailureNeverFallsBackForLiveOrOfflineReaders(t *testing.T) {
	root := t.TempDir()
	password := []byte("isolated legacy fixture password")
	path := filepath.Join(root, "vault.password")
	if err := os.WriteFile(path, password, 0600); err != nil {
		t.Fatal(err)
	}
	v, err := storage.Open(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Save(State{Version: 1, Network: chain.Regtest}); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	for _, providerErr := range []error{credential.ErrDenied, credential.ErrLocked, credential.ErrMissing, credential.ErrUnavailable} {
		t.Run(providerErr.Error(), func(t *testing.T) {
			cfg := Config{Name: "alice", Mode: "trader", Network: chain.Regtest, DataDir: root, PasswordFile: path, Relays: []string{"ws://127.0.0.1:1"}, CredentialMode: "native", Credential: credential.SourceFunc(func(context.Context) ([]byte, error) { return nil, providerErr })}
			if engine, err := Open(context.Background(), cfg); !errors.Is(err, providerErr) {
				if engine != nil {
					engine.Close()
				}
				t.Fatal("Open bypassed native credential failure", err)
			}
			if err := CheckStoredNetwork(cfg); !errors.Is(err, providerErr) {
				t.Fatal("network guard bypassed native credential failure", err)
			}
			if _, err := LoadStoredActions(cfg); !errors.Is(err, providerErr) {
				t.Fatal("offline actions bypassed native credential failure", err)
			}
		})
	}
	cfg := Config{CredentialMode: "native", PasswordFile: path}
	if _, err := cfg.acquirePassword(context.Background()); !errors.Is(err, credential.ErrUnavailable) {
		t.Fatal("missing native source selected existing legacy file", err)
	}
}

func TestNativeCredentialPreservesExactBytesAndOwnedLifetime(t *testing.T) {
	root := t.TempDir()
	password := []byte("  isolated native fixture credential  \n")
	v, err := storage.Open(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Save(State{Version: 1, Network: chain.Regtest}); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	cfg := Config{Name: "alice", Network: chain.Regtest, DataDir: root, CredentialMode: "native", Credential: credential.SourceFunc(func(context.Context) ([]byte, error) { calls++; return append([]byte(nil), password...), nil })}
	if err := CheckStoredNetwork(cfg); err != nil {
		t.Fatal("native password bytes were altered", err)
	}
	if _, err := LoadStoredActions(cfg); err != nil {
		t.Fatal("offline projection could not reopen independent credential copy", err)
	}
	if calls != 2 || string(password) != "  isolated native fixture credential  \n" {
		t.Fatal("one reader cleared another credential lifetime", calls)
	}
}
