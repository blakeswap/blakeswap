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
// obtaining this deep copy, before performing encryption or filesystem IO.
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
			entry := backupWallet{ID: profile.Id, Name: profile.Name, Mnemonic: seed, Identity: identity, Networks: map[chain.Network]*daemon.State{}}
			for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
				var snapshot daemon.State
				engine := m.engines[profile.Id]
				if engine != nil && engine.Config.Network.Normalized() == network {
					snapshot, err = engine.BackupSnapshot()
				} else {
					path := filepath.Join(root, string(network), "state.db")
					if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
						// Explicitly describe unused networks too. Import does not turn
						// this absence into proof that a later instance never used them.
						snapshot = daemon.State{Version: 1, Network: network, Mnemonic: seed}
					} else if statErr != nil {
						return entry, statErr
					} else {
						vault, openErr := storage.Open(path, bytes.TrimSpace(password))
						if openErr != nil {
							return entry, openErr
						}
						_, err = vault.Load(&snapshot)
						closeErr := vault.Close()
						if err == nil {
							err = closeErr
						}
					}
				}
				if err != nil {
					return entry, fmt.Errorf("cannot back up %s state: %w", network, err)
				}
				normalizeState(&snapshot)
				entry.Networks[network] = &snapshot
			}
			return entry, nil
		}()
		if err != nil {
			return manifest, err
		}
		manifest.Wallets = append(manifest.Wallets, entry)
	}
	return manifest, validateBackupManifest(&manifest)
}
