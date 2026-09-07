package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type portableExportResult struct {
	Path            string `json:"path"`
	CreatedAt       int64  `json:"created_at"`
	Wallets         int    `json:"wallets"`
	Networks        int    `json:"networks"`
	ReminderWarning string `json:"reminder_warning,omitempty"`
}

// Snapshot under the lifecycle lock; encrypt the immutable copy after restarting
// workers. A completed archive is useful even if recording its reminder fails.
func (m *Manager) exportPortable(ctx context.Context, profile, path, password string, all bool) (portableExportResult, error) {
	result := portableExportResult{}
	if !filepath.IsAbs(path) || len(password) < 16 {
		return result, errors.New("choose an absolute destination and a backup password of at least 16 bytes")
	}
	m.mu.Lock()
	manifest, err := m.backupSnapshotLocked(ctx, profile, all)
	defer manifest.close()
	if m.runtimeCtx != nil && !m.stopped {
		m.startWorkers(m.runtimeCtx)
	}
	m.mu.Unlock()
	if err != nil {
		return result, err
	}
	secret := []byte(password)
	defer clear(secret)
	if err := writeStreamManifest(ctx, path, secret, manifest); err != nil {
		return result, err
	}
	result.Path, result.CreatedAt, result.Wallets = path, manifest.CreatedAt, len(manifest.Wallets)
	for _, wallet := range manifest.Wallets {
		result.Networks += len(wallet.Networks)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recordPortableLocked(manifest); err != nil {
		result.ReminderWarning = "Backup saved successfully; its freshness reminder could not be updated. Keep this file and create another export after the wallet is available."
	}
	return result, nil
}

func (m *Manager) recordPortableLocked(manifest backupManifest) error {
	if m.stopped {
		return errors.New("daemon is stopping")
	}
	// A network switch may have started while encryption ran. Join openers before
	// opening inactive vaults, and resolve active engines again under this lock.
	m.stopOpening()
	for _, profile := range manifest.Wallets {
		root := filepath.Join(m.root, "wallets", profile.ID)
		mnemonic, password, err := readMaster(root)
		if err != nil {
			return err
		}
		err = func() error {
			defer clear(password)
			if mnemonic != profile.Mnemonic {
				return errors.New("wallet identity changed after snapshot")
			}
			for network, snapshot := range profile.Networks {
				mark, marked := profile.marks[network]
				if !marked {
					if snapshot == nil {
						return errors.New("portable snapshot mark is missing")
					}
					fingerprint, err := daemon.BackupFingerprint(*snapshot)
					if err != nil {
						return err
					}
					mark = backupMark{Fingerprint: fingerprint, SemanticToken: daemon.BackupSemanticToken(*snapshot)}
				}
				if engine := m.engines[profile.ID]; engine != nil && engine.Config.Network.Normalized() == network {
					if err := engine.RecordBackupSnapshot(mark.Fingerprint, mark.SemanticToken, manifest.CreatedAt); err != nil {
						return err
					}
					continue
				}
				path := filepath.Join(root, string(network), "state.db")
				if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return err
				}
				vault, err := storage.Open(path, bytes.TrimSpace(password))
				if err != nil {
					return err
				}
				err = func() error {
					var live daemon.State
					if _, err := vault.Load(&live); err != nil {
						return err
					}
					if live.Mnemonic != profile.Mnemonic || live.Network.Normalized() != network {
						return errors.New("network state changed identity")
					}
					live.Backup = &daemon.BackupRecord{CreatedAt: manifest.CreatedAt, Fingerprint: mark.Fingerprint, SemanticToken: mark.SemanticToken}
					return vault.Save(live)
				}()
				closeErr := vault.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	m.publishView()
	return nil
}
