package credential

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitFileProviderRejectsUnsafeFilesAndReturnsOwnedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "operator.password")
	want := []byte("isolated synthetic credential\n")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := File(path).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clear(a)
	b, err := File(path).Acquire(context.Background())
	if err != nil || string(b) != "isolated synthetic credential" {
		t.Fatal("credential acquisition aliased caller storage", err)
	}
	clear(b)
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := File(link).Acquire(context.Background()); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := File(path).Acquire(context.Background()); err == nil {
		t.Fatal("public credential accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := File(path).Acquire(ctx); err == nil {
		t.Fatal("cancelled acquisition succeeded")
	}
}
