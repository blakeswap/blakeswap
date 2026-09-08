package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"google.golang.org/protobuf/proto"
)

// Portable-produced vault validation uses actual filesystem and disk bounds.
// The legacy source reader retains its independent 64 MiB input policy.
const portableVaultLimit = math.MaxInt64

type portableImportRequest struct {
	Path         string
	Password     string
	SourceWallet string
	Name         string
	Revision     uint64
}

type portableImportResult struct {
	ProfileID string       `json:"profile_id"`
	Settings  *pb.Settings `json:"settings"`
	Legacy    bool         `json:"legacy"`
}

// Local commit marker permits startup to finish an interrupted installation.
// The random destination ID comes from this installation, never archive paths.
type preparedImport struct {
	Name     string          `json:"name"`
	Identity string          `json:"identity"`
	Networks []chain.Network `json:"networks"`
}

func (m *Manager) importPortable(ctx context.Context, request portableImportRequest) (portableImportResult, error) {
	defer m.beginInstallation()()
	result := portableImportResult{}
	manifest, legacy, err := readBackupManifest(ctx, m.root, request.Path, request.Password)
	if err != nil {
		return result, err
	}
	defer manifest.close()
	var selected *backupWallet
	for i := range manifest.Wallets {
		if manifest.Wallets[i].ID == request.SourceWallet || request.SourceWallet == "" && len(manifest.Wallets) == 1 {
			selected = &manifest.Wallets[i]
			break
		}
	}
	if selected == nil {
		return result, errors.New("select the wallet to import from this backup")
	}
	name := request.Name
	if name == "" {
		name = selected.Name
	}
	if err = validateWalletName(name); err != nil {
		return result, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "backup.import", &pb.ImportBackupRequest{Path: request.Path, Password: request.Password, SourceWalletId: request.SourceWallet, Name: request.Name, Revision: request.Revision}); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if m.stopped {
		return result, errors.New("daemon is stopping")
	}
	if request.Revision != m.settings.Revision {
		return result, errors.New("settings changed; reload before importing")
	}
	if m.settings.OnboardingStage != "" {
		return result, errors.New("use the first wallet restore flow during setup")
	}
	if len(m.settings.Wallets) >= 20 {
		return result, errors.New("at most 20 wallet profiles")
	}
	selectedManifest := backupManifest{FormatVersion: manifest.FormatVersion, CreatedAt: manifest.CreatedAt, Wallets: []backupWallet{*selected}}
	if err = m.checkBackupIdentitiesLocked(selectedManifest); err != nil {
		return result, err
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return result, err
	}
	id := "wallet-" + hex.EncodeToString(random[:])
	walletRoot := filepath.Join(m.root, "wallets")
	if err = os.MkdirAll(walletRoot, 0700); err != nil {
		return result, err
	}
	staging, err := os.MkdirTemp(walletRoot, ".import-")
	if err != nil {
		return result, err
	}
	defer m.removeStaging(staging, id)
	metadata, err := m.prepareImportedProfile(ctx, staging, id, *selected, name, manifest.CreatedAt, legacy)
	if err != nil {
		return result, err
	}
	marker, err := json.Marshal(metadata)
	if err != nil {
		return result, err
	}
	if err = writePrivate(filepath.Join(staging, "import.json"), marker); err != nil {
		return result, err
	}
	if err = syncDirectory(staging); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	target := filepath.Join(walletRoot, id)
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("generated wallet destination already exists")
	}
	if err = os.Rename(staging, target); err != nil {
		return result, err
	}
	m.unpublishedInstalls.Add(1)
	if err = syncDirectory(walletRoot); err != nil {
		return result, err
	}
	// From here the complete, gated vault is durable. Keep it on any later error:
	// startup can finish its marker; it must never create another identity copy.
	if m.runtimeCtx != nil {
		if err = m.startAPI(id); err != nil {
			return result, fmt.Errorf("recovery wallet installed; reopen the app to finish: %w", err)
		}
		if err = m.writeRuntime(); err != nil {
			m.servers[id].Close()
			delete(m.servers, id)
			return result, err
		}
	}
	next := proto.Clone(m.settings).(*pb.Settings)
	next.Wallets = append(next.Wallets, &pb.WalletProfile{Id: id, Name: name})
	next.Revision++
	if err = saveSettings(m.root, next); err != nil {
		if server := m.servers[id]; server != nil {
			server.Close()
			delete(m.servers, id)
			_ = m.writeRuntime()
		}
		return result, fmt.Errorf("recovery wallet installed; reopen the app to finish: %w", err)
	}
	m.settings = next
	m.publishView()
	m.unpublishedInstalls.Add(-1)
	return portableImportResult{ProfileID: id, Settings: proto.Clone(next).(*pb.Settings), Legacy: legacy}, nil
}

func prepareImportedProfile(ctx context.Context, staging string, entry backupWallet, name string, snapshotAt int64, legacy bool) (preparedImport, error) {
	return (&Manager{}).prepareImportedProfile(ctx, staging, "", entry, name, snapshotAt, legacy)
}
func (m *Manager) prepareImportedProfile(ctx context.Context, staging, profile string, entry backupWallet, name string, snapshotAt int64, legacy bool) (preparedImport, error) {
	var metadata preparedImport
	err := m.initializeProfile(ctx, staging, profile, entry.Mnemonic, func(password []byte) error {
		var err error
		metadata, err = writeImportedProfile(ctx, staging, entry, name, snapshotAt, legacy, password)
		return err
	})
	return metadata, err
}
func writeImportedProfile(ctx context.Context, staging string, entry backupWallet, name string, snapshotAt int64, legacy bool, password []byte) (preparedImport, error) {
	metadata := preparedImport{Name: name, Identity: entry.Identity}
	if err := saveVault(filepath.Join(staging, "master.db"), password, struct {
		Mnemonic string `json:"mnemonic"`
	}{entry.Mnemonic}); err != nil {
		return metadata, err
	}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		if err := ctx.Err(); err != nil {
			return metadata, err
		}
		path := filepath.Join(staging, string(network), "state.db")
		if source := entry.sources[network]; source != nil {
			if err := source.restore(ctx, path, password, snapshotAt, legacy); err != nil {
				return metadata, err
			}
		} else {
			state := entry.Networks[network]
			if state == nil {
				// Even networks omitted by a legacy file enter the recovery gate.
				state = &daemon.State{Version: 1, Network: network, Mnemonic: entry.Mnemonic}
				normalizeState(state)
			}
			if err := daemon.PrepareRecovery(state, snapshotAt, legacy); err != nil {
				return metadata, err
			}
			if err := saveVault(path, password, state); err != nil {
				return metadata, err
			}
		}

		info, err := os.Stat(path)
		if err != nil {
			return metadata, err
		}
		if err := storage.CheckPathSpace(staging, uint64(info.Size()), 4); err != nil {
			return metadata, err
		}
		metadata.Networks = append(metadata.Networks, network)
	}
	return metadata, nil
}

// Called before any wallet engines start. A crash after the atomic directory
// rename but before Settings publication leaves a complete gated profile here.
func recoverPreparedImports(root string, settings *pb.Settings, readers ...func(string) (string, []byte, error)) error {
	reader := readMaster
	if len(readers) == 1 {
		reader = readers[0]
	}
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
	pending := false
	for _, entry := range entries {
		if !entry.IsDir() || !walletID.MatchString(entry.Name()) || listed[entry.Name()] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "wallets", entry.Name(), "import.json")); err == nil {
			pending = true
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !pending {
		return nil
	}
	identities := map[string]bool{}
	for _, profile := range settings.Wallets {
		seed, password, err := reader(filepath.Join(root, "wallets", profile.Id))
		clear(password)
		if err != nil {
			return errors.New("cannot verify an installed wallet identity before finishing import")
		}
		identity, err := backupIdentity(seed)
		if err != nil || identities[identity] {
			return errors.New("duplicate or invalid installed wallet identity")
		}
		identities[identity] = true
	}
	changed := false
	for _, entry := range entries {
		if !entry.IsDir() || !walletID.MatchString(entry.Name()) || listed[entry.Name()] {
			continue
		}
		path := filepath.Join(root, "wallets", entry.Name())
		raw, err := os.ReadFile(filepath.Join(path, "import.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var marker preparedImport
		if len(raw) > 4096 || json.Unmarshal(raw, &marker) != nil || validateWalletName(marker.Name) != nil || len(marker.Networks) != 3 {
			return errors.New("invalid interrupted wallet import")
		}
		seed, password, err := reader(path)
		if err != nil {
			return err
		}
		clear(password)
		identity, err := backupIdentity(seed)
		if err != nil || identity != marker.Identity || identities[identity] {
			return errors.New("interrupted wallet import identity mismatch")
		}
		// Each network was committed before the marker. Re-authenticate the complete
		// profile, including the gate, rather than trusting plaintext marker claims.
		if err = validateInstalledRecovery(path, seed, marker.Networks, reader); err != nil {
			return err
		}
		if len(settings.Wallets) >= 20 {
			return errors.New("finish the interrupted import after reducing wallet profile count")
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

func validateInstalledRecovery(root, seed string, networks []chain.Network, readers ...func(string) (string, []byte, error)) error {
	reader := readMaster
	if len(readers) == 1 {
		reader = readers[0]
	}
	seen := map[chain.Network]bool{}
	for _, network := range networks {
		if network == "" || !network.Valid() || seen[network] {
			return errors.New("invalid interrupted import network")
		}
		seen[network] = true
		_, password, err := reader(root)
		if err != nil {
			return err
		}
		// Authenticate a private copy with the same bound enforced before atomic
		// installation. The legacy source's 64 MiB policy does not apply here.
		err = withPrivateStateBackupBytes(root, filepath.Join(root, string(network), "state.db"), password, portableVaultLimit, func(vault *storage.Vault) error {
			view, err := vault.Freeze()
			if err != nil {
				return err
			}
			defer view.Close()
			var state daemon.State
			stats, _, err := view.LoadState(&state)
			if err != nil {
				return err
			}
			if err = validateStreamedActive(&state, stats); err != nil {
				return err
			}
			if state.Mnemonic != seed || state.Network.Normalized() != network || state.Recovery == nil || state.Recovery.ImportedAt <= 0 || state.Recovery.ImportedAt > time.Now().Add(24*time.Hour).Unix() {
				return errors.New("interrupted import lacks its recovery gate")
			}
			return view.VisitArchive(context.Background(), func(record storage.ArchiveRecord) error { return validateStreamedRecord(state, record) })
		})
		clear(password)
		if err != nil {
			return err
		}
	}
	return nil
}
