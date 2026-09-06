package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPortableArchiveAuthenticatesManifestAndNeverReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.blakeswap")
	password := []byte("chosen backup password")
	input := map[string]any{"format_version": 1, "created_at": 123, "wallet": "private-identity", "network": "regtest", "mnemonic": "private recovery words", "secret": "private preimage"}
	if err := WritePortable(context.Background(), path, password, input); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-identity", "regtest", "private recovery words", "private preimage"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("plaintext archive metadata", secret)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("backup is not private")
	}
	var output map[string]any
	if err := ReadPortable(context.Background(), path, password, &output); err != nil || output["wallet"] != input["wallet"] {
		t.Fatal(output, err)
	}
	if err := WritePortable(context.Background(), path, password, map[string]string{"other": "state"}); err == nil {
		t.Fatal("replaced existing backup")
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(raw, again) {
		t.Fatal("existing backup changed")
	}
	if err := ReadPortable(context.Background(), path, []byte("incorrect long password"), &output); err == nil {
		t.Fatal("wrong password accepted")
	}
	again, _ = os.ReadFile(path)
	if !bytes.Equal(raw, again) {
		t.Fatal("failed import changed source")
	}
	for _, position := range []int{0, len(portableMagic), len(raw) - 1} {
		corrupt := append([]byte(nil), raw...)
		corrupt[position] ^= 1
		tampered := filepath.Join(t.TempDir(), "tampered")
		if err := os.WriteFile(tampered, corrupt, 0600); err != nil {
			t.Fatal(err)
		}
		if err := ReadPortable(context.Background(), tampered, password, &output); err == nil {
			t.Fatal("tampered archive accepted", position)
		}
	}
}
func TestPortableCancellationLeavesNoPublishedArchive(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cancelled.blakeswap")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WritePortable(ctx, path, []byte("chosen backup password"), map[string]string{"state": "value"}); err == nil {
		t.Fatal("cancelled export published")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("partial export leaked", entries, err)
	}
}
