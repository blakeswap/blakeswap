package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

func backupAPIManager(t *testing.T) *Manager {
	t.Helper()
	m := installedManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.runtimeCtx, m.runtimeDir, m.servers = ctx, t.TempDir(), map[string]*api.Server{}
	if err := m.startAPI("alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.writeRuntime(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		for _, server := range m.servers {
			server.Close()
		}
	})
	return m
}
func backupAPICall(t *testing.T, m *Manager, method string, params any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return api.Call(context.Background(), m.servers["alice"].Endpoint.Socket, daemon.Request{Method: method, Params: raw})
}
func TestPortableBackupThroughTypedAPI(t *testing.T) {
	source, dest := backupAPIManager(t), backupAPIManager(t)
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	password := "typed portable export password"
	raw, err := backupAPICall(t, source, "backup.export", map[string]any{"path": path, "password": password, "all_wallets": false})
	if err != nil {
		t.Fatal(err)
	}
	var exported portableExportResult
	if err = json.Unmarshal(raw, &exported); err != nil || exported.Networks != 3 || exported.Wallets != 1 {
		t.Fatal("wrong export scope", string(raw), err)
	}
	before, _ := os.ReadFile(path)
	if _, err = backupAPICall(t, dest, "backup.inspect", map[string]string{"path": path, "password": "wrong backup password"}); err == nil {
		t.Fatal("incorrect password accepted over API")
	}
	raw, err = backupAPICall(t, dest, "backup.inspect", map[string]string{"path": path, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	var contents struct {
		Wallets []struct {
			SourceWalletID string `json:"source_wallet_id"`
		}
	}
	if err = json.Unmarshal(raw, &contents); err != nil || len(contents.Wallets) != 1 {
		t.Fatal("invalid inspection", string(raw), err)
	}
	request := &pb.ImportBackupRequest{Path: path, Password: password, SourceWalletId: contents.Wallets[0].SourceWalletID, Name: "Restored", Revision: dest.settings.Revision}
	raw, err = backupAPICall(t, dest, "backup.import", request)
	if err != nil {
		t.Fatal(err)
	}
	var imported portableImportResult
	if err = json.Unmarshal(raw, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.ProfileID == "alice" || len(dest.settings.Wallets) != 2 || dest.servers[imported.ProfileID] == nil {
		t.Fatal("typed import did not install isolated profile")
	}
	root := filepath.Join(dest.root, "wallets", imported.ProfileID)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		_, key, err := readMaster(root)
		if err != nil {
			t.Fatal(err)
		}
		state, err := readStateBackup(dest.root, filepath.Join(root, string(network), "state.db"), string(key))
		clear(key)
		if err != nil || state.Recovery == nil || state.Recovery.Status.State != "recovering" {
			t.Fatal("API published an ungated import", err)
		}
	}
	request.Revision = dest.settings.Revision
	if _, err = backupAPICall(t, dest, "backup.import", request); err == nil {
		t.Fatal("typed duplicate import accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("API import modified archive")
	}
}
