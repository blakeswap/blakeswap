package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

// profileCredentials contains references only. Every acquisition owns its bytes;
// the native Store must be noninteractive. User authentication precedes this
// boundary, outside the manager/engine locks, through the owned broker.
type profileCredentials struct {
	installation string
	profiles     credential.Profiles
}

func openProfileCredentials(ctx context.Context, root string, store credential.Store) (*profileCredentials, error) {
	if store == nil {
		return nil, credential.ErrUnavailable
	}
	path := filepath.Join(root, "installation.json")
	var id string
	if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return nil, err
		}
		id = hex.EncodeToString(random[:])
		if err = writePrivate(path, []byte(id)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 64 {
			return nil, errors.New("invalid private installation identity")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(raw) != 64 {
			return nil, errors.New("invalid installation identity")
		}
		if _, err := hex.DecodeString(string(raw)); err != nil {
			return nil, errors.New("invalid installation identity")
		}
		id = string(raw)
	}
	c := &profileCredentials{installation: id, profiles: credential.Profiles{Store: store}}
	entries, err := os.ReadDir(filepath.Join(root, "wallets"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !walletID.MatchString(entry.Name()) {
			continue
		}
		profileRoot := filepath.Join(root, "wallets", entry.Name())
		password, err := c.profiles.Migrate(ctx, profileRoot, c.key(entry.Name()), func(password []byte) (string, error) { return verifyProfile(profileRoot, password) })
		clear(password)
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (c *profileCredentials) key(profile string) credential.Key {
	return credential.Key{Installation: c.installation, Profile: profile}
}
func (c *profileCredentials) source(root, profile string) (credential.Source, error) {
	return c.profiles.Source(root, c.key(profile))
}
func (c *profileCredentials) readMaster(root string) (string, []byte, error) {
	source, err := c.source(root, filepath.Base(root))
	if err != nil {
		return "", nil, err
	}
	return readMasterSource(root, source)
}
func (m *Manager) readMaster(root string) (string, []byte, error) {
	if m.credentials == nil {
		return readMaster(root)
	}
	return m.credentials.readMaster(root)
}
func readMasterSource(root string, source credential.Source) (string, []byte, error) {
	password, err := source.Acquire(context.Background())
	if err != nil {
		clear(password)
		return "", nil, err
	}
	seed, err := readMasterPassword(root, password)
	if err != nil {
		clear(password)
		return "", nil, err
	}
	return seed, password, nil
}
func readMasterPassword(root string, password []byte) (string, error) {
	path := filepath.Join(root, "master.db")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() == 0 {
		return "", errors.New("invalid private master vault")
	}
	vault, err := storage.Open(path, password)
	if err != nil {
		return "", errors.New("cannot open the wallet with its native credential")
	}
	defer vault.Close()
	var state struct {
		Mnemonic string `json:"mnemonic"`
	}
	if _, err = vault.Load(&state); err != nil {
		return "", errors.New("cannot read wallet recovery phrase")
	}
	if _, err = wallet.FromMnemonic(state.Mnemonic); err != nil {
		return "", errors.New("cannot read wallet recovery phrase")
	}
	return state.Mnemonic, nil
}

// The verifier authenticates each existing vault without rewriting its state.
// Missing unused networks are allowed; missing/corrupt master and mismatched
// network identities are not. A caller must own the lifecycle before opening.
func verifyProfile(root string, password []byte) (string, error) {
	seed, err := readMasterPassword(root, password)
	if err != nil {
		return "", err
	}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		path := filepath.Join(root, string(network), "state.db")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() == 0 {
			return "", errors.New("invalid private network vault")
		}
		vault, err := storage.Open(path, password)
		if err != nil {
			return "", errors.New("cannot verify an existing network vault")
		}
		var state daemon.State
		_, err = vault.Load(&state)
		closeErr := vault.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		if state.Mnemonic != seed || state.Network.Normalized() != network {
			return "", errors.New("credential migration found mismatched wallet identities")
		}
	}
	return backupIdentity(seed)
}
func (m *Manager) storedConfig(profile, network string) (daemon.Config, error) {
	root := filepath.Join(m.root, "wallets", profile)
	cfg := daemon.Config{Name: profile, Network: chain.Network(network), DataDir: filepath.Join(root, network), CredentialMode: "file", PasswordFile: filepath.Join(root, "vault.password")}
	if m.credentials != nil {
		source, err := m.credentials.source(root, profile)
		if err != nil {
			return cfg, err
		}
		cfg.CredentialMode, cfg.Credential, cfg.PasswordFile, cfg.Installation = "native", source, "", m.credentials.installation
	}
	return cfg, nil
}
func (m *Manager) initializeProfile(ctx context.Context, root, profile, mnemonic string, initialize func([]byte) error) error {
	if m.credentials == nil {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		password := []byte(hex.EncodeToString(random[:]))
		clear(random[:])
		defer clear(password)
		if err := writePrivate(filepath.Join(root, "vault.password"), password); err != nil {
			return err
		}
		return initialize(password)
	}
	identity, err := backupIdentity(mnemonic)
	if err != nil {
		return err
	}
	password, err := m.credentials.profiles.Initialize(ctx, root, m.credentials.key(profile), identity, initialize, func(password []byte) (string, error) { return verifyProfile(root, password) })
	clear(password)
	return err
}
func (m *Manager) initializeMaster(ctx context.Context, root, profile, seed string) error {
	return m.initializeProfile(ctx, root, profile, seed, func(password []byte) error {
		return saveVault(filepath.Join(root, "master.db"), password, struct {
			Mnemonic string `json:"mnemonic"`
		}{seed})
	})
}

// A published directory no longer exists under its private staging name. Never
// delete its item after publication, even if settings/runtime publication fails.
// On a failed Delete keep the private journal for an explicit later cleanup.
func (m *Manager) removeStaging(root, profile string) {
	if m.credentials != nil {
		j, err := credential.ReadJournal(root)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil || j.Origin != "new" || j.Key.Installation != m.credentials.installation || j.Key.Profile != profile {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.credentials.profiles.Store.Delete(ctx, j.Key); err != nil && !errors.Is(err, credential.ErrMissing) {
			return
		}
	}
	_ = os.RemoveAll(root)
}
