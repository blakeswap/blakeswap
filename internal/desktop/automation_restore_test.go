package desktop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"google.golang.org/protobuf/proto"
)

// Import must revoke stale automatic authority before any engine starts, for
// legacy files as well as the portable path that prepares the same private copy.
func TestImportedAutomationNeverResumesOldSpendingAuthority(t *testing.T) {
	for _, format := range []string{"legacy", "portable"} {
		t.Run(format, func(t *testing.T) {
			m := setupManager(t)
			state := daemon.State{Version: 1, Network: chain.Regtest, Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"}
			id := strings.Repeat("ab", 32)
			keys, err := wallet.FromMnemonic(state.Mnemonic)
			if err != nil {
				t.Fatal(err)
			}
			keys.SetNetwork(chain.Regtest)
			key, err := keys.Derive(2, "nostr-identity")
			if err != nil {
				t.Fatal(err)
			}
			identity := nostr.SecretKey(key.Serialize())
			state.Automations = map[string]*daemon.AutomationPolicy{id: {Config: daemon.AutomationConfig{ID: id, Wallet: "alice", Network: chain.Regtest, Sell: chain.Blake, SellAmount: 1000000, VolumeLimit: 3000000, Rate: daemon.AutomationRate{Numerator: 1, Denominator: 2}, MinRate: daemon.AutomationRate{Numerator: 1, Denominator: 4}, MaxRate: daemon.AutomationRate{Numerator: 1, Denominator: 1}, Lifetime: 120, Cadence: 60, MaxOpen: 1, FundingFee: 6500, MaxFundingFee: 10000, BTCFeeBudget: 100000, BlakeFeeBudget: 100000, Reference: "fixed"}, WalletKey: identity.Public().Hex(), Enabled: true, Revision: 5, NextAction: 1, CurrentOfferID: "source", Charges: map[string]*daemon.AutomationCharge{"funded": {OfferID: "funded", Volume: 1000000, BTCFees: 26500, BlakeFees: 20000, State: "committed", Successor: "source"}, "source": {OfferID: "source", Volume: 1000000, BTCFees: 26500, BlakeFees: 20000, State: "reserved"}}, Pending: &daemon.ConfirmTradeRequest{RequestID: "pending", ExpectedWallet: "alice", ExpectedNetwork: "regtest"}}}
			state.TradeReceipts = map[string]*daemon.TradeReceipt{"pending": {AutomationID: id, AutomationRevision: 5, Digest: "pending-original", Result: daemon.ConfirmTradeResult{ID: "pending", State: "pending"}}, "accepted": {AutomationID: id, AutomationRevision: 4, Digest: "accepted-original", Result: daemon.ConfirmTradeResult{ID: "accepted", State: "accepted"}}}
			backup := filepath.Join(t.TempDir(), "old-wallet.db")
			password := []byte("isolated-regression-password")
			if format == "legacy" {
				if err := saveVault(backup, password, state); err != nil {
					t.Fatal(err)
				}
			} else {
				identity, err := backupIdentity(state.Mnemonic)
				if err != nil {
					t.Fatal(err)
				}
				manifest := backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []backupWallet{{ID: "old-profile", Name: "Original", Identity: identity, Mnemonic: state.Mnemonic, Networks: map[chain.Network]*daemon.State{chain.Regtest: &state}}}}
				if err := storage.WritePortable(context.Background(), backup, password, manifest); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Restored", BackupPath: backup, BackupPassword: string(password), Revision: m.settings.Revision}); err != nil {
				t.Fatal(err)
			}
			_, secret, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
			if err != nil {
				t.Fatal(err)
			}
			defer clear(secret)
			vault, err := storage.Open(filepath.Join(m.root, "wallets", "alice", "regtest", "state.db"), secret)
			if err != nil {
				t.Fatal(err)
			}
			defer vault.Close()
			var installed daemon.State
			if _, err = vault.Load(&installed); err != nil {
				t.Fatal(err)
			}
			p := installed.Automations[id]
			if p == nil || p.Enabled || !p.RestoreHold || p.Revision <= 5 {
				t.Fatal("import retained live automatic spending authorization", p)
			}
			if p.Charges["funded"].State != "committed" || p.Charges["funded"].Successor != "source" || p.Charges["source"].State != "reserved" || !p.Charges["source"].Uncertain {
				t.Fatal("import changed recorded/uncertain policy budget")
			}
			if installed.TradeReceipts["pending"].Digest != "pending-original" || installed.TradeReceipts["accepted"].Digest != "accepted-original" || installed.TradeReceipts["accepted"].Result.State != "accepted" {
				t.Fatal("import erased or reinterpreted accepted request identity")
			}
		})
	}
}

func TestAutomationMalformedBackupRejectedBeforeInstallation(t *testing.T) {
	for _, format := range []string{"legacy", "portable"} {
		for _, mode := range []string{"nil-policy", "nil-charge-map", "nil-charge", "strategy-empty-reference"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				m := setupManager(t)
				state := daemon.State{Version: 1, Network: chain.Regtest, Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about", Automations: map[string]*daemon.AutomationPolicy{"policy": {Config: daemon.AutomationConfig{ID: "policy"}, Charges: map[string]*daemon.AutomationCharge{}}}}
				switch mode {
				case "nil-policy":
					state.Automations["policy"] = nil
				case "nil-charge-map":
					state.Automations["policy"].Charges = nil
				case "nil-charge":
					state.Automations["policy"].Charges["offer"] = nil
				case "strategy-empty-reference":
					side := daemon.StrategySide{Target: 2000000, MinimumReserve: 500000, MaxExposure: 1000000, MinOffer: 100000, MaxOffer: 200000, VolumeLimit: 1000000, FundingFee: 2000, MaxFundingFee: 4000}
					c := daemon.StrategyConfig{ID: strings.Repeat("a", 64), Wallet: "alice", Network: chain.Regtest, BTC: side, Blake: side, Rate: daemon.AutomationRate{Numerator: 1, Denominator: 1}, MinRate: daemon.AutomationRate{Numerator: 1, Denominator: 2}, MaxRate: daemon.AutomationRate{Numerator: 2, Denominator: 1}, SpreadBPS: 100, MinSpreadBPS: 50, MaxSpreadBPS: 200, SkewBPS: 50, Lifetime: 120, Cadence: 60, MaxConcurrent: 4, BTCFeeBudget: 300000, BlakeFeeBudget: 300000, Reference: "fixed", MaxConsecutiveFailures: 3, MaxReplacementFailures: 2, FailureRateBPS: 8000}
					parent := &daemon.MakerStrategy{Config: c, WalletKey: strings.Repeat("1", 64), Revision: 1, Enabled: true}
					state.MakerStrategies = map[string]*daemon.MakerStrategy{c.ID: parent}
					state.Automations = map[string]*daemon.AutomationPolicy{}
					for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
						id := protocol.Digest([]string{"maker-strategy-v1", c.ID, string(sell)})
						child := daemon.AutomationConfig{ID: id, StrategyID: c.ID, Wallet: c.Wallet, Network: c.Network, Sell: sell, SellAmount: side.MaxOffer, VolumeLimit: side.VolumeLimit, Rate: c.Rate, MinRate: c.MinRate, MaxRate: c.MaxRate, Lifetime: c.Lifetime, Cadence: c.Cadence, MaxOpen: c.MaxConcurrent, FundingFee: side.FundingFee, MaxFundingFee: side.MaxFundingFee, BTCFeeBudget: c.BTCFeeBudget, BlakeFeeBudget: c.BlakeFeeBudget, Reference: c.Reference}
						state.Automations[id] = &daemon.AutomationPolicy{Config: child, WalletKey: parent.WalletKey, Revision: 1, Enabled: true, Charges: map[string]*daemon.AutomationCharge{}}
					}
					if err := daemon.ValidateAutomationState(&state); err != nil {
						t.Fatal("valid strategy control", err)
					}
					parent.Config.Reference = "orderbook"
					parent.Config.ReferenceFreshness, parent.Config.ReferenceSpreadBPS = 60, 20
					for _, child := range state.Automations {
						child.Config.Reference = "orderbook"
						child.Config.ReferenceFreshness, child.Config.ReferenceSpreadBPS = 60, 20
					}
				}
				path := filepath.Join(t.TempDir(), "invalid.blakeswap")
				password := []byte("malformed archive test password")
				if format == "legacy" {
					if err := saveVault(path, password, state); err != nil {
						t.Fatal(err)
					}
				} else {
					identity, err := backupIdentity(state.Mnemonic)
					if err != nil {
						t.Fatal(err)
					}
					manifest := backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []backupWallet{{ID: "original", Name: "Original", Identity: identity, Mnemonic: state.Mnemonic, Networks: map[chain.Network]*daemon.State{chain.Regtest: &state}}}}
					if err := storage.WritePortable(context.Background(), path, password, manifest); err != nil {
						t.Fatal(err)
					}
				}
				source, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before := proto.Clone(m.settings)
				if _, err = m.prepareFirstWallet(context.Background(), &pb.PrepareFirstWalletRequest{Name: "Restored", BackupPath: path, BackupPassword: string(password), Revision: m.settings.Revision}); err == nil {
					t.Fatal("invalid automation installed")
				}
				if !proto.Equal(before, m.settings) {
					t.Fatal("rejected import changed settings")
				}
				if _, err = os.Lstat(filepath.Join(m.root, "wallets", "alice")); !os.IsNotExist(err) {
					t.Fatal("rejected import published profile", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(source, after) {
					t.Fatal("rejected import changed source archive", err)
				}
				// Post-onboarding import uses the same validator before creating a new
				// profile; a bad archive cannot disturb the already installed wallet.
				if format == "portable" && (mode == "nil-charge" || mode == "strategy-empty-reference") {
					existing := installedManager(t)
					before = proto.Clone(existing.settings)
					seed, secret, err := readMaster(filepath.Join(existing.root, "wallets", "alice"))
					clear(secret)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = existing.importPortable(context.Background(), portableImportRequest{Path: path, Password: string(password), Name: "Restored", Revision: existing.settings.Revision}); err == nil {
						t.Fatal("invalid additional profile installed")
					}
					restored, secret, err := readMaster(filepath.Join(existing.root, "wallets", "alice"))
					clear(secret)
					if err != nil || seed != restored || !proto.Equal(before, existing.settings) {
						t.Fatal("bad archive disturbed existing profile", err)
					}
				}
			})
		}
	}
}
