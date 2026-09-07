package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

func portableManifest(t *testing.T) backupManifest {
	t.Helper()
	mnemonic, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	id, err := backupIdentity(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	profile := backupWallet{ID: "source-profile", Name: "Personal", Identity: id, Mnemonic: mnemonic, Networks: map[chain.Network]*daemon.State{}}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		profile.Networks[network] = &daemon.State{Version: 1, Network: network, Mnemonic: mnemonic, ReceiveIndexes: map[chain.ID]uint32{chain.BTC: 12, chain.Blake: 37}, Swaps: map[string]*daemon.Swap{"swap": {ID: "swap", Role: "maker", Secret: "private preimage", LongFunding: "saved transaction", SelfRefunds: []string{"saved refund"}}}}
		state := profile.Networks[network]
		state.ActivityVersion, state.ActivityRevision, state.ActivityObservationSequence = 1, 3, 9
		state.Activities = map[string]daemon.Activity{"receive/known": {Version: 1, ID: "receive/known", Wallet: profile.ID, Network: network, Kind: "receive", Chain: chain.BTC, TxID: "transaction", Variants: []string{"transaction"}, Status: "confirmed", Confirmations: 2, Observations: []daemon.ActivityObservation{{Sequence: 9, TxID: "transaction", Status: "confirmed", Height: 10, BlockHash: "current-block", Source: "verified-source", Generation: 1}}, History: []daemon.ActivityOutcome{{TxID: "transaction", Status: "orphaned", BlockHash: "old-block", Source: "prior-source", Generation: 1}}}}
		state.ActivityReceipts = map[string]daemon.ReceiptEvidence{"transaction": {Inputs: []daemon.CoinOutpoint{{TxID: "parent", Vout: 1}}, Total: 200000, OwnedTotal: 150000}}
		state.ActivityIndexes = map[chain.ID]daemon.ActivityIndex{chain.BTC: {Address: 7, After: "cursor", Source: "verified-source", Generation: 1, CompletedPass: 100}}
	}
	return backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []backupWallet{profile}}
}

func TestPortableSnapshotSelectedOrAllProfiles(t *testing.T) {
	m := setupManager(t)
	first := prepare(t, m)
	otherRoot := filepath.Join(m.root, "wallets", "wallet-second")
	otherSeed, _, err := master(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	m.settings.Wallets = append(m.settings.Wallets, &pb.WalletProfile{Id: "wallet-second", Name: "Second"})
	selected, err := m.backupSnapshotLocked(context.Background(), "alice", false)
	if err != nil || len(selected.Wallets) != 1 || len(selected.Wallets[0].Networks) != 3 || selected.Wallets[0].Mnemonic != first.Recovery.Mnemonic {
		t.Fatal("selected profile snapshot is incomplete", err)
	}
	all, err := m.backupSnapshotLocked(context.Background(), "alice", true)
	if err != nil || len(all.Wallets) != 2 || all.Wallets[1].Mnemonic != otherSeed || len(all.Wallets[1].Networks) != 3 {
		t.Fatal("all profiles snapshot is incomplete", err)
	}
	if _, err := m.backupSnapshotLocked(context.Background(), "missing", false); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestPortableManifestPreservesAllNetworkState(t *testing.T) {
	manifest := portableManifest(t)
	if err := validateBackupManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "all-networks.blakeswap")
	password := []byte("chosen portable password")
	if err := storage.WritePortable(context.Background(), path, password, manifest); err != nil {
		t.Fatal(err)
	}
	var recovered backupManifest
	if err := storage.ReadPortable(context.Background(), path, password, &recovered); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupManifest(&recovered); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("archive lost private or per-network recovery state")
	}
}

func TestPortableManifestRejectsIdentityAndNetworkMismatch(t *testing.T) {
	for name, corrupt := range map[string]func(*backupManifest){
		"duplicate identity": func(m *backupManifest) {
			other := m.Wallets[0]
			other.ID = "different-profile"
			m.Wallets = append(m.Wallets, other)
		},
		"identity mismatch": func(m *backupManifest) { m.Wallets[0].Identity = "different identity" },
		"network mismatch":  func(m *backupManifest) { m.Wallets[0].Networks[chain.Regtest].Network = chain.Mainnet },
		"seed mismatch":     func(m *backupManifest) { m.Wallets[0].Networks[chain.Regtest].Mnemonic = "different seed" },
		"nil state":         func(m *backupManifest) { m.Wallets[0].Networks[chain.Regtest] = nil },
		"nil swap":          func(m *backupManifest) { m.Wallets[0].Networks[chain.Regtest].Swaps["swap"] = nil },
		"activity network mismatch": func(m *backupManifest) {
			a := m.Wallets[0].Networks[chain.Regtest].Activities["receive/known"]
			a.Network = chain.Mainnet
			m.Wallets[0].Networks[chain.Regtest].Activities[a.ID] = a
		},
		"activity id mismatch": func(m *backupManifest) {
			a := m.Wallets[0].Networks[chain.Regtest].Activities["receive/known"]
			a.ID = "different"
			m.Wallets[0].Networks[chain.Regtest].Activities["receive/known"] = a
		},
		"future format": func(m *backupManifest) { m.FormatVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := portableManifest(t)
			corrupt(&manifest)
			if err := validateBackupManifest(&manifest); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestPortableAndLegacyReaderKeepSourceUnchanged(t *testing.T) {
	ctx := context.Background()
	manifest := portableManifest(t)
	root := t.TempDir()
	password := "a portable test password"
	for _, legacy := range []bool{false, true} {
		name := "portable.blakeswap"
		if legacy {
			name = "legacy.db"
		}
		path := filepath.Join(root, name)
		var err error
		if legacy {
			err = saveVault(path, []byte(password), manifest.Wallets[0].Networks[chain.Regtest])
		} else {
			err = storage.WritePortable(ctx, path, []byte(password), manifest)
		}
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := readBackupManifest(ctx, root, path, "the wrong long password"); err == nil {
			t.Fatal("wrong password accepted")
		}
		restored, oldFormat, err := readBackupManifest(ctx, root, path, password)
		if err != nil || oldFormat != legacy || len(restored.Wallets) != 1 || restored.Wallets[0].Identity != manifest.Wallets[0].Identity {
			t.Fatal("cannot recover source", err, oldFormat)
		}
		state := restored.Wallets[0].Networks[chain.Regtest]
		if state.ReceiveIndexes[chain.Blake] != 37 || state.Swaps["swap"].SelfRefunds[0] != "saved refund" {
			t.Fatal("lost recovery state")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("source changed", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := readBackupManifest(canceled, root, path, password); err == nil {
			t.Fatal("canceled import read accepted")
		}
	}
}
