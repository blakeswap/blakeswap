package desktop

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

func TestBackupCutoverRejectsEmbeddedLegacyBeforeMutation(t *testing.T) {
	seed, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := backupIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 2, 3, daemon.StateVersion} {
		state := &daemon.State{Version: version, Network: chain.Regtest, Mnemonic: seed}
		manifest := backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []backupWallet{{ID: "cutover", Name: "Cutover", Mnemonic: seed, Identity: identity, Networks: map[chain.Network]*daemon.State{chain.Regtest: state}}}}
		before, _ := json.Marshal(manifest)
		checks := []func() error{
			func() error { return validateBackupManifest(&manifest) },
			func() error { return validateBackupState(state) },
			func() error { return validateStreamedActive(state, storage.ArchiveStats{}) },
			func() error {
				return validateStreamedRecord(*state, storage.ArchiveRecord{Kind: "seen", ID: "retained", Data: json.RawMessage(`"digest"`)})
			},
		}
		for i, check := range checks {
			err := check()
			if (err == nil) != (version == daemon.StateVersion) {
				t.Fatalf("state%d boundary%d: %v", version, i, err)
			}
			after, _ := json.Marshal(manifest)
			if !bytes.Equal(before, after) {
				t.Fatalf("state%d boundary%d changed source", version, i)
			}
		}
	}
}
