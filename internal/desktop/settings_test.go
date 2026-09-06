package desktop

import (
	"context"
	"encoding/json"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
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
	settings.OnboardingStage = "" // This test exercises an already configured installation.
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
	settings := configuredDefaults()
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
	m := &Manager{settings: configuredDefaults(), engines: map[string]*daemon.Engine{}, lastError: "Connecting", restart: true}
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
	m := &Manager{settings: configuredDefaults(), engines: map[string]*daemon.Engine{}}
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

// A slow initial node rescan must neither block Settings reads nor retain a
// vault lock when a user switches endpoints or the application closes.
func TestSettingsCancelsBootstrapBeforeInspectingStoredObligations(t *testing.T) {
	root := t.TempDir()
	settings := configuredDefaults()
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
	vault, err := storage.Open(filepath.Join(walletDir, "mainnet", "state.db"), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(daemon.State{Version: 1, Network: chain.Mainnet, Mnemonic: seed, Swaps: map[string]*daemon.Swap{"pending": {ID: "pending", Stage: "funding broadcast"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &networkOpening{cancel: cancel, done: make(chan networkResult, 1)}
	m.openings = map[string]*networkOpening{"alice": job}
	go func() {
		<-ctx.Done()
		_ = vault.Close()
		job.done <- networkResult{manager: &Manager{engines: map[string]*daemon.Engine{}}, err: ctx.Err()}
	}()
	m.publishView()
	if _, err := m.readSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := proto.Clone(settings).(*pb.Settings)
	next.ActiveNetwork = "testnet"
	if _, err = m.writeSettings(context.Background(), next); err == nil {
		t.Fatal("bootstrap concealed an outstanding swap")
	}
	if len(m.openings) != 0 || ctx.Err() == nil {
		t.Fatal("bootstrap not cancelled before checking stored state")
	}
	// A second open proves that cancellation waited for the bootstrap's vault.
	reopened, err := storage.Open(filepath.Join(walletDir, "mainnet", "state.db"), raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
}

func TestWatchtowerPrivacyAndFavoritesPersistPerNetwork(t *testing.T) {
	settings := configuredDefaults()
	for _, env := range settings.Environments {
		if env.PublicWatchtower {
			t.Fatal("watchtower public by default")
		}
	}
	key := nostr.Generate().Public()
	env := environment(settings, "mainnet")
	env.PublicWatchtower = true
	env.FavoriteWatchtowers = []string{key.Hex()}
	if err := validate(settings); err != nil {
		t.Fatal(err)
	}
	if env.FavoriteWatchtowers[0] != nip19.EncodeNpub(key) {
		t.Fatal("favorite not normalized to npub")
	}
	root := t.TempDir()
	if err := saveSettings(root, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(root)
	if err != nil || !proto.Equal(loaded, settings) {
		t.Fatal("watchtower preferences not persisted", err)
	}
	if environment(loaded, "regtest").PublicWatchtower || len(environment(loaded, "regtest").FavoriteWatchtowers) != 0 {
		t.Fatal("preferences crossed networks")
	}
	env.FavoriteWatchtowers = append(env.FavoriteWatchtowers, key.Hex())
	if validate(settings) == nil {
		t.Fatal("duplicate favorite identity accepted")
	}
	env.FavoriteWatchtowers = []string{"not-an-npub"}
	if validate(settings) == nil {
		t.Fatal("invalid favorite accepted")
	}
}

func TestLegacyWalletMigrationPreservesVaults(t *testing.T) {
	for _, hasBob := range []bool{false, true} {
		root := t.TempDir()
		legacy := configuredDefaults()
		legacy.Wallets = nil
		if err := saveSettings(root, legacy); err != nil {
			t.Fatal(err)
		}
		aliceSeed, _, err := master(filepath.Join(root, "wallets", "alice"))
		if err != nil {
			t.Fatal(err)
		}
		var bobSeed string
		if hasBob {
			bobSeed, _, err = master(filepath.Join(root, "wallets", "bob"))
			if err != nil {
				t.Fatal(err)
			}
		}
		migrated, err := loadSettings(root)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if hasBob {
			want++
		}
		if len(migrated.Wallets) != want || migrated.Wallets[0].Id != "alice" {
			t.Fatal("lost existing profiles")
		}
		if seed, _, err := master(filepath.Join(root, "wallets", "alice")); err != nil || seed != aliceSeed {
			t.Fatal("replaced Alice seed")
		}
		if hasBob {
			if seed, _, err := master(filepath.Join(root, "wallets", "bob")); err != nil || seed != bobSeed {
				t.Fatal("replaced Bob seed")
			}
		}
		again, err := loadSettings(root)
		if err != nil || !proto.Equal(again, migrated) {
			t.Fatal("migration is not stable")
		}
	}
}
func TestWalletIDsCannotBeChangedOrDeletedBySettings(t *testing.T) {
	m := &Manager{root: t.TempDir(), settings: configuredDefaults()}
	for _, change := range []func(*pb.Settings){
		func(s *pb.Settings) { s.Wallets[0].Id = "../../outside" },
		func(s *pb.Settings) { s.Wallets[0].Id = "different" },
		func(s *pb.Settings) { s.Wallets = nil },
		func(s *pb.Settings) { s.Wallets = append(s.Wallets, &pb.WalletProfile{Id: "another", Name: "Another"}) },
		func(s *pb.Settings) { s.Wallets[0].Name = " " },
	} {
		next := proto.Clone(m.settings).(*pb.Settings)
		change(next)
		if _, err := m.writeSettings(context.Background(), next); err == nil {
			t.Fatal("invalid wallet update accepted")
		}
	}
}

func TestRenameAndInactiveSettingsDoNotRestartWallets(t *testing.T) {
	m := &Manager{root: t.TempDir(), settings: configuredDefaults()}
	next := proto.Clone(m.settings).(*pb.Settings)
	next.Wallets[0].Name = "Savings"
	saved, err := m.writeSettings(context.Background(), next)
	if err != nil || m.restart || saved.Wallets[0].Name != "Savings" {
		t.Fatal("rename reconnected wallet", err)
	}
	next = proto.Clone(saved).(*pb.Settings)
	environment(next, "regtest").PublicWatchtower = true
	if _, err := m.writeSettings(context.Background(), next); err != nil || m.restart {
		t.Fatal("inactive network settings reconnected active wallet", err)
	}
}

func TestReadyWalletStartsWhileAnotherBootstrapIsBlocked(t *testing.T) {
	settings := configuredDefaults()
	settings.Wallets = append(settings.Wallets, &pb.WalletProfile{Id: "savings", Name: "Savings"})
	ready, blocked := &networkOpening{cancel: func() {}, done: make(chan networkResult, 1)}, &networkOpening{cancel: func() {}, done: make(chan networkResult, 1)}
	engine := &daemon.Engine{} // Identity-only sentinel; no chain calls are needed.
	ready.done <- networkResult{manager: &Manager{engines: map[string]*daemon.Engine{"alice": engine}, configs: map[string]daemon.Config{"alice": {Name: "alice"}}}}
	m := &Manager{settings: settings, engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}, openings: map[string]*networkOpening{"alice": ready, "savings": blocked}}
	m.connect(context.Background())
	if m.engines["alice"] != engine || m.openings["savings"] != blocked {
		t.Fatal("ready engine held until another wallet's rescan finishes")
	}
	blocked.done <- networkResult{manager: &Manager{engines: map[string]*daemon.Engine{}}, err: context.DeadlineExceeded}
	m.connect(context.Background())
	if m.engines["alice"] != engine || len(m.openings) != 0 {
		t.Fatal("failed bootstrap discarded another wallet's running engine")
	}
}

func configuredDefaults() *pb.Settings { s := Defaults(); s.OnboardingStage = ""; return s }

func TestRescueFeePersistsPerNetworkAndReachesEveryWallet(t *testing.T) {
	m := &Manager{root: t.TempDir(), settings: configuredDefaults()}
	next := proto.Clone(m.settings).(*pb.Settings)
	environment(next, "regtest").RescueFeeBps = 125
	saved, err := m.writeSettings(context.Background(), next)
	if err != nil || m.restart {
		t.Fatal("inactive rescue fee update", err)
	}
	loaded, err := loadSettings(m.root)
	if err != nil || !proto.Equal(saved, loaded) {
		t.Fatal("rescue fee not persisted", err)
	}
	if environment(loaded, "mainnet").RescueFeeBps != 0 {
		t.Fatal("fee crossed networks")
	}
	for _, profile := range []string{"alice", "savings"} {
		cfg, err := m.config(profile, environment(loaded, "regtest"))
		if err != nil || cfg.RescueFeeBPS != 125 || cfg.Tower.BPS != 0 {
			t.Fatal("own rate not isolated from selected tower", err)
		}
	}
	for _, bps := range []int64{-1, 1001} {
		bad := proto.Clone(saved).(*pb.Settings)
		environment(bad, "mainnet").RescueFeeBps = bps
		if _, err := m.writeSettings(context.Background(), bad); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid fee accepted", err)
		}
	}
	for _, bps := range []int64{1, 1000, 0} {
		next := proto.Clone(m.settings).(*pb.Settings)
		environment(next, "mainnet").RescueFeeBps = bps
		if _, err := m.writeSettings(context.Background(), next); err != nil || !m.restart {
			t.Fatal("active fee did not reconnect", err)
		}
		m.restart = false
	}
}

func TestBootstrapPublishesIndependentChainHeightsAndClearsStaleProgress(t *testing.T) {
	job := &networkOpening{}
	m := &Manager{settings: configuredDefaults(), openings: map[string]*networkOpening{"alice": job}}
	read := func() daemon.Status {
		t.Helper()
		m.publishView()
		var s daemon.Status
		if err := json.Unmarshal(m.view.Load().statuses["alice"], &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if len(read().Heights) != 0 {
		t.Fatal("unconnected chains reported a height")
	}
	job.chainReady(chain.BTC, 123)
	first := read()
	if first.Heights[chain.BTC] != 123 || len(first.Heights) != 1 {
		t.Fatal("BTC readiness waited for BLAKE")
	}
	job.chainReady(chain.Blake, 456)
	if len(first.Heights) != 1 || read().Heights[chain.Blake] != 456 {
		t.Fatal("progress snapshot was mutated or lost")
	}
	m.restart = true
	if len(read().Heights) != 0 {
		t.Fatal("old heights survived settings change")
	}
	m.restart = false
	delete(m.openings, "alice")
	if len(read().Heights) != 0 {
		t.Fatal("failed bootstrap retained heights")
	}
}
