package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Inspect every installed wallet directory, including a fully prepared orphan
// left by a crash before the Settings commit. Profile labels and IDs cannot
// establish identity, and uncertainty must not enable a second local engine.
// The manager's lifecycle lock must be held while checking/installing a backup.
func (m *Manager) checkBackupIdentitiesLocked(manifest backupManifest) error {
	wanted := map[string]bool{}
	for _, entry := range manifest.Wallets {
		wanted[entry.Identity] = true
	}
	root := filepath.Join(m.root, "wallets")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(path, "master.db")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		seed, password, err := readMaster(path)
		if err != nil {
			return errors.New("cannot verify an installed wallet identity; repair its local vault before importing")
		}
		clear(password)
		identity, err := backupIdentity(seed)
		if err != nil {
			return err
		}
		if wanted[identity] {
			return errors.New("this wallet identity already exists in this installation; use its existing profile to avoid duplicate recovery engines")
		}
	}
	return nil
}
