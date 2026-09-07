package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
	"google.golang.org/protobuf/proto"
)

func installedManager(t *testing.T) *Manager {
	t.Helper()
	m := setupManager(t)
	prepare(t, m)
	m.settings.OnboardingStage = ""
	if err := saveSettings(m.root, m.settings); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPortableImportIsolatedProfilePreservesExistingWallet(t *testing.T) {
	m := installedManager(t)
	beforeSeed, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
	clear(password)
	if err != nil {
		t.Fatal(err)
	}
	manifest := portableManifest(t)
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	const secret = "independent archive password"
	if err = storage.WritePortable(context.Background(), path, []byte(secret), manifest); err != nil {
		t.Fatal(err)
	}
	beforeFile, _ := os.ReadFile(path)
	request := portableImportRequest{Path: path, Password: secret, Name: "Recovered", Revision: m.settings.Revision}
	result, err := m.importPortable(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProfileID == "alice" || result.ProfileID == manifest.Wallets[0].ID || len(result.Settings.Wallets) != 2 {
		t.Fatal("import replaced profile", result)
	}
	afterSeed, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
	clear(password)
	if err != nil || beforeSeed != afterSeed {
		t.Fatal("original wallet changed", err)
	}
	profileRoot := filepath.Join(m.root, "wallets", result.ProfileID)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		_, password, err := readMaster(profileRoot)
		if err != nil {
			t.Fatal(err)
		}
		state, err := readStateBackup(m.root, filepath.Join(profileRoot, string(network), "state.db"), string(password))
		clear(password)
		if err != nil {
			t.Fatal(err)
		}
		if state.Recovery == nil || state.Recovery.Status.State != "recovering" || state.ReceiveIndexes[chain.Blake] != 37 || state.Swaps["swap"].SelfRefunds[0] != "saved refund" {
			t.Fatal("import lost gate or recovery state")
		}
		activity := state.Activities["receive/known"]
		if activity.Wallet != manifest.Wallets[0].ID || activity.Network != network || len(activity.History) != 1 || activity.History[0].BlockHash != "old-block" || activity.Observations[0].BlockHash != "current-block" || state.ActivityReceipts["transaction"].OwnedTotal != 150000 || state.ActivityIndexes[chain.BTC].After != "cursor" {
			t.Fatal("import lost encrypted per-network activity, receipt, or source evidence")
		}
	}
	request.Revision = m.settings.Revision
	if _, err = m.importPortable(context.Background(), request); err == nil {
		t.Fatal("duplicate identity imported")
	}
	afterFile, _ := os.ReadFile(path)
	if !bytes.Equal(beforeFile, afterFile) {
		t.Fatal("source archive changed")
	}
}

func TestPortableImportFailureDoesNotChangeInstallation(t *testing.T) {
	m := installedManager(t)
	manifest := portableManifest(t)
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	const secret = "independent archive password"
	if err := storage.WritePortable(context.Background(), path, []byte(secret), manifest); err != nil {
		t.Fatal(err)
	}
	before := proto.Clone(m.settings)
	entries, _ := os.ReadDir(filepath.Join(m.root, "wallets"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, attempt := range []struct {
		ctx      context.Context
		password string
	}{{context.Background(), "incorrect backup password"}, {ctx, secret}} {
		_, err := m.importPortable(attempt.ctx, portableImportRequest{Path: path, Password: attempt.password, Revision: m.settings.Revision})
		if err == nil {
			t.Fatal("invalid import succeeded")
		}
	}
	after, _ := os.ReadDir(filepath.Join(m.root, "wallets"))
	if !proto.Equal(before, m.settings) || len(entries) != len(after) {
		t.Fatal("failed import changed installation")
	}
}

func TestInterruptedPortableImportResumesOnlyCompleteGatedProfile(t *testing.T) {
	m := installedManager(t)
	manifest := portableManifest(t)
	target := filepath.Join(m.root, "wallets", "wallet-interrupted")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker, err := prepareImportedProfile(context.Background(), target, manifest.Wallets[0], "Interrupted", manifest.CreatedAt, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(marker)
	if err = writePrivate(filepath.Join(target, "import.json"), raw); err != nil {
		t.Fatal(err)
	}
	restored := proto.Clone(m.settings).(*pb.Settings)
	if err = recoverPreparedImports(m.root, restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Wallets) != 2 || restored.Wallets[1].Id != "wallet-interrupted" {
		t.Fatal("interrupted import was lost")
	}
	if err = recoverPreparedImports(m.root, restored); err != nil || len(restored.Wallets) != 2 {
		t.Fatal("restart duplicated profile", err)
	}
}

func TestInterruptedImportRejectsMissingGateAndDuplicateIdentity(t *testing.T) {
	for _, kind := range []string{"missing-gate", "duplicate-identity"} {
		t.Run(kind, func(t *testing.T) {
			m := installedManager(t)
			manifest := portableManifest(t)
			entry := manifest.Wallets[0]
			if kind == "duplicate-identity" {
				seed, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
				clear(password)
				if err != nil {
					t.Fatal(err)
				}
				entry.Mnemonic = seed
				entry.Identity, _ = backupIdentity(seed)
				for _, state := range entry.Networks {
					state.Mnemonic = seed
				}
			}
			target := filepath.Join(m.root, "wallets", "wallet-interrupted")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			marker, err := prepareImportedProfile(context.Background(), target, entry, "Interrupted", manifest.CreatedAt, false)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing-gate" {
				_, password, err := readMaster(target)
				if err != nil {
					t.Fatal(err)
				}
				state := entry.Networks[chain.Regtest]
				state.Recovery = nil
				err = saveVault(filepath.Join(target, "regtest", "state.db"), password, state)
				clear(password)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(marker)
			if err = writePrivate(filepath.Join(target, "import.json"), raw); err != nil {
				t.Fatal(err)
			}
			before := proto.Clone(m.settings)
			if err = recoverPreparedImports(m.root, m.settings); err == nil {
				t.Fatal("unsafe interrupted import activated")
			}
			if !proto.Equal(before, m.settings) {
				t.Fatal("failed validation changed settings")
			}
		})
	}
}

func TestLegacyImportUsesSameAllNetworkRecoveryGate(t *testing.T) {
	m := installedManager(t)
	entry := portableManifest(t).Wallets[0]
	path := filepath.Join(t.TempDir(), "legacy.db")
	const password = "legacy archive vault password"
	if err := saveVault(path, []byte(password), entry.Networks[chain.Regtest]); err != nil {
		t.Fatal(err)
	}
	result, err := m.importPortable(context.Background(), portableImportRequest{Path: path, Password: password, Revision: m.settings.Revision})
	if err != nil || !result.Legacy {
		t.Fatal(result, err)
	}
	root := filepath.Join(m.root, "wallets", result.ProfileID)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		_, key, err := readMaster(root)
		if err != nil {
			t.Fatal(err)
		}
		state, err := readStateBackup(m.root, filepath.Join(root, string(network), "state.db"), string(key))
		clear(key)
		if err != nil || state.Recovery == nil || !state.Recovery.Legacy || state.Recovery.Status.State != "recovering" {
			t.Fatal("legacy bypassed recovery gate", err)
		}
	}
}
