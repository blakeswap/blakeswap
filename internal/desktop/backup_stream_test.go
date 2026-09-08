package desktop

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestPortableStreamRestoresEveryNetworkWithoutRetainingWholeManifest(t *testing.T) {
	m := installedManager(t)
	manifest := portableManifest(t)
	path := filepath.Join(t.TempDir(), "stream.backup")
	password := "a separately chosen streaming password"
	if err := writeStreamManifest(context.Background(), path, []byte(password), manifest); err != nil {
		t.Fatal(err)
	}
	read, legacy, err := readBackupManifest(context.Background(), m.root, path, password)
	if err != nil || legacy {
		t.Fatal(err, legacy)
	}
	for network, want := range manifest.Wallets[0].Networks {
		if read.Wallets[0].Networks[network] != nil {
			t.Fatal("reader retained every decoded network")
		}
		got, err := read.Wallets[0].networkState(network)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Fatal("network state changed", network)
		}
	}
	read.close()
	if files, _ := filepath.Glob(filepath.Join(m.root, ".portable-snapshot-*")); len(files) != 0 {
		t.Fatal("inspection staging remains", files)
	}
	contents, err := m.inspectPortable(context.Background(), &pb.InspectBackupRequest{Path: path, Password: password})
	if err != nil || len(contents.Wallets) != 1 || len(contents.Wallets[0].Networks) != 3 {
		t.Fatal(contents, err)
	}
	result, err := m.importPortable(context.Background(), portableImportRequest{Path: path, Password: password, Revision: m.settings.Revision})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(m.root, "wallets", result.ProfileID)
	_, key, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		state, err := readStateBackupBounded(m.root, filepath.Join(root, string(network), "state.db"), string(key), portableVaultLimit)
		if err != nil || state.Recovery == nil || state.Recovery.Status.State != "recovering" || state.Swaps["swap"].SelfRefunds[0] != "saved refund" || len(state.Activities["receive/known"].History) != 1 {
			t.Fatal("lost gated retained network", network, err)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(m.root, ".portable-snapshot-*")); len(files) != 0 {
		t.Fatal("import staging remains", files)
	}
}
func TestPortableStreamRejectsMissingDuplicateAndMisboundNetworksBeforeInstall(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "identity", "extra"} {
		t.Run(kind, func(t *testing.T) {
			m := installedManager(t)
			manifest := portableManifest(t)
			wallet := manifest.Wallets[0]
			path := filepath.Join(t.TempDir(), "bad.backup")
			password := []byte("a separately chosen streaming password")
			err := storage.WritePortableStream(context.Background(), path, password, func(w io.Writer) error {
				inventory := streamInventory{FormatVersion: 2, CreatedAt: manifest.CreatedAt, Wallets: []streamWallet{{ID: wallet.ID, Name: wallet.Name, Identity: wallet.Identity, Mnemonic: wallet.Mnemonic, Networks: []chain.Network{chain.Regtest, chain.Testnet}}}}
				if kind == "duplicate" {
					inventory.Wallets[0].Networks[1] = chain.Regtest
				}
				if err := storage.WriteJSONRecord(context.Background(), w, inventory); err != nil {
					return err
				}
				for i, network := range []chain.Network{chain.Regtest, chain.Testnet} {
					if kind == "missing" && i == 1 {
						break
					}
					id := wallet.ID
					if kind == "identity" {
						id = "wrong-wallet"
					}
					if err := storage.WriteJSONRecord(context.Background(), w, streamNetwork{Wallet: id, Network: network, State: wallet.Networks[network]}); err != nil {
						return err
					}
				}
				if kind == "extra" {
					return storage.WriteJSONRecord(context.Background(), w, daemon.State{})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir(filepath.Join(m.root, "wallets"))
			if _, err := m.importPortable(context.Background(), portableImportRequest{Path: path, Password: string(password), Revision: m.settings.Revision}); err == nil {
				t.Fatal("invalid stream imported")
			}
			after, _ := os.ReadDir(filepath.Join(m.root, "wallets"))
			if len(after) != len(before) || len(m.settings.Wallets) != 1 {
				t.Fatal("invalid stream changed installation")
			}
			if files, _ := filepath.Glob(filepath.Join(m.root, ".portable-snapshot-*")); len(files) != 0 {
				t.Fatal("failed staging remains", files)
			}
		})
	}
}
