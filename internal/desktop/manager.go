// Package desktop owns the app daemon and wallet runtime. Chain backends are external.
package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Manager struct {
	mu         sync.Mutex
	view       atomic.Pointer[desktopView]
	root       string
	settings   *pb.Settings
	engines    map[string]*daemon.Engine
	configs    map[string]daemon.Config
	lastError  string
	restart    bool
	stopped    bool
	openings   map[string]*networkOpening
	runtimeCtx context.Context
	runtimeDir string
	servers    map[string]*api.Server
}

type networkOpening struct {
	cancel context.CancelFunc
	done   chan networkResult
}
type networkResult struct {
	manager *Manager
	err     error
}

// Immutable snapshots keep status and Settings readable while an external
// endpoint is slow. Protocol mutations remain serialized by mu.
type desktopView struct {
	settings *pb.Settings
	statuses map[string]json.RawMessage
}

func (m *Manager) publishView() {
	v := &desktopView{settings: proto.Clone(m.settings).(*pb.Settings), statuses: map[string]json.RawMessage{}}
	for _, wallet := range m.settings.Wallets {
		profile := wallet.Id
		s := daemon.Status{Name: profile, Mode: "trader", Network: chain.Network(m.settings.ActiveNetwork), LastError: m.lastError}
		if e := m.engines[profile]; e != nil && !m.restart {
			s = e.Status()
			if m.lastError != "" {
				s.LastError = m.lastError
			}
		}
		raw, err := json.Marshal(s)
		if err != nil {
			panic(err)
		} // Status consists solely of serializable public values.
		v.statuses[profile] = raw
	}
	m.view.Store(v)
}

// Run owns a per-installation lock. Parent death is checked in addition to
// signals so a forcibly killed GUI cannot orphan its wallet daemon.
func Run(ctx context.Context, root string, parent int) error {
	if !filepath.IsAbs(root) {
		return errors.New("absolute data path required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, "desktop.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("this Blakeswap data directory is already open")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if parent > 0 {
		go func() {
			tick := time.NewTicker(300 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if os.Getppid() != parent {
						cancel()
						return
					}
				}
			}
		}()
	}
	settings, err := loadSettings(root)
	if err != nil {
		return err
	}
	m := &Manager{root: root, settings: settings, engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}, restart: true}
	m.lastError = "Connecting"
	m.publishView()
	runtimeDir, err := os.MkdirTemp("", "blakeswap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runtimeDir)
	m.runtimeCtx, m.runtimeDir, m.servers = ctx, runtimeDir, map[string]*api.Server{}
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, server := range m.servers {
			server.Close()
		}
		_ = os.Remove(filepath.Join(root, "runtime.json"))
	}()
	for _, wallet := range settings.Wallets {
		if err := m.startAPI(wallet.Id); err != nil {
			return err
		}
	}
	if err := m.writeRuntime(); err != nil {
		return err
	}
	return m.run(ctx)
}
func (m *Manager) readSettings(ctx context.Context) (*pb.Settings, error) {
	if view := m.view.Load(); view != nil {
		return proto.Clone(view.settings).(*pb.Settings), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return proto.Clone(m.settings).(*pb.Settings), nil
}
func (m *Manager) writeSettings(ctx context.Context, next *pb.Settings) (*pb.Settings, error) {
	if err := validate(next); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if next.Revision != m.settings.Revision {
		return nil, status.Error(codes.Aborted, "settings changed; reload before saving")
	}
	if len(next.Wallets) != len(m.settings.Wallets) {
		return nil, status.Error(codes.InvalidArgument, "use CreateWallet to add wallets; removing wallets is not supported")
	}
	for i, profile := range next.Wallets {
		if profile.Id != m.settings.Wallets[i].Id {
			return nil, status.Error(codes.InvalidArgument, "wallet IDs and order cannot be changed")
		}
	}
	runtimeChanged := next.ActiveNetwork != m.settings.ActiveNetwork || !proto.Equal(environment(next, next.ActiveNetwork), environment(m.settings, m.settings.ActiveNetwork))
	// Release bootstrap vaults before changing their runtime or checking obligations.
	// Display-name edits need no engine restart or bootstrap cancellation.
	if runtimeChanged {
		m.stopOpening()
	}
	if next.ActiveNetwork != m.settings.ActiveNetwork {
		for _, wallet := range m.settings.Wallets {
			profile := wallet.Id
			if e := m.engines[profile]; e != nil {
				if err := e.CanChangeNetwork(); err != nil {
					return nil, err
				}
			} else {
				cfg := daemon.Config{DataDir: filepath.Join(m.root, "wallets", profile, m.settings.ActiveNetwork), PasswordFile: filepath.Join(m.root, "wallets", profile, "vault.password")}
				if err := daemon.CheckStoredNetwork(cfg); err != nil {
					return nil, err
				}
			}
		}
	}
	saved := proto.Clone(next).(*pb.Settings)
	saved.Revision++
	if err := saveSettings(m.root, saved); err != nil {
		return nil, err
	}
	m.settings = saved
	if runtimeChanged {
		m.restart = true
		m.lastError = "Connecting"
	}
	m.publishView()
	return proto.Clone(saved).(*pb.Settings), nil
}
func (m *Manager) command(ctx context.Context, profile string, req daemon.Request) (any, error) {
	if req.Method == "status" {
		if view := m.view.Load(); view != nil {
			return view.statuses[profile], nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.stopped {
		return nil, status.Error(codes.Unavailable, "daemon is stopping")
	}
	if err := daemon.CheckCommandNetwork(req, chain.Network(m.settings.ActiveNetwork), true); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	e := m.engines[profile]
	if e == nil || m.restart {
		if req.Method == "status" {
			return daemon.Status{Name: profile, Mode: "trader", Network: chain.Network(m.settings.ActiveNetwork), Addresses: map[chain.ID]string{}, Balances: map[chain.ID]int64{}, Heights: map[chain.ID]uint32{}, LastError: m.lastError}, nil
		}
		return nil, status.Error(codes.Unavailable, "wallet is connecting; check Settings and connection status")
	}
	if req.Method == "status" {
		s := e.Status()
		if m.lastError != "" {
			s.LastError = m.lastError
		}
		return s, nil
	}
	result, err := e.Command(ctx, req)
	m.publishView()
	return result, err
}
func (m *Manager) closeNetwork() {
	m.stopOpening()
	for _, e := range m.engines {
		_ = e.Close()
	}
	m.engines = map[string]*daemon.Engine{}
}

func (m *Manager) stopOpening() {
	for _, job := range m.openings {
		job.cancel()
	}
	for id, job := range m.openings {
		result := <-job.done
		result.manager.closeNetwork()
		delete(m.openings, id)
	}
}

// Each wallet bootstraps independently, including after a cold start. A slow
// history scan never holds back another wallet that is already ready to trade.
func (m *Manager) connect(ctx context.Context) {
	if m.openings == nil {
		m.openings = map[string]*networkOpening{}
	}
	for _, profile := range m.settings.Wallets {
		if m.engines[profile.Id] != nil {
			continue
		}
		if job := m.openings[profile.Id]; job != nil {
			select {
			case result := <-job.done:
				job.cancel()
				delete(m.openings, profile.Id)
				if result.err != nil {
					result.manager.closeNetwork()
					m.lastError = result.err.Error()
				} else {
					m.engines[profile.Id] = result.manager.engines[profile.Id]
					m.configs[profile.Id] = result.manager.configs[profile.Id]
				}
			default:
			}
			continue
		}
		pending := proto.Clone(m.settings).(*pb.Settings)
		pending.Wallets = []*pb.WalletProfile{proto.Clone(profile).(*pb.WalletProfile)}
		worker := &Manager{root: m.root, settings: pending, engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}}
		openingCtx, cancel := context.WithCancel(ctx)
		job := &networkOpening{cancel: cancel, done: make(chan networkResult, 1)}
		m.openings[profile.Id] = job
		m.lastError = "Connecting; RPC wallet history may still be synchronizing"
		go func() { err := worker.openNetwork(openingCtx); job.done <- networkResult{worker, err} }()
	}
}
func (m *Manager) run(ctx context.Context) error {
	defer func() { m.mu.Lock(); defer m.mu.Unlock(); m.stopped = true; m.closeNetwork() }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		m.mu.Lock()
		cycle, cancel := context.WithTimeout(ctx, 30*time.Second)
		if m.restart {
			m.closeNetwork()
			m.configs = map[string]daemon.Config{}
			m.restart = false
		}
		if len(m.engines) < len(m.settings.Wallets) || len(m.openings) > 0 {
			m.connect(ctx)
		}
		if len(m.engines) == len(m.settings.Wallets) && len(m.openings) == 0 {
			m.lastError = ""
		}
		// Each wallet gets the whole cycle budget. A slow provider for one wallet
		// must not consume the time available for the other wallets' settlements.
		var ticks sync.WaitGroup
		failures := make(chan error, len(m.engines))
		for _, wallet := range m.settings.Wallets {
			if e := m.engines[wallet.Id]; e != nil {
				ticks.Add(1)
				go func() {
					defer ticks.Done()
					if err := e.Tick(cycle); err != nil {
						failures <- err
					}
				}()
			}
		}
		ticks.Wait()
		close(failures)
		for err := range failures {
			m.lastError = err.Error()
		}
		cancel()
		m.publishView()
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(1500 * time.Millisecond):
		}
	}
}
func (m *Manager) openNetwork(ctx context.Context) error {
	env := environment(m.settings, m.settings.ActiveNetwork)
	if env == nil {
		return errors.New("missing active environment")
	}
	var tower daemon.TowerConfig
	if env.Tower != nil {
		tower = daemon.TowerConfig{PubKey: env.Tower.Pubkey, BPS: env.Tower.Bps, Scripts: map[chain.ID]string{}}
		for id, script := range env.Tower.Scripts {
			tower.Scripts[chain.ID(id)] = script
		}
	}
	for _, wallet := range m.settings.Wallets {
		profile := wallet.Id
		cfg, err := m.config(profile, env)
		if err != nil {
			return err
		}
		cfg.Tower = tower
		m.configs[profile] = cfg
		if m.engines[profile] != nil {
			continue
		}
		e, err := daemon.Open(ctx, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", profile, err)
		}
		m.engines[profile] = e
	}
	m.lastError = ""
	return nil
}
func (m *Manager) config(profile string, env *pb.Environment) (daemon.Config, error) {
	walletDir := filepath.Join(m.root, "wallets", profile)
	mnemonic, password, err := master(walletDir)
	if err != nil {
		return daemon.Config{}, err
	}
	c := daemon.Config{PublicWatchtower: env.PublicWatchtower, FavoriteWatchtowers: append([]string(nil), env.FavoriteWatchtowers...), Name: profile, Mode: "trader", Network: chain.Network(env.Network), InitialMnemonic: mnemonic, DataDir: filepath.Join(walletDir, env.Network), PasswordFile: password, Relays: append([]string(nil), env.Relays...), Nodes: map[chain.ID]daemon.NodeConfig{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		n := env.Nodes[string(id)]
		if n == nil || n.Url == "" {
			return c, fmt.Errorf("configure a %s %s endpoint in Settings", env.Network, id)
		}
		c.Nodes[id] = daemon.NodeConfig{Kind: n.Kind, URL: n.Url, Cookie: n.Cookie, CertificateSHA256: n.CertificateSha256}
	}
	return c, nil
}
func master(root string) (string, string, error) {
	passwordPath := filepath.Join(root, "vault.password")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", "", err
	}
	password, err := os.ReadFile(passwordPath)
	if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			return "", "", err
		}
		password = []byte(hex.EncodeToString(secret[:]))
		if err = writePrivate(passwordPath, password); err != nil {
			return "", "", err
		}
	}
	if err != nil {
		return "", "", err
	}
	defer clear(password)
	vault, err := storage.Open(filepath.Join(root, "master.db"), bytes.TrimSpace(password))
	if err != nil {
		return "", "", err
	}
	defer vault.Close()
	var state struct {
		Mnemonic string `json:"mnemonic"`
	}
	if _, err = vault.Load(&state); err != nil {
		return "", "", err
	}
	if state.Mnemonic == "" {
		state.Mnemonic, err = wallet.NewMnemonic()
		if err != nil {
			return "", "", err
		}
		if err = vault.Save(state); err != nil {
			return "", "", err
		}
	}
	return state.Mnemonic, passwordPath, nil
}
