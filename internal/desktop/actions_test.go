package desktop

import (
	"context"
	"encoding/json"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActionSummaryAllWalletsAndOpeningNotSelectedPage(t *testing.T) {
	now := time.Now().Unix()
	raw := func(id string, actions []daemon.WalletAction) json.RawMessage {
		b, _ := json.Marshal(daemon.Status{Actions: daemon.WalletActions{WalletID: id, Network: chain.Regtest, Known: true, ObservedAt: now, Source: "live", Actions: actions}})
		return b
	}
	m := &Manager{}
	view := &desktopView{settings: &pb.Settings{ActiveNetwork: "regtest", Revision: 8, Wallets: []*pb.WalletProfile{{Id: "selected"}, {Id: "tower"}, {Id: "opening"}}}, statuses: map[string]json.RawMessage{"selected": raw("selected", nil), "tower": raw("tower", []daemon.WalletAction{{ID: "tower/accepted", Kind: "tower", State: "tower_monitoring", RequiresMonitoring: true}})}, workers: map[string]*walletWorker{}}
	m.view.Store(view)
	result, err := m.command(context.Background(), "selected", daemon.Request{Method: "actions.summary", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	s := result.(daemon.ActionSummary)
	if len(s.Wallets) != 3 || s.Complete || !s.RequiresMonitoring || s.SettingsRevision != 8 {
		t.Fatal(s)
	}
	if len(s.Wallets[1].Actions) != 1 || s.Wallets[2].Known {
		t.Fatal("accepted job or unknown wallet hidden", s)
	}
}
func TestActionSummaryCommandBoundaryAndNonblockingRefresh(t *testing.T) {
	now := time.Now().Unix()
	m := &Manager{}
	raw, _ := json.Marshal(daemon.Status{Actions: daemon.WalletActions{WalletID: "a", Network: chain.Regtest, Known: true, Source: "live", ObservedAt: now}})
	snapshot := json.RawMessage(raw)
	w := &walletWorker{refresh: make(chan chan refreshResult, 1)}
	w.snapshot.Store(&snapshot)
	v := &desktopView{settings: &pb.Settings{ActiveNetwork: "regtest", Wallets: []*pb.WalletProfile{{Id: "a"}}}, workers: map[string]*walletWorker{"a": w}}
	m.view.Store(v)
	check := func(refresh bool) daemon.ActionSummary {
		raw, _ := json.Marshal(map[string]bool{"refresh": refresh})
		s, err := m.actionSummary(context.Background(), daemon.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := check(false); !s.Complete || s.RequiresMonitoring {
		t.Fatal(s)
	}
	m.actionCommands.Add(1)
	if s := check(false); s.Complete || !s.RequiresMonitoring {
		t.Fatal("queued authorization hidden", s)
	}
	m.actionCommands.Add(-1)
	w.busy.Add(1)
	if s := check(false); s.Complete {
		t.Fatal("inflight worker hidden", s)
	}
	w.busy.Add(-1)
	// No worker is consuming refreshes. Both reads must return immediately,
	// including when the bounded queue is full, without waiting for chain IO.
	for i := 0; i < 2; i++ {
		if s := check(true); !s.Complete {
			t.Fatal(s)
		}
	}
	if len(w.refresh) != 1 {
		t.Fatal("refresh was not queued")
	}
}

type actionRuntime struct {
	mu      sync.Mutex
	current daemon.Status
}

func (f *actionRuntime) Tick(context.Context) error { return nil }
func (f *actionRuntime) Status() daemon.Status      { f.mu.Lock(); defer f.mu.Unlock(); return f.current }
func TestActionTradeConfirmationPublishesBeforeCommandBoundaryRetires(t *testing.T) {
	now := time.Now().Unix()
	runtime := &actionRuntime{current: daemon.Status{Actions: daemon.WalletActions{WalletID: "a", Network: chain.Regtest, Source: "live", Known: true, ObservedAt: now}}}
	started, release := make(chan struct{}), make(chan struct{})
	worker := &walletWorker{ctx: context.Background(), engine: runtime, refresh: make(chan chan refreshResult, 1)}
	worker.command = func(context.Context, daemon.Request) (any, error) {
		close(started)
		<-release
		runtime.mu.Lock()
		runtime.current.Actions.Actions = []daemon.WalletAction{{ID: "swap/accepted", Kind: "swap", RequiresMonitoring: true}}
		runtime.mu.Unlock()
		return true, nil
	}
	worker.capture(runtime, nil)
	m := &Manager{settings: &pb.Settings{ActiveNetwork: "regtest", Wallets: []*pb.WalletProfile{{Id: "a"}}}, workers: map[string]*walletWorker{"a": worker}}
	m.view.Store(&desktopView{settings: m.settings, workers: m.workers})
	done := make(chan error, 1)
	go func() {
		_, err := m.command(context.Background(), "a", daemon.Request{Method: "trade.confirm", Params: json.RawMessage(`{"expected_network":"regtest"}`)})
		done <- err
	}()
	<-started
	during, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if during.Complete || !during.RequiresMonitoring {
		t.Fatal("accepted command disappeared behind old empty snapshot", during)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Complete || !after.RequiresMonitoring || len(after.Wallets[0].Actions) != 1 {
		t.Fatal("new authority was not published before idle", after)
	}
}

func TestActionDurableFailedImportIsNotKnownEmptyAtQuit(t *testing.T) {
	m := installedManager(t)
	network := chain.Network(m.settings.ActiveNetwork)
	m.storedActions = map[string]daemon.WalletActions{"alice": {WalletID: "alice", Network: network, Known: true, Source: "stored", ObservedAt: time.Now().Unix()}}
	m.publishView()
	before, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil || !before.Complete || before.RequiresMonitoring {
		t.Fatalf("precondition: %+v %v", before, err)
	}
	manifest := portableManifest(t)
	path := filepath.Join(t.TempDir(), "restore.blakeswap")
	const password = "independent portable review password"
	if err := storage.WritePortable(context.Background(), path, []byte(password), manifest); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(m.root, "settings.json")
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(settingsPath, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = m.importPortableAPI(context.Background(), &pb.ImportBackupRequest{Path: path, Password: password, Revision: m.settings.Revision})
	if err == nil || !strings.Contains(err.Error(), "recovery wallet installed") {
		t.Fatalf("expected durable post-install error, got %v", err)
	}
	markers, err := filepath.Glob(filepath.Join(m.root, "wallets", "wallet-*", "import.json"))
	if err != nil || len(markers) != 1 {
		t.Fatalf("durable marker not retained: %v %v", markers, err)
	}
	installedRoot := filepath.Dir(markers[0])
	w, err := daemon.LoadStoredActions(daemon.Config{Name: filepath.Base(installedRoot), Network: network, DataDir: filepath.Join(installedRoot, string(network)), PasswordFile: filepath.Join(installedRoot, "vault.password")})
	if err != nil || !daemon.SummarizeActions(network, 0, []daemon.WalletActions{w}, time.Now().Unix()).RequiresMonitoring {
		t.Fatalf("installed gated obligations missing: %+v %v", w, err)
	}
	after, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !after.InstallationPending || after.Complete || !after.RequiresMonitoring {
		t.Fatalf("durable funded import omitted from quit summary after error: %+v", after)
	}
	// A failed retry cannot clear an earlier durable publication hold.
	_, _ = m.importPortableAPI(context.Background(), &pb.ImportBackupRequest{Path: path, Password: "wrong", Revision: m.settings.Revision})
	held, _ := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if !held.InstallationPending {
		t.Fatal("failed retry cleared durable hold")
	}
	// Restart authenticates the retained marker and all gated network vaults
	// before the new manager exists. The hold becomes actual listed obligations.
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(m.root, m.settings); err != nil {
		t.Fatal(err)
	}
	recovered, err := loadSettings(m.root)
	if err != nil || len(recovered.Wallets) != 2 {
		t.Fatal("restart failed to publish durable profile", recovered, err)
	}
	restarted := &Manager{root: m.root, settings: recovered, storedActions: map[string]daemon.WalletActions{"alice": m.storedActions["alice"], w.WalletID: w}}
	restarted.publishView()
	current, err := restarted.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil || current.InstallationPending || !current.Complete || !current.RequiresMonitoring {
		t.Fatal("restart lost recovered obligations", current, err)
	}

}

type actionGateContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *actionGateContext) Err() error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Err()
}
func TestActionFirstWalletRestoreProtectsQuitDuringDirectRPC(t *testing.T) {
	m := setupManager(t)
	m.publishView()
	request := daemon.Request{Params: json.RawMessage(`{}`)}
	before, err := m.actionSummary(context.Background(), request)
	if err != nil || !before.Complete || before.RequiresMonitoring {
		t.Fatal("not initially empty", before, err)
	}
	state := daemon.State{Version: daemon.StateVersion, Network: chain.Mainnet, Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"}
	child := fixtureSwap(t, chain.Mainnet, state.Mnemonic, "taker")
	child.Stage, child.Secret = "funding broadcast", "test-only-secret"
	state.Swaps = map[string]*daemon.Swap{child.ID: child}
	fixtureIndexSwaps(t, &state)
	backup := t.TempDir() + "/old-wallet.db"
	if err := saveVault(backup, []byte("isolated-restore-test-password"), state); err != nil {
		t.Fatal(err)
	}
	gated := &actionGateContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := m.prepareFirstWallet(gated, &pb.PrepareFirstWalletRequest{Name: "Restored", BackupPath: backup, BackupPassword: "isolated-restore-test-password", Revision: m.settings.Revision})
		done <- err
	}()
	<-gated.entered
	during, readErr := m.actionSummary(context.Background(), request)
	close(gated.release)
	if err := <-done; err != nil {
		t.Fatal("restore failed", err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !during.InstallationPending || during.Complete || !during.RequiresMonitoring {
		t.Fatal("direct restore RPC disappeared behind old known-empty view", during)
	}
}

func TestActionDirectSettingsMutationCannotUseOldEmptyView(t *testing.T) {
	m := installedManager(t)
	network := chain.Network(m.settings.ActiveNetwork)
	m.storedActions = map[string]daemon.WalletActions{"alice": {WalletID: "alice", Network: network, Known: true, Source: "stored", ObservedAt: time.Now().Unix()}}
	m.publishView()
	next := proto.Clone(m.settings).(*pb.Settings)
	next.Wallets[0].Name = "Renamed"
	gated := &actionGateContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := m.writeSettings(gated, next); done <- err }()
	<-gated.entered
	during, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	close(gated.release)
	if writeErr := <-done; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || during.Complete || !during.RequiresMonitoring {
		t.Fatal("direct settings mutation hidden", during, err)
	}
	after, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage(`{}`)})
	if err != nil || !after.Complete || after.RequiresMonitoring || after.InstallationPending {
		t.Fatal("completed empty mutation retained false obligation", after, err)
	}
}
