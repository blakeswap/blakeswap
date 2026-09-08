package desktop

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
)

type preparedProfile struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
}

// A fresh profile is published only after its credential and master have been
// verified. This marker finishes a crash between directory publication and the
// settings commit; authentication/identity checks precede making it visible.
func recoverPreparedProfiles(root string, settings *pb.Settings, reader func(string) (string, []byte, error)) error {
	if settings.OnboardingStage != "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "wallets"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, profile := range settings.Wallets {
		listed[profile.Id] = true
	}
	identities := map[string]bool{}
	loaded := false
	changed := false
	for _, entry := range entries {
		if !entry.IsDir() || !walletID.MatchString(entry.Name()) || listed[entry.Name()] {
			continue
		}
		directory := filepath.Join(root, "wallets", entry.Name())
		path := filepath.Join(directory, "profile.json")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return errors.New("invalid interrupted profile marker")
		}
		markerFile, err := os.Open(path)
		if err != nil {
			return err
		}
		opened, err := markerFile.Stat()
		if err != nil || !os.SameFile(info, opened) {
			markerFile.Close()
			return errors.New("interrupted profile marker changed while opening")
		}
		raw, err := io.ReadAll(io.LimitReader(markerFile, 4097))
		closeErr := markerFile.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		var marker preparedProfile
		if len(raw) > 4096 || json.Unmarshal(raw, &marker) != nil || validateWalletName(marker.Name) != nil {
			return errors.New("invalid interrupted profile")
		}
		if !loaded {
			for _, profile := range settings.Wallets {
				seed, password, err := reader(filepath.Join(root, "wallets", profile.Id))
				clear(password)
				if err != nil {
					return err
				}
				identity, err := backupIdentity(seed)
				if err != nil || identities[identity] {
					return errors.New("cannot verify installed profile identities")
				}
				identities[identity] = true
			}
			loaded = true
		}
		seed, password, err := reader(directory)
		clear(password)
		if err != nil {
			return err
		}
		identity, err := backupIdentity(seed)
		if err != nil || identity != marker.Identity || identities[identity] {
			return errors.New("interrupted profile identity mismatch")
		}
		if len(settings.Wallets) >= 20 {
			return errors.New("too many profiles to finish interrupted creation")
		}
		settings.Wallets = append(settings.Wallets, &pb.WalletProfile{Id: entry.Name(), Name: marker.Name})
		listed[entry.Name()] = true
		identities[identity] = true
		changed = true
	}
	if changed {
		settings.Revision++
		return saveSettings(root, settings)
	}
	return nil
}
