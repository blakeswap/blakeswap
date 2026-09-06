package desktop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/protobuf/proto"
)

func setupManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	settings, err := loadSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{root: root, settings: settings, engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}}
}
func prepare(t *testing.T, m *Manager) *pb.FirstWallet {
	t.Helper()
	first, err := m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Savings", Revision: m.settings.Revision})
	if err != nil {
		t.Fatal(err)
	}
	return first
}
func confirm(t *testing.T, m *Manager, first *pb.FirstWallet) {
	t.Helper()
	words := strings.Fields(first.Recovery.Mnemonic)
	request := &pb.ConfirmFirstWalletRequest{Revision: m.settings.Revision}
	for _, position := range first.BackupWordPositions {
		request.Words = append(request.Words, words[position-1])
	}
	if _, err := m.confirmFirstWallet(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}
func TestFreshInstallWaitsForOnboardingBeforeKeysOrConnections(t *testing.T) {
	m := setupManager(t)
	if m.settings.OnboardingStage != "wallet" {
		t.Fatal("fresh install skips onboarding")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.engines) != 0 || len(m.openings) != 0 {
		t.Fatal("connected before user setup")
	}
	if _, err := os.Stat(filepath.Join(m.root, "wallets")); !os.IsNotExist(err) {
		t.Fatal("created keys before user choice")
	}
	m.stopped = false
	next := proto.Clone(m.settings).(*pb.Settings)
	next.OnboardingStage = ""
	if _, err := m.writeSettings(context.Background(), next); err == nil {
		t.Fatal("ordinary settings bypassed onboarding")
	}
	if _, err := m.createWallet(context.Background(), &pb.CreateWalletRequest{Name: "Bypass", Revision: m.settings.Revision}); err == nil {
		t.Fatal("created another wallet during onboarding")
	}
}
func TestBackupConfirmationAndRestartPreserveFirstWallet(t *testing.T) {
	m := setupManager(t)
	first := prepare(t, m)
	if first.Settings.OnboardingStage != "backup" || len(strings.Fields(first.Recovery.Mnemonic)) != 24 {
		t.Fatal("missing created wallet backup")
	}
	if _, err := m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Replacement", Revision: m.settings.Revision}); err == nil {
		t.Fatal("replaced prepared seed")
	}
	if _, err := m.confirmFirstWallet(context.Background(), &pb.ConfirmFirstWalletRequest{Revision: m.settings.Revision, Words: []string{"wrong", "wrong", "wrong"}}); err == nil {
		t.Fatal("invalid backup confirmation accepted")
	}
	next := proto.Clone(m.settings).(*pb.Settings)
	next.OnboardingStage = "connect"
	if _, err := m.finishOnboarding(context.Background(), next); err == nil {
		t.Fatal("skipped backup check")
	}
	loaded, err := loadSettings(m.root)
	if err != nil {
		t.Fatal(err)
	}
	m.settings = loaded
	resumed, err := m.firstWallet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Recovery.Mnemonic != first.Recovery.Mnemonic {
		t.Fatal("restart generated different keys")
	}
	raw, _ := os.ReadFile(filepath.Join(m.root, "settings.json"))
	if bytes.Contains(raw, []byte(first.Recovery.Mnemonic)) {
		t.Fatal("recovery leaked into settings")
	}
	confirm(t, m, first)
	if m.settings.OnboardingStage != "connect" {
		t.Fatal("confirmation did not advance")
	}
	next = proto.Clone(m.settings).(*pb.Settings)
	saved, err := m.finishOnboarding(context.Background(), next)
	if err != nil || saved.OnboardingStage != "" {
		t.Fatal("onboarding did not finish", err)
	}
	if _, err := m.firstWallet(context.Background()); err == nil {
		t.Fatal("setup recovery endpoint remained open after completion")
	}
	loaded, err = loadSettings(m.root)
	if err != nil || loaded.OnboardingStage != "" {
		t.Fatal("completed onboarding reappears", err)
	}
}
func TestInterruptedSettingsCommitRecoversPreparedWallet(t *testing.T) {
	m := setupManager(t)
	original := proto.Clone(m.settings).(*pb.Settings)
	first := prepare(t, m)
	if err := saveSettings(m.root, original); err != nil {
		t.Fatal(err)
	} // Simulate interruption after directory install.
	loaded, err := loadSettings(m.root)
	if err != nil || loaded.OnboardingStage != "backup" || loaded.Wallets[0].Name != "Savings" {
		t.Fatal("prepared wallet not recovered", err)
	}
	m.settings = loaded
	recovered, err := m.firstWallet(context.Background())
	if err != nil || recovered.Recovery.Mnemonic != first.Recovery.Mnemonic {
		t.Fatal("recovery replaced seed", err)
	}
}
func TestPhraseRestoreValidatesBeforeWritingAndKeepsExactKeys(t *testing.T) {
	m := setupManager(t)
	for _, phrase := range []string{" ", "not a recovery phrase", strings.Repeat("abandon ", 12)} {
		if _, err := m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Restored", Mnemonic: phrase, Revision: m.settings.Revision}); err == nil {
			t.Fatal("invalid mnemonic accepted")
		}
	}
	phrase := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	first, err := m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Restored", Mnemonic: "  " + strings.ToUpper(phrase) + "\n", Revision: m.settings.Revision})
	if err != nil || first.Recovery.Mnemonic != phrase {
		t.Fatal("phrase changed during restore", err)
	}
	if _, err := m.confirmFirstWallet(context.Background(), &pb.ConfirmFirstWalletRequest{Revision: 1}); err == nil {
		t.Fatal("stale confirmation accepted")
	}
}
func TestEncryptedBackupRoundTripPreservesStateAndSource(t *testing.T) {
	original := setupManager(t)
	first := prepare(t, original)
	backup := filepath.Join(t.TempDir(), "wallet.blakeswap")
	password := "a test-only backup password"
	request := &pb.ExportFirstWalletRequest{Path: backup, Password: password, Revision: original.settings.Revision}
	if _, err := original.exportFirstWallet(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(backup)
	if bytes.Contains(before, []byte(first.Recovery.Mnemonic)) {
		t.Fatal("backup is not encrypted")
	}
	if _, err := original.exportFirstWallet(context.Background(), request); err == nil {
		t.Fatal("existing backup overwritten")
	}
	restored := setupManager(t)
	wrong := &pb.PrepareFirstWalletRequest{Name: "Restored", BackupPath: backup, BackupPassword: "incorrect password", Revision: restored.settings.Revision}
	if _, err := restored.prepareFirstWallet(context.Background(), wrong); err == nil {
		t.Fatal("incorrect password accepted")
	}
	after, _ := os.ReadFile(backup)
	if !bytes.Equal(before, after) {
		t.Fatal("failed restore modified source")
	}
	wrong.BackupPassword = password
	result, err := restored.prepareFirstWallet(context.Background(), wrong)
	if err != nil || result.Settings.OnboardingStage != "connect" {
		t.Fatal("backup restore failed", err)
	}
	seed, secret, err := readMaster(filepath.Join(restored.root, "wallets", "alice"))
	defer clear(secret)
	if err != nil || seed != first.Recovery.Mnemonic {
		t.Fatal("backup restored different keys", err)
	}
	if result.Recovery != nil {
		t.Fatal("state restore unnecessarily returned recovery words")
	}
	// An existing state backup may have unsettled swaps; retain them and the network guard.
	state := daemon.State{Version: 1, Network: chain.Mainnet, Mnemonic: seed, Swaps: map[string]*daemon.Swap{"pending": {ID: "pending", Stage: "funding broadcast", Secret: "test-only-secret"}}}
	if err := saveVault(backup, []byte(password), state); err != nil {
		t.Fatal(err)
	}
	pending := setupManager(t)
	if _, err := pending.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Pending", BackupPath: backup, BackupPassword: password, Revision: pending.settings.Revision}); err != nil {
		t.Fatal(err)
	}
	next := proto.Clone(pending.settings).(*pb.Settings)
	next.ActiveNetwork = "regtest"
	if _, err := pending.finishOnboarding(context.Background(), next); err == nil {
		t.Fatal("restored active swap network guard bypassed")
	}
	reexport := filepath.Join(t.TempDir(), "preserved.blakeswap")
	if _, err := pending.exportFirstWallet(context.Background(), &pb.ExportFirstWalletRequest{Path: reexport, Password: password, Revision: pending.settings.Revision}); err != nil {
		t.Fatal(err)
	}
	recovered, err := readStateBackup(pending.root, reexport, password)
	if err != nil || recovered.Swaps["pending"].Secret != "test-only-secret" {
		t.Fatal("pending swap state lost", err)
	}
}
func TestLegacyInstallationDoesNotRequireOnboarding(t *testing.T) {
	m := setupManager(t)
	legacy := configuredDefaults()
	if err := saveSettings(m.root, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(m.root)
	if err != nil || loaded.OnboardingStage != "" {
		t.Fatal("existing installation forced through setup", err)
	}
}
