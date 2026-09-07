package desktop

import (
	"path/filepath"
	"testing"

	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestPortableDuplicateIdentityIncludesUnlistedPreparedProfiles(t *testing.T) {
	m := setupManager(t)
	original := prepare(t, m)
	manifest := portableManifest(t)
	if err := m.checkBackupIdentitiesLocked(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Wallets[0].Mnemonic = original.Recovery.Mnemonic
	manifest.Wallets[0].Identity, _ = backupIdentity(original.Recovery.Mnemonic)
	if err := m.checkBackupIdentitiesLocked(manifest); err == nil {
		t.Fatal("duplicate live identity accepted")
	}
	manifest = portableManifest(t)
	path := filepath.Join(m.root, "wallets", "wallet-unlisted")
	password := []byte("private install password")
	if err := writePrivate(filepath.Join(path, "vault.password"), password); err != nil {
		t.Fatal(err)
	}
	vault, err := storage.Open(filepath.Join(path, "master.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Save(struct {
		Mnemonic string `json:"mnemonic"`
	}{manifest.Wallets[0].Mnemonic}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.checkBackupIdentitiesLocked(manifest); err == nil {
		t.Fatal("unlisted prepared duplicate accepted")
	}
	if err := writePrivate(filepath.Join(path, "vault.password"), []byte("a wrong install password")); err != nil {
		t.Fatal(err)
	}
	if err := m.checkBackupIdentitiesLocked(portableManifest(t)); err == nil {
		t.Fatal("unreadable installed identity was ignored")
	}
}
