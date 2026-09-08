package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"google.golang.org/protobuf/proto"
)

func backupFixtureDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		t.Fatal(err)
	}
	return string(digest.Sum(nil))
}

func TestInterruptedPortableImportPreservesLargeActivityHistory(t *testing.T) {
	m := installedManager(t)
	manifest := portableManifest(t)
	entry := manifest.Wallets[0]
	state := entry.Networks[chain.Regtest]
	entry.Networks = map[chain.Network]*daemon.State{chain.Regtest: state}
	state.Activities = map[string]daemon.Activity{}
	txid := strings.Repeat("a", 64)
	// This remains below the existing 50,000-record activity capacity. Retained
	// prior outcomes make the portable snapshot larger than the legacy DB limit.
	const records = 40000
	for i := 0; i < records; i++ {
		id := fmt.Sprintf("receive/%064d/0", i)
		activity := daemon.Activity{Version: 1, ID: id, Wallet: entry.ID, Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Direction: "incoming", TxID: txid, Variants: []string{txid}, Status: "confirmed", Principal: 100000, Amount: 100000, Observations: []daemon.ActivityObservation{{TxID: txid, Status: "confirmed", Height: 100, BlockHash: txid, Source: "configured-endpoint", Generation: 1}}}
		for j := 0; j < 4; j++ {
			activity.History = append(activity.History, daemon.ActivityOutcome{Status: "unknown", TxID: txid, Amount: 100000, BlockHash: txid, Source: "configured-endpoint", Generation: 1})
		}
		state.Activities[id] = activity
	}
	manifest.Wallets = []backupWallet{entry}
	if err := validateBackupManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) <= 64<<20 || len(encoded) > storage.PortableLimit {
		t.Fatal("fixture outside portable capacity", len(encoded), err)
	}
	t.Logf("accepted portable activity manifest: %d bytes", len(encoded))
	encoded = nil
	target := filepath.Join(m.root, "wallets", "wallet-largeinterrupted")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker, err := prepareImportedProfile(context.Background(), target, entry, "Interrupted", manifest.CreatedAt, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(target, "import.json"), raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target, "regtest", "state.db")
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 64<<20 || info.Size() > portableVaultLimit {
		t.Fatal("fixture DB does not exercise portable reader", info, err)
	}
	before := backupFixtureDigest(t, path)
	// Startup after the directory commit must finish Settings exactly once.
	resumed, err := loadSettings(m.root)
	if err != nil {
		t.Fatal("portable prepared import blocked startup", err)
	}
	if len(resumed.Wallets) != 2 || resumed.Wallets[1].Id != "wallet-largeinterrupted" {
		t.Fatal("interrupted profile not recovered", resumed.Wallets)
	}
	again, err := loadSettings(m.root)
	if err != nil || !proto.Equal(resumed, again) {
		t.Fatal("second startup changed recovered profile", err)
	}
	seed, password, err := readMaster(target)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	if _, err = readStateBackup(m.root, path, string(password)); err == nil {
		t.Fatal("legacy source limit was relaxed")
	}
	restored, err := readStateBackupBounded(m.root, path, string(password), portableVaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Mnemonic != seed || len(restored.Activities) != records || restored.Recovery == nil || restored.Recovery.Status.State != "recovering" || !restored.Recovery.Swaps[fixtureChildID] {
		t.Fatal("recovery gate, identity or activity lost")
	}
	for _, i := range []int{0, records - 1} {
		activity := restored.Activities[fmt.Sprintf("receive/%064d/0", i)]
		if activity.Amount != 100000 || activity.TxID != txid || len(activity.History) != 4 || activity.History[3].BlockHash != txid {
			t.Fatal("retained activity outcomes changed", i)
		}
	}
	if backupFixtureDigest(t, path) != before {
		t.Fatal("startup validation modified installed vault")
	}
}

func TestPortableInstalledStateReaderRejectsOversizeWithoutCopy(t *testing.T) {
	const testValidationLimit = 1 << 20
	root := t.TempDir()
	source := filepath.Join(root, "oversize.db")
	f, err := os.OpenFile(source, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(testValidationLimit + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readStateBackupBounded(root, source, "isolated fixture password", testValidationLimit); err == nil {
		t.Fatal("oversize installed vault accepted")
	}
	info, err := os.Stat(source)
	if err != nil || info.Size() != testValidationLimit+1 {
		t.Fatal("source changed", err)
	}
	copies, err := filepath.Glob(filepath.Join(root, ".restore-*"))
	if err != nil || len(copies) != 0 {
		t.Fatal("oversize source copied before rejection", copies, err)
	}
}
