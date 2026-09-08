package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type capturedNetwork struct {
	wallet   int
	network  chain.Network
	engine   *daemon.Engine
	vault    *storage.Vault
	empty    *daemon.State
	password []byte
	path     string
	page     backupSourceView
	release  func()
	cloned   bool
}
type portableCapture struct {
	manifest backupManifest
	staging  *portableStaging
	sources  []*capturedNetwork
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

// Completion never takes either lifecycle lock: reconfiguration can cancel and
// join the source read while holding Manager.mu, and Engine.Close can join its
// fallback lease without deadlocking on the export's reminder publication.
func (c *portableCapture) closeSources() {
	c.once.Do(func() {
		for _, source := range c.sources {
			if source.release != nil {
				source.release()
			}
			if source.vault != nil {
				_ = source.vault.Close()
			}
		}
		c.cancel()
		close(c.done)
	})
}

func (m *Manager) backupSnapshot(ctx context.Context, selected string, all bool) (backupManifest, error) {
	m.mu.Lock()
	capture, err := m.captureBackupLocked(ctx, selected, all)
	m.mu.Unlock()
	if err != nil {
		return backupManifest{}, err
	}
	return capture.materialize()
}

// Credentials, inactive KDFs and token validation happen while workers keep
// running. All selected sources are then captured in one common quiescent
// interval. Full archive validation, hashing and fallback IO run after resume.
func (m *Manager) captureBackupLocked(ctx context.Context, selected string, all bool) (*portableCapture, error) {
	if m.stopped {
		return nil, errors.New("daemon is stopping")
	}
	found := false
	for _, profile := range m.settings.Wallets {
		if profile.Id == selected {
			found = true
		}
	}
	if !found {
		return nil, errors.New("wallet profile does not exist")
	}
	// Join opening engines and any preceding fallback before opening inactive DBs.
	// Existing wallet workers continue to settle while these preparations run.
	m.stopOpening()
	staging, err := newPortableStaging(m.root)
	if err != nil {
		return nil, err
	}
	sourceCtx, cancel := context.WithCancel(ctx)
	c := &portableCapture{manifest: backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), release: staging.close}, staging: staging, ctx: sourceCtx, cancel: cancel, done: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			c.closeSources()
			staging.close()
		}
	}()
	for _, profile := range m.settings.Wallets {
		if !all && profile.Id != selected {
			continue
		}
		if err := sourceCtx.Err(); err != nil {
			return nil, err
		}
		root := filepath.Join(m.root, "wallets", profile.Id)
		seed, password, err := m.readMaster(root)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer clear(password)
			identity, err := backupIdentity(seed)
			if err != nil {
				return err
			}
			index := len(c.manifest.Wallets)
			c.manifest.Wallets = append(c.manifest.Wallets, backupWallet{ID: profile.Id, Name: profile.Name, Mnemonic: seed, Identity: identity, Networks: map[chain.Network]*daemon.State{}, sources: map[chain.Network]*backupNetwork{}, marks: map[chain.Network]backupMark{}})
			for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
				if err := sourceCtx.Err(); err != nil {
					return err
				}
				source := &capturedNetwork{wallet: index, network: network, path: filepath.Join(staging.root, fmt.Sprintf("clone-%d.db", len(c.sources)))}
				c.sources = append(c.sources, source)
				c.manifest.Wallets[index].Networks[network] = nil
				source.password = bytes.Clone(password)
				staging.clonePasswords = append(staging.clonePasswords, source.password)
				if engine := m.engines[profile.Id]; engine != nil && engine.Config.Network.Normalized() == network {
					source.engine = engine
					continue
				}
				path := filepath.Join(root, string(network), "state.db")
				if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
					source.empty = &daemon.State{Version: 1, Network: network, Mnemonic: seed}
					normalizeState(source.empty)
					continue
				} else if statErr != nil {
					return statErr
				}
				source.vault, err = storage.Open(path, source.password)
				if err != nil {
					return err
				}
				var active daemon.State
				if _, err = source.vault.Load(&active); err != nil {
					return err
				}
				if active.Mnemonic != seed || active.Network.Normalized() != network {
					return errors.New("backup network state does not match its wallet")
				}
				changed, err := daemon.PrepareStoredBackupToken(&active)
				if err != nil {
					return err
				}
				if changed {
					if err = source.vault.Save(active); err != nil {
						return err
					}
				}
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	if err := validateBackupInventory(c.manifest); err != nil {
		return nil, err
	}
	paused := time.Now()
	m.stopWorkers()
	defer func() {
		if m.runtimeCtx != nil && !m.stopped {
			m.startWorkers(m.runtimeCtx)
		}
		c.manifest.capturePause = time.Since(paused)
	}()
	for _, source := range c.sources {
		if err := sourceCtx.Err(); err != nil {
			return nil, err
		}
		if source.empty != nil {
			continue
		}
		var page *storage.PageSnapshot
		if source.engine != nil {
			source.cloned, page, source.release, err = source.engine.CaptureBackup(source.path, !m.backupNoClone)
		} else {
			source.cloned, page, err = source.vault.CaptureArchive(source.path, !m.backupNoClone)
			if page != nil {
				source.release = func() { _ = page.Close() }
			}
		}
		if page != nil {
			source.page = page
		}
		if err != nil {
			return nil, fmt.Errorf("cannot capture %s state: %w", source.network, err)
		}
	}
	m.backupSourceCancel, m.backupSourceDone = cancel, c.done
	success = true
	return c, nil
}

func (c *portableCapture) materialize() (backupManifest, error) {
	defer c.closeSources()
	success := false
	defer func() {
		if !success {
			c.staging.close()
		}
	}()
	for _, source := range c.sources {
		if err := c.ctx.Err(); err != nil {
			return backupManifest{}, err
		}
		entry := &c.manifest.Wallets[source.wallet]
		var provider *backupNetwork
		var mark backupMark
		var err error
		switch {
		case source.empty != nil:
			provider, err = c.staging.save(source.empty)
			if err == nil {
				mark.Fingerprint, err = daemon.BackupFingerprint(*source.empty)
			}
		case source.cloned:
			provider = &backupNetwork{path: source.path, password: source.password}
			mark, err = provider.validateSnapshot(c.ctx, entry.Mnemonic, source.network)
		default:
			var active daemon.State
			var stats storage.ArchiveStats
			stats, _, err = source.page.LoadState(&active)
			if err == nil && (active.Mnemonic != entry.Mnemonic || active.Network.Normalized() != source.network) {
				err = errors.New("backup network state does not match its wallet")
			}
			if err == nil {
				provider, mark, err = c.staging.saveView(c.ctx, active, stats, source.page)
			}
		}
		if err != nil {
			return backupManifest{}, fmt.Errorf("cannot back up %s state: %w", source.network, err)
		}
		entry.sources[source.network], entry.marks[source.network] = provider, mark
	}
	if err := c.ctx.Err(); err != nil {
		return backupManifest{}, err
	}
	success = true
	return c.manifest, nil
}
