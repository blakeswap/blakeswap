package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type preparedWallet struct {
	Name    string
	Stage   string
	Network string
}

// The complete wallet directory is installed before Settings advances. If the
// process stops between those atomic writes, startup resumes that same wallet.
func recoverPreparedWallet(root string, s *pb.Settings) error {
	raw, err := os.ReadFile(filepath.Join(root, "wallets", "alice", "setup.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var prepared preparedWallet
	if err := json.Unmarshal(raw, &prepared); err != nil {
		return err
	}
	if err := validateWalletName(prepared.Name); err != nil {
		return err
	}
	if (prepared.Stage != "backup" && prepared.Stage != "connect") || prepared.Network == "" || !chain.Network(prepared.Network).Valid() {
		return errors.New("invalid prepared wallet")
	}
	s.Wallets[0].Name, s.OnboardingStage, s.ActiveNetwork = prepared.Name, prepared.Stage, prepared.Network
	s.Revision++
	return saveSettings(root, s)
}

func (m *Manager) setupGuard(ctx context.Context, revision uint64, stages ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.stopped {
		return status.Error(codes.Unavailable, "daemon is stopping")
	}
	if m.settings.Revision != revision {
		return status.Error(codes.Aborted, "setup changed; reload before continuing")
	}
	for _, stage := range stages {
		if m.settings.OnboardingStage == stage {
			return nil
		}
	}
	return status.Error(codes.FailedPrecondition, "this onboarding step is no longer available")
}

func (m *Manager) prepareFirstWallet(ctx context.Context, request *pb.PrepareFirstWalletRequest) (*pb.FirstWallet, error) {
	defer m.beginInstallation()()
	if err := validateWalletName(request.Name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if request.Mnemonic != "" && request.BackupPath != "" {
		return nil, status.Error(codes.InvalidArgument, "choose a recovery phrase or a backup file")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "onboarding.prepare", request); err != nil {
		return nil, err
	}
	if err := m.setupGuard(ctx, request.Revision, "wallet"); err != nil {
		return nil, err
	}
	var restored *backupWallet
	var legacy bool
	snapshotAt := time.Now().Unix()
	mnemonic := strings.Join(strings.Fields(strings.ToLower(request.Mnemonic)), " ")
	stage, network := "backup", m.settings.ActiveNetwork
	if request.BackupPath != "" {
		manifest, oldFormat, err := readBackupManifest(ctx, m.root, request.BackupPath, request.BackupPassword)
		if err != nil {
			return nil, err
		}
		defer manifest.close()
		for i := range manifest.Wallets {
			if manifest.Wallets[i].ID == request.SourceWalletId || request.SourceWalletId == "" && len(manifest.Wallets) == 1 {
				restored = &manifest.Wallets[i]
				break
			}
		}
		if restored == nil {
			return nil, errors.New("select the wallet to restore from this backup")
		}
		legacy, snapshotAt = oldFormat, manifest.CreatedAt
		mnemonic, stage = restored.Mnemonic, "connect"
		if len(restored.Networks) == 1 {
			for n := range restored.Networks {
				network = string(n)
			}
		}
	} else if request.BackupPassword != "" {
		return nil, errors.New("select the backup file")
	}
	if request.Mnemonic != "" && mnemonic == "" {
		return nil, errors.New("enter a valid BIP39 recovery phrase")
	}
	if mnemonic == "" {
		var err error
		mnemonic, err = wallet.NewMnemonic()
		if err != nil {
			return nil, err
		}
	}
	if _, err := wallet.FromMnemonic(mnemonic); err != nil {
		return nil, errors.New("enter a valid BIP39 recovery phrase")
	}
	if restored != nil || request.Mnemonic != "" {
		identity, err := backupIdentity(mnemonic)
		if err != nil {
			return nil, err
		}
		if err = m.checkBackupIdentitiesLocked(backupManifest{Wallets: []backupWallet{{Identity: identity}}}); err != nil {
			return nil, err
		}
	}
	walletRoot := filepath.Join(m.root, "wallets")
	if err := os.MkdirAll(walletRoot, 0700); err != nil {
		return nil, err
	}
	target := filepath.Join(walletRoot, "alice")
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("a wallet already exists; reopen the app to resume setup")
	}
	staging, err := os.MkdirTemp(walletRoot, ".setup-")
	if err != nil {
		return nil, err
	}
	defer m.removeStaging(staging, "alice")
	if restored != nil || request.Mnemonic != "" {
		entry := backupWallet{Mnemonic: mnemonic, Networks: map[chain.Network]*daemon.State{}}
		entry.Identity, err = backupIdentity(mnemonic)
		if err != nil {
			return nil, err
		}
		if restored != nil {
			entry = *restored
		}
		if _, err = m.prepareImportedProfile(ctx, staging, "alice", entry, request.Name, snapshotAt, legacy); err != nil {
			return nil, err
		}
	} else {
		if err := m.initializeMaster(ctx, staging, "alice", mnemonic); err != nil {
			return nil, err
		}
	}
	metadata, err := json.Marshal(preparedWallet{request.Name, stage, network})
	if err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(staging, "setup.json"), metadata); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(staging, target); err != nil {
		return nil, err
	}
	m.unpublishedInstalls.Add(1)
	if err := syncDirectory(walletRoot); err != nil {
		return nil, err
	}
	next := proto.Clone(m.settings).(*pb.Settings)
	if err := recoverPreparedWallet(m.root, next); err != nil {
		return nil, err
	}
	m.settings = next
	m.publishView()
	m.unpublishedInstalls.Add(-1)
	return m.firstWalletLocked()
}

func saveVault(path string, password []byte, value any) error {
	vault, err := storage.Open(path, password)
	if err != nil {
		return err
	}
	if err := vault.Save(value); err != nil {
		vault.Close()
		return err
	}
	if err := vault.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readStateBackup(root, source, password string) (*daemon.State, error) {
	return readStateBackupBounded(root, source, password, 64<<20)
}

// The legacy source policy and the portable installer have distinct file bounds.
// Both authenticate only a private copy, so a failed read never edits its source.
func readStateBackupBounded(root, source, password string, maxBytes int64) (*daemon.State, error) {
	secret := []byte(password)
	defer clear(secret)
	return readStateBackupBytes(root, source, secret, maxBytes)
}
func readStateBackupBytes(root, source string, password []byte, maxBytes int64) (*daemon.State, error) {
	var result *daemon.State
	err := withPrivateStateBackupBytes(root, source, password, maxBytes, func(vault *storage.Vault) error {
		var err error
		result, err = readLegacyStateVault(vault)
		return err
	})
	return result, err
}

func readLegacyStateVault(vault *storage.Vault) (*daemon.State, error) {
	state, err := daemon.LoadCompleteState(vault)
	if err != nil {
		return nil, errors.New("invalid wallet backup")
	}
	if err := daemon.ValidateStateVersion(&state); err != nil {
		return nil, err
	}
	if _, err := wallet.FromMnemonic(state.Mnemonic); err != nil {
		return nil, errors.New("backup does not contain a valid wallet")
	}
	normalizeState(&state)
	for _, swap := range state.Swaps {
		if swap == nil {
			return nil, errors.New("invalid swap in backup")
		}
	}
	for _, job := range state.TowerJobs {
		if job == nil {
			return nil, errors.New("invalid watchtower job in backup")
		}
	}
	for _, delivery := range state.Outbox {
		if delivery == nil {
			return nil, errors.New("invalid delivery in backup")
		}
	}
	return &state, nil
}
func withPrivateStateBackup(root, source, password string, maxBytes int64, use func(*storage.Vault) error) error {
	secret := []byte(password)
	defer clear(secret)
	return withPrivateStateBackupBytes(root, source, secret, maxBytes, use)
}
func withPrivateStateBackupBytes(root, source string, password []byte, maxBytes int64, use func(*storage.Vault) error) error {
	if !filepath.IsAbs(source) {
		return errors.New("choose an absolute backup file path")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBytes {
		return fmt.Errorf("choose a wallet backup file up to %d MiB", maxBytes>>20)
	}
	if err := storage.CheckPathSpace(root, uint64(info.Size()), 2); err != nil {
		return err
	}
	// Open only a private copy: authentication failures never modify the source.
	copy, err := os.CreateTemp(root, ".restore-*.db")
	if err != nil {
		return err
	}
	defer os.Remove(copy.Name())
	copyLimit := maxBytes
	if copyLimit < int64(^uint64(0)>>1) {
		copyLimit++
	}
	n, err := io.Copy(copy, io.LimitReader(input, copyLimit))
	closeErr := copy.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxBytes {
		return fmt.Errorf("backup exceeds %d MiB", maxBytes>>20)
	}
	vault, err := storage.Open(copy.Name(), password)
	if err != nil {
		return errors.New("cannot unlock backup; check its password and file")
	}
	defer vault.Close()
	return use(vault)
}

func normalizeState(s *daemon.State) {
	if s.Offers == nil {
		s.Offers = map[string]nostr.Event{}
	}
	if s.Book == nil {
		s.Book = map[string]nostr.Event{}
	}
	if s.Swaps == nil {
		s.Swaps = map[string]*daemon.Swap{}
	}
	if s.Outbox == nil {
		s.Outbox = map[string]*daemon.Delivery{}
	}
	if s.Seen == nil {
		s.Seen = map[string]string{}
	}
	if s.TowerJobs == nil {
		s.TowerJobs = map[string]*daemon.TowerJob{}
	}
}

func (m *Manager) firstWallet(ctx context.Context) (*pb.FirstWallet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "onboarding.get", struct{}{}); err != nil {
		return nil, err
	}
	if err := m.setupGuard(ctx, m.settings.Revision, "backup", "connect"); err != nil {
		return nil, err
	}
	return m.firstWalletLocked()
}
func (m *Manager) firstWalletLocked() (*pb.FirstWallet, error) {
	if m.settings.OnboardingStage != "backup" {
		return &pb.FirstWallet{Settings: proto.Clone(m.settings).(*pb.Settings)}, nil
	}
	seed, password, err := m.readMaster(filepath.Join(m.root, "wallets", "alice"))
	if err != nil {
		return nil, err
	}
	defer clear(password)
	words := strings.Fields(seed)
	return &pb.FirstWallet{Settings: proto.Clone(m.settings).(*pb.Settings), Recovery: &pb.Recovery{Mnemonic: seed}, BackupWordPositions: []uint32{3, uint32(len(words)/2 + 1), uint32(len(words))}}, nil
}
func readMaster(root string) (string, []byte, error) {
	return readMasterSource(root, credential.File(filepath.Join(root, "vault.password")))
}
func (m *Manager) confirmFirstWallet(ctx context.Context, request *pb.ConfirmFirstWalletRequest) (*pb.Settings, error) {
	defer m.beginAction()()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "onboarding.confirm", request); err != nil {
		return nil, err
	}
	if err := m.setupGuard(ctx, request.Revision, "backup"); err != nil {
		return nil, err
	}
	first, err := m.firstWalletLocked()
	if err != nil {
		return nil, err
	}
	words := strings.Fields(first.Recovery.Mnemonic)
	if len(request.Words) != len(first.BackupWordPositions) {
		return nil, errors.New("enter the three requested recovery words")
	}
	for i, position := range first.BackupWordPositions {
		if strings.TrimSpace(strings.ToLower(request.Words[i])) != words[position-1] {
			return nil, errors.New("recovery words do not match; check your backup")
		}
	}
	next := proto.Clone(m.settings).(*pb.Settings)
	next.OnboardingStage = "connect"
	next.Revision++
	if err := saveSettings(m.root, next); err != nil {
		return nil, err
	}
	m.settings = next
	m.publishView()
	return proto.Clone(next).(*pb.Settings), nil
}
func (m *Manager) exportFirstWallet(ctx context.Context, request *pb.ExportFirstWalletRequest) (*pb.Backup, error) {
	m.mu.Lock()
	err := m.setupGuard(ctx, request.Revision, "backup", "connect")
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, err := m.exportPortable(ctx, "alice", request.Path, request.Password, false, exportConsent{"onboarding.export", request})
	if err != nil {
		return nil, err
	}
	return &pb.Backup{Path: result.Path}, nil
}
func (m *Manager) finishOnboarding(ctx context.Context, next *pb.Settings) (*pb.Settings, error) {
	defer m.beginAction()()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "onboarding.finish", next); err != nil {
		return nil, err
	}
	if err := validate(next); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := m.setupGuard(ctx, next.Revision, "connect"); err != nil {
		return nil, err
	}
	if next.OnboardingStage != "connect" || len(next.Wallets) != 1 || !proto.Equal(next.Wallets[0], m.settings.Wallets[0]) {
		return nil, errors.New("wallet setup changed; reload before finishing")
	}
	env := environment(next, next.ActiveNetwork)
	for _, node := range env.Nodes {
		if node.Url == "" {
			return nil, errors.New("configure endpoints for both chains")
		}
	}
	if next.ActiveNetwork != m.settings.ActiveNetwork {
		cfg, err := m.storedConfig("alice", m.settings.ActiveNetwork)
		if err != nil {
			return nil, err
		}
		if err := daemon.CheckStoredNetwork(cfg); err != nil {
			return nil, err
		}
	}
	saved := proto.Clone(next).(*pb.Settings)
	saved.OnboardingStage = ""
	saved.Revision++
	if err := saveSettings(m.root, saved); err != nil {
		return nil, err
	}
	m.settings, m.restart, m.lastError = saved, true, "Connecting"
	m.publishView()
	return proto.Clone(saved).(*pb.Settings), nil
}
