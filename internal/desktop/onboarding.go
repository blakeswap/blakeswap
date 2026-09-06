package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
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
	if err := validateWalletName(request.Name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if request.Mnemonic != "" && request.BackupPath != "" {
		return nil, status.Error(codes.InvalidArgument, "choose a recovery phrase or a backup file")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.setupGuard(ctx, request.Revision, "wallet"); err != nil {
		return nil, err
	}
	var restored *daemon.State
	mnemonic := strings.Join(strings.Fields(strings.ToLower(request.Mnemonic)), " ")
	stage, network := "backup", m.settings.ActiveNetwork
	if request.BackupPath != "" {
		var err error
		restored, err = readStateBackup(m.root, request.BackupPath, request.BackupPassword)
		if err != nil {
			return nil, err
		}
		mnemonic, network, stage = restored.Mnemonic, string(restored.Network.Normalized()), "connect"
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
	defer os.RemoveAll(staging)
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	password := []byte(hex.EncodeToString(random[:]))
	clear(random[:])
	defer clear(password)
	if err := writePrivate(filepath.Join(staging, "vault.password"), password); err != nil {
		return nil, err
	}
	if err := saveVault(filepath.Join(staging, "master.db"), password, struct {
		Mnemonic string `json:"mnemonic"`
	}{mnemonic}); err != nil {
		return nil, err
	}
	if restored != nil {
		if err := saveVault(filepath.Join(staging, network, "state.db"), password, restored); err != nil {
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
	if err := syncDirectory(walletRoot); err != nil {
		return nil, err
	}
	next := proto.Clone(m.settings).(*pb.Settings)
	if err := recoverPreparedWallet(m.root, next); err != nil {
		return nil, err
	}
	m.settings = next
	m.publishView()
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
	if !filepath.IsAbs(source) {
		return nil, errors.New("choose an absolute backup file path")
	}
	input, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<20 {
		return nil, errors.New("choose a wallet backup file up to 64 MiB")
	}
	// Open only a private copy: authentication failures never modify the source.
	copy, err := os.CreateTemp(root, ".restore-*.db")
	if err != nil {
		return nil, err
	}
	defer os.Remove(copy.Name())
	n, err := io.Copy(copy, io.LimitReader(input, (64<<20)+1))
	closeErr := copy.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if n > 64<<20 {
		return nil, errors.New("backup exceeds 64 MiB")
	}
	vault, err := storage.Open(copy.Name(), []byte(password))
	if err != nil {
		return nil, errors.New("cannot unlock backup; check its password and file")
	}
	defer vault.Close()
	var state daemon.State
	if _, err := vault.Load(&state); err != nil {
		return nil, errors.New("invalid wallet backup")
	}
	if state.Version != 1 || !state.Network.Valid() {
		return nil, errors.New("unsupported wallet backup")
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
	if err := m.setupGuard(ctx, m.settings.Revision, "backup", "connect"); err != nil {
		return nil, err
	}
	return m.firstWalletLocked()
}
func (m *Manager) firstWalletLocked() (*pb.FirstWallet, error) {
	if m.settings.OnboardingStage != "backup" {
		return &pb.FirstWallet{Settings: proto.Clone(m.settings).(*pb.Settings)}, nil
	}
	seed, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
	if err != nil {
		return nil, err
	}
	defer clear(password)
	words := strings.Fields(seed)
	return &pb.FirstWallet{Settings: proto.Clone(m.settings).(*pb.Settings), Recovery: &pb.Recovery{Mnemonic: seed}, BackupWordPositions: []uint32{3, uint32(len(words)/2 + 1), uint32(len(words))}}, nil
}
func readMaster(root string) (string, []byte, error) {
	password, err := os.ReadFile(filepath.Join(root, "vault.password"))
	if err != nil {
		return "", nil, err
	}
	vault, err := storage.Open(filepath.Join(root, "master.db"), bytes.TrimSpace(password))
	if err != nil {
		clear(password)
		return "", nil, err
	}
	defer vault.Close()
	var state struct {
		Mnemonic string `json:"mnemonic"`
	}
	_, err = vault.Load(&state)
	if err == nil {
		_, err = wallet.FromMnemonic(state.Mnemonic)
	}
	if err != nil {
		clear(password)
		return "", nil, errors.New("cannot read wallet recovery phrase")
	}
	return state.Mnemonic, password, nil
}
func (m *Manager) confirmFirstWallet(ctx context.Context, request *pb.ConfirmFirstWalletRequest) (*pb.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	defer m.mu.Unlock()
	if err := m.setupGuard(ctx, request.Revision, "backup", "connect"); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(request.Path) || len(request.Password) < 16 {
		return nil, errors.New("choose a backup location and a password of at least 16 characters")
	}
	seed, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
	if err != nil {
		return nil, err
	}
	defer clear(password)
	state := daemon.State{Version: 1, Network: chain.Network(m.settings.ActiveNetwork), Mnemonic: seed}
	normalizeState(&state)
	// A restored state backup must preserve its pending swaps when exported again.
	current := filepath.Join(m.root, "wallets", "alice", m.settings.ActiveNetwork, "state.db")
	if _, err := os.Stat(current); err == nil {
		vault, err := storage.Open(current, bytes.TrimSpace(password))
		if err != nil {
			return nil, err
		}
		_, err = vault.Load(&state)
		vault.Close()
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	output, err := os.OpenFile(request.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("choose a new backup filename; existing files are never replaced")
	}
	if err := output.Close(); err != nil {
		return nil, err
	}
	if err := saveVault(request.Path, []byte(request.Password), state); err != nil {
		_ = os.Remove(request.Path)
		return nil, err
	}
	return &pb.Backup{Path: request.Path}, nil
}
func (m *Manager) finishOnboarding(ctx context.Context, next *pb.Settings) (*pb.Settings, error) {
	if err := validate(next); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
		cfg := daemon.Config{DataDir: filepath.Join(m.root, "wallets", "alice", m.settings.ActiveNetwork), PasswordFile: filepath.Join(m.root, "wallets", "alice", "vault.password")}
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
