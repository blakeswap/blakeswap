package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Called with the lifecycle lock held. Workers and opening engines are joined
// before any snapshot is read, so archive membership and every network's durable
// state share a consistent local boundary. The caller may restart workers after
// copying the pinned views into private encrypted staging.
func (m *Manager) backupSnapshotLocked(ctx context.Context, selected string, all bool) (backupManifest, error) {
	manifest := backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix()}
	if m.stopped {
		return manifest, errors.New("daemon is stopping")
	}
	found := false
	for _, profile := range m.settings.Wallets {
		if profile.Id == selected {
			found = true
		}
	}
	if !found {
		return manifest, errors.New("wallet profile does not exist")
	}
	staging, err := newPortableStaging(m.root)
	if err != nil {
		return manifest, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			staging.close()
		}
	}()
	manifest.release = staging.close
	m.stopWorkers()
	m.stopOpening()
	for _, profile := range m.settings.Wallets {
		if !all && profile.Id != selected {
			continue
		}
		if err := ctx.Err(); err != nil {
			return manifest, err
		}
		root := filepath.Join(m.root, "wallets", profile.Id)
		seed, password, err := readMaster(root)
		if err != nil {
			return manifest, err
		}
		// Keep credentials scoped to one profile, never in the archive object.
		entry, err := func() (backupWallet, error) {
			defer clear(password)
			identity, err := backupIdentity(seed)
			if err != nil {
				return backupWallet{}, err
			}
			entry := backupWallet{ID: profile.Id, Name: profile.Name, Mnemonic: seed, Identity: identity, Networks: map[chain.Network]*daemon.State{}, sources: map[chain.Network]*backupNetwork{}, marks: map[chain.Network]backupMark{}}
			for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
				var source *backupNetwork
				var mark backupMark
				stageView := func(view *storage.ReadSnapshot) error {
					var snapshot daemon.State
					stats, _, loadErr := view.LoadState(&snapshot)
					if loadErr != nil {
						return loadErr
					}
					if snapshot.Mnemonic != seed || snapshot.Network.Normalized() != network {
						return errors.New("backup network state does not match its wallet")
					}
					var stageErr error
					source, mark, stageErr = staging.saveView(ctx, snapshot, stats, view)
					return stageErr
				}
				engine := m.engines[profile.Id]
				if engine != nil && engine.Config.Network.Normalized() == network {
					view, release, freezeErr := engine.FreezeBackup()
					if freezeErr != nil {
						return entry, freezeErr
					}
					err = stageView(view)
					release()
				} else {
					path := filepath.Join(root, string(network), "state.db")
					if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
						snapshot := daemon.State{Version: 1, Network: network, Mnemonic: seed}
						normalizeState(&snapshot)
						source, err = staging.save(&snapshot)
						if err == nil {
							mark.Fingerprint, err = daemon.BackupFingerprint(snapshot)
						}
					} else if statErr != nil {
						return entry, statErr
					} else {
						vault, openErr := storage.Open(path, bytes.TrimSpace(password))
						if openErr != nil {
							return entry, openErr
						}
						err = func() error {
							var active daemon.State
							if _, err := vault.Load(&active); err != nil {
								return err
							}
							changed, err := daemon.PrepareStoredBackupToken(&active)
							if err != nil {
								return err
							}
							if changed {
								if err = vault.Save(active); err != nil {
									return err
								}
							}
							view, err := vault.Freeze()
							if err != nil {
								return err
							}
							defer view.Close()
							return stageView(view)
						}()
						closeErr := vault.Close()
						if err == nil {
							err = closeErr
						}
					}
				}
				if err != nil {
					return entry, fmt.Errorf("cannot back up %s state: %w", network, err)
				}

				entry.Networks[network] = nil
				entry.sources[network] = source
				entry.marks[network] = mark
			}
			return entry, nil
		}()
		if err != nil {
			return manifest, err
		}
		manifest.Wallets = append(manifest.Wallets, entry)
	}
	if err := validateBackupInventory(manifest); err != nil {
		return manifest, err
	}
	succeeded = true
	return manifest, nil
}
