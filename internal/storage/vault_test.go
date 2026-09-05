package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestVaultAuthenticationAtomicBackupAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	pass := []byte("a long test-only passphrase")
	v, e := Open(path, pass)
	if e != nil {
		t.Fatal(e)
	}
	secret := "never publish this wallet seed or swap preimage"
	if e = v.Save(map[string]string{"secret": secret}); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(path, pass); e == nil {
		t.Fatal("two writers opened the vault")
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if e = v.Backup(backup); e != nil {
		t.Fatal(e)
	}
	if e = v.Close(); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("plaintext secret on disk")
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("vault permissions", e)
	}
	if _, e = Open(path, []byte("wrong long test-only passphrase")); e == nil {
		t.Fatal("wrong password accepted")
	}
	v, e = Open(backup, pass)
	if e != nil {
		t.Fatal(e)
	}
	defer v.Close()
	var state map[string]string
	if _, e = v.Load(&state); e != nil || state["secret"] != secret {
		t.Fatal("backup did not preserve secret", e)
	}
}
