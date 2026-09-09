package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type backupMark struct{ Fingerprint, SemanticToken string }
type streamWallet struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Identity string          `json:"identity"`
	Mnemonic string          `json:"mnemonic"`
	Networks []chain.Network `json:"networks"`
}
type streamInventory struct {
	FormatVersion int            `json:"format_version"`
	CreatedAt     int64          `json:"created_at"`
	Wallets       []streamWallet `json:"wallets"`
}
type streamNetwork struct {
	Wallet  string               `json:"wallet"`
	Network chain.Network        `json:"network"`
	State   *daemon.State        `json:"state"`
	Archive storage.ArchiveStats `json:"archive"`
}

func (m backupManifest) close() {
	if m.release != nil {
		m.release()
	}
}
func (w backupWallet) networkState(n chain.Network) (*daemon.State, error) {
	if state := w.Networks[n]; state != nil {
		return state, nil
	}
	if source := w.sources[n]; source != nil {
		return source.complete()
	}
	return nil, nil
}

// Temporary snapshots use private encrypted clones or a fresh random staging
// key, with generated filenames. One network is decoded at a time. Every
// caller owns close(), including cancellation and inspection-only paths.
type portableStaging struct {
	root           string
	password       []byte
	count          int
	clonePasswords [][]byte
	once           sync.Once
}

func newPortableStaging(root string) (*portableStaging, error) {
	directory, err := os.MkdirTemp(root, ".portable-snapshot-")
	if err != nil {
		return nil, err
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	password := []byte(hex.EncodeToString(random))
	clear(random)
	return &portableStaging{root: directory, password: password}, nil
}
func (s *portableStaging) close() {
	s.once.Do(func() {
		clear(s.password)
		for _, password := range s.clonePasswords {
			clear(password)
		}
		_ = os.RemoveAll(s.root)
	})
}

// Called only after acquiring the process's exclusive data-directory lock.
// These names were never published profiles. Clones retain source encryption,
// so reserved private directories must be cleaned even though the old process
// lost its copied credentials. Published wallet-* recovery markers survive.
func cleanupPortableStaging(root string) error {
	for _, location := range []struct {
		path     string
		prefixes []string
	}{
		{root, []string{".portable-snapshot-", ".restore-"}},
		{filepath.Join(root, "wallets"), []string{".import-"}},
	} {
		entries, err := os.ReadDir(location.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			for _, prefix := range location.prefixes {
				if strings.HasPrefix(entry.Name(), prefix) {
					if err := os.RemoveAll(filepath.Join(location.path, entry.Name())); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return nil
}
func writeStreamManifest(ctx context.Context, path string, password []byte, manifest backupManifest) error {
	inventory := streamInventory{FormatVersion: 1, CreatedAt: manifest.CreatedAt}
	for _, wallet := range manifest.Wallets {
		item := streamWallet{ID: wallet.ID, Name: wallet.Name, Identity: wallet.Identity, Mnemonic: wallet.Mnemonic}
		for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
			if _, ok := wallet.Networks[network]; ok {
				item.Networks = append(item.Networks, network)
			}
		}
		inventory.Wallets = append(inventory.Wallets, item)
	}
	return storage.WritePortableStream(ctx, path, password, func(w io.Writer) error {
		if err := storage.WriteJSONRecord(ctx, w, inventory); err != nil {
			return err
		}

		for i, wallet := range manifest.Wallets {
			for _, network := range inventory.Wallets[i].Networks {
				writeNetwork := func(state daemon.State, stats storage.ArchiveStats, visit func(func(storage.ArchiveRecord) error) error) error {
					if err := validateStreamedActive(&state, stats); err != nil {
						return err
					}
					if err := storage.WriteJSONRecord(ctx, w, streamNetwork{Wallet: wallet.ID, Network: network, State: &state, Archive: stats}); err != nil {
						return err
					}
					return visit(func(record storage.ArchiveRecord) error { return storage.WriteJSONRecord(ctx, w, record) })
				}
				if source := wallet.sources[network]; source != nil {
					if err := source.withView(func(view *storage.ReadSnapshot) error {
						var state daemon.State
						stats, _, err := view.LoadState(&state)
						if err != nil {
							return err
						}
						return writeNetwork(state, stats, func(each func(storage.ArchiveRecord) error) error { return view.VisitArchive(ctx, each) })
					}); err != nil {
						return err
					}
				} else {
					state := wallet.Networks[network]
					if state == nil {
						return errors.New("portable snapshot omits a declared network")
					}
					active := *state
					active.Archive = nil
					stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
					if active.Capacity != nil {
						stats = active.Capacity.Archived
					}
					if err := writeNetwork(active, stats, func(each func(storage.ArchiveRecord) error) error {
						for _, record := range state.Archive {
							if err := each(record); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
func readStreamManifest(ctx context.Context, root, path string, password []byte) (backupManifest, error) {
	result := backupManifest{FormatVersion: 1}
	staging, err := newPortableStaging(root)
	if err != nil {
		return result, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			staging.close()
		}
	}()
	err = storage.ReadPortableStream(ctx, path, password, func(r io.Reader) error {
		decoder := storage.NewJSONRecordDecoder(r)
		var inventory streamInventory
		if err := decoder.Decode(ctx, &inventory); err != nil {
			return err
		}
		if inventory.FormatVersion != 1 || len(inventory.Wallets) == 0 || len(inventory.Wallets) > 20 {
			return errors.New("unsupported portable inventory")
		}
		result.CreatedAt = inventory.CreatedAt
		for _, item := range inventory.Wallets {
			wallet := backupWallet{ID: item.ID, Name: item.Name, Identity: item.Identity, Mnemonic: item.Mnemonic, Networks: map[chain.Network]*daemon.State{}, sources: map[chain.Network]*backupNetwork{}}
			if len(item.Networks) == 0 || len(item.Networks) > 3 {
				return errors.New("invalid portable network inventory")
			}
			for _, network := range item.Networks {
				if network == "" || !network.Valid() {
					return errors.New("invalid portable network identity")
				}
				if _, duplicate := wallet.Networks[network]; duplicate {
					return errors.New("duplicate portable network identity")
				}
				// Placeholders let the same manifest identity validator run before states.
				wallet.Networks[network] = &daemon.State{Version: daemon.StateVersion, Network: network, Mnemonic: item.Mnemonic}
			}
			result.Wallets = append(result.Wallets, wallet)
		}
		if err := validateBackupManifest(&result); err != nil {
			return err
		}
		for i, item := range inventory.Wallets {
			for _, network := range item.Networks {
				var record streamNetwork
				if err := decoder.Decode(ctx, &record); err != nil {
					return err
				}
				if record.Wallet != item.ID || record.Network != network || record.State == nil {
					return errors.New("portable network ordering or completeness mismatch")
				}
				if record.State.Mnemonic != item.Mnemonic || record.State.Network.Normalized() != network || (record.State.Version != daemon.StateVersion) {
					return errors.New("portable network state does not match inventory")
				}
				source, err := staging.saveStream(ctx, *record.State, record.Archive, func(write func(storage.ArchiveRecord) error) error {
					for n := uint64(0); n < record.Archive.Count; n++ {
						var archived storage.ArchiveRecord
						if err := decoder.Decode(ctx, &archived); err != nil {
							return err
						}
						if err := write(archived); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
				result.Wallets[i].Networks[network] = nil
				result.Wallets[i].sources[network] = source
			}
		}
		return decoder.End()
	})
	if err != nil {
		return backupManifest{}, err
	}
	result.release = staging.close
	succeeded = true
	return result, nil
}

func validateBackupInventory(manifest backupManifest) error {
	metadata := backupManifest{FormatVersion: 1, CreatedAt: manifest.CreatedAt}
	for _, wallet := range manifest.Wallets {
		entry := backupWallet{ID: wallet.ID, Name: wallet.Name, Identity: wallet.Identity, Mnemonic: wallet.Mnemonic, Networks: map[chain.Network]*daemon.State{}}
		for network := range wallet.Networks {
			entry.Networks[network] = &daemon.State{Version: daemon.StateVersion, Network: network, Mnemonic: wallet.Mnemonic}
		}
		metadata.Wallets = append(metadata.Wallets, entry)
	}
	return validateBackupManifest(&metadata)
}
