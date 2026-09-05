package desktop

import (
	"context"
	"encoding/json"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsPersistAndRejectStaleOrInvalidUpdates(t *testing.T) {
	root := t.TempDir()
	settings, err := loadSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ActiveNetwork != "mainnet" {
		t.Fatal("public mainnet is the default")
	}
	m := &Manager{root: root, settings: settings, engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}}
	next := proto.Clone(settings).(*pb.Settings)
	next.ActiveNetwork = "testnet"
	next.Environments[1].Nodes["blake"].Url = "ssl://my-indexer.example:50002"
	saved, err := m.writeSettings(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 2 {
		t.Fatal("revision not incremented")
	}
	disk, err := loadSettings(root)
	if err != nil || !proto.Equal(saved, disk) {
		t.Fatal("settings not persisted", err)
	}
	if _, err = m.writeSettings(context.Background(), next); status.Code(err) != codes.Aborted {
		t.Fatal("stale update accepted", err)
	}
	for name, mutate := range map[string]func(*pb.Settings){"duplicate network": func(s *pb.Settings) { s.Environments[1].Network = s.Environments[0].Network }, "plaintext public electrum": func(s *pb.Settings) { s.Environments[2].Nodes["btc"].Url = "tcp://public.example:50001" }, "unknown backend": func(s *pb.Settings) { s.Environments[2].Nodes["btc"].Kind = "magic" }, "bad relay": func(s *pb.Settings) { s.Environments[2].Relays = []string{"ws://public.example"} }, "missing chain": func(s *pb.Settings) { delete(s.Environments[0].Nodes, "blake") }} {
		t.Run(name, func(t *testing.T) {
			bad := proto.Clone(saved).(*pb.Settings)
			mutate(bad)
			if _, err := m.writeSettings(context.Background(), bad); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
	info, err := os.Stat(filepath.Join(root, "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("settings permissions", err)
	}
}
func TestOfflineActiveSwapCannotBeHiddenByNetworkChange(t *testing.T) {
	root := t.TempDir()
	settings := Defaults()
	m := &Manager{root: root, settings: settings, engines: map[string]*daemon.Engine{}}
	walletDir := filepath.Join(root, "wallets", "alice")
	seed, password, err := master(walletDir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(password)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	v, err := storage.Open(filepath.Join(walletDir, "mainnet", "state.db"), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Save(daemon.State{Version: 1, Network: chain.Mainnet, Mnemonic: seed, Swaps: map[string]*daemon.Swap{"pending": {ID: "pending", Stage: "funding broadcast"}}}); err != nil {
		t.Fatal(err)
	}
	v.Close()
	next := proto.Clone(settings).(*pb.Settings)
	next.ActiveNetwork = "testnet"
	if _, err = m.writeSettings(context.Background(), next); err == nil {
		t.Fatal("offline funded state bypassed network guard")
	}
	again, _, err := master(walletDir)
	if err != nil || seed != again {
		t.Fatal("profile master seed changed", err)
	}
}

func TestStatusAndSettingsStayReadableDuringExternalIO(t *testing.T) {
	m := &Manager{settings: Defaults(), engines: map[string]*daemon.Engine{}, lastError: "Connecting", restart: true}
	m.publishView()
	m.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := m.readSettings(context.Background())
		if err == nil {
			_, err = m.command(context.Background(), "alice", daemon.Request{Method: "status"})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		m.mu.Unlock()
		t.Fatal("read blocked on external IO")
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.writeSettings(ctx, proto.Clone(m.settings).(*pb.Settings)); err != context.Canceled {
		t.Fatal("cancelled settings mutation executed", err)
	}
}

func TestStaleNetworkMutationIsRejectedBeforeWalletLookup(t *testing.T) {
	m := &Manager{settings: Defaults(), engines: map[string]*daemon.Engine{}}
	for _, network := range []string{"", "regtest", "testnet"} {
		raw, _ := json.Marshal(map[string]any{"expected_network": network, "sell": "btc", "sell_amount": 1000000, "buy_amount": 2000000})
		_, err := m.command(context.Background(), "alice", daemon.Request{Method: "offer.create", Params: raw})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%q crossed network boundary: %v", network, err)
		}
	}
	raw := json.RawMessage(`{"expected_network":"mainnet"}`)
	_, err := m.command(context.Background(), "alice", daemon.Request{Method: "offer.create", Params: raw})
	if status.Code(err) != codes.Unavailable {
		t.Fatal("matching network did not reach wallet readiness check", err)
	}
}
