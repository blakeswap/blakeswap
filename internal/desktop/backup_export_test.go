package desktop

import (
	"context"
	"github.com/blakeswap/blakeswap/internal/storage"
	"os"
	"path/filepath"
	"testing"
)

func TestPortableExportFromExistingProfile(t *testing.T) {
	m := setupManager(t)
	prepared := prepare(t, m)
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	password := "independent chosen backup password"
	result, err := m.exportPortable(context.Background(), "alice", path, password, false)
	if err != nil || result.Path != path || result.Wallets != 1 || result.Networks != 3 || result.ReminderWarning != "" {
		t.Fatal(result, err)
	}
	var restored backupManifest
	if err := storage.ReadPortable(context.Background(), path, []byte(password), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Wallets[0].Mnemonic != prepared.Recovery.Mnemonic || len(restored.Wallets[0].Networks) != 3 {
		t.Fatal("portable export lost wallet or network state")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.exportPortable(context.Background(), "alice", path, password, false); err == nil {
		t.Fatal("export overwrote destination")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing backup changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := filepath.Join(t.TempDir(), "cancelled.blakeswap")
	if _, err := m.exportPortable(ctx, "alice", canceled, password, false); err == nil {
		t.Fatal("cancelled export published")
	}
	if _, err := os.Stat(canceled); !os.IsNotExist(err) {
		t.Fatal("cancelled archive exists", err)
	}
}
