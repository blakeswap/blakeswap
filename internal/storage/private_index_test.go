package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateIndexExactOwnershipRollbackAndCleanup(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	index, err := NewPrivateIndex(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	// Cross several bounded initialization batches without keeping the rows.
	for start := 0; start < 257; start += 64 {
		tx, err := index.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := start; i < min(start+64, 257); i++ {
			if err := tx.Assign(fmt.Sprintf("exact-secret-point/%03d", i), fmt.Sprintf("owner/%03d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := index.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 63, 64, 256} {
		value, found, err := tx.Get(fmt.Sprintf("exact-secret-point/%03d", i))
		if err != nil || !found || string(value) != fmt.Sprintf("owner/%03d", i) {
			t.Fatal("exact lookup", i, found, err)
		}
		clear(value)
	}
	if err := tx.Assign("exact-secret-point/000", "different owner"); err == nil {
		t.Fatal("conflict accepted")
	}
	if err := tx.Release("missing", "owner/000"); err == nil {
		t.Fatal("missing old authority accepted")
	}
	if err := tx.Release("exact-secret-point/000", "owner/000"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Assign("exact-secret-point/000", "replacement"); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	tx, err = index.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := tx.Get("exact-secret-point/000")
	if err != nil || !found || string(value) != "owner/000" {
		t.Fatal("rollback changed published cache", found, err)
	}
	clear(value)
	tx.Rollback()
	data, err := os.ReadFile(filepath.Join(index.dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("exact-secret-point/")) || bytes.Contains(data, []byte("owner/000")) {
		t.Fatal("private index contains plaintext authority")
	}
	clear(data)
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(index.key, make([]byte, 32)) {
		t.Fatal("index key retained")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("index files retained", err)
	}
}

func TestPrivateIndexCancellationCorruptionAndVaultLifetime(t *testing.T) {
	ctx := context.Background()
	v, err := Open(filepath.Join(t.TempDir(), "source.db"), []byte("isolated private index lifetime"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	index, err := NewPrivateIndex(ctx, v.PrivateDirectory())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.OwnPrivateIndex(index); err != nil {
		t.Fatal(err)
	}
	pending, cancel := context.WithCancel(ctx)
	tx, err := index.Begin(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Assign("point", "owner"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Commit(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled transaction published", err)
	}
	tx, err = index.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if value, found, err := tx.Get("point"); err != nil || found || len(value) != 0 {
		t.Fatal("cancelled ownership persisted", found, err)
	}
	if err := tx.Assign("point", "owner"); err != nil {
		t.Fatal(err)
	}
	address := tx.address("point")
	sealed := bytes.Clone(tx.tx.Bucket(privateIndexBucket).Get(address))
	sealed[len(sealed)-1] ^= 1
	if err := tx.tx.Bucket(privateIndexBucket).Put(address, sealed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.Get("point"); err == nil {
		t.Fatal("tampered exact entry accepted")
	}
	tx.Rollback()
	dir := index.dir
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("vault close retained private index", err)
	}
	if _, err := index.Begin(ctx); err == nil {
		t.Fatal("closed index reused")
	}
}
