package desktop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Both formats enter through the same validated manifest. The legacy reader
// authenticates a private copy, and portable archives are opened read-only.
// Import callers must install the recovery gate before enabling any engine.
func readBackupManifest(ctx context.Context, root, source, password string) (backupManifest, bool, error) {
	var manifest backupManifest
	if !filepath.IsAbs(source) {
		return manifest, false, errors.New("choose an absolute backup file path")
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() > storage.PortableLimit+1024 {
		return manifest, false, errors.New("choose a regular backup file up to 256 MiB")
	}
	file, err := os.Open(source)
	if err != nil {
		return manifest, false, err
	}
	var prefix [len("BLAKESWAP-BACKUP\x00")]byte
	_, readErr := io.ReadFull(file, prefix[:])
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return manifest, false, errors.New("cannot read backup file")
	}
	if string(prefix[:]) == "BLAKESWAP-BACKUP\x00" {
		if err := storage.ReadPortable(ctx, source, []byte(password), &manifest); err != nil {
			return manifest, false, err
		}
		return manifest, false, validateBackupManifest(&manifest)
	}
	if err := ctx.Err(); err != nil {
		return manifest, true, err
	}
	state, err := readStateBackup(root, source, password)
	if err != nil {
		return manifest, true, err
	}
	identity, err := backupIdentity(state.Mnemonic)
	if err != nil {
		return manifest, true, err
	}
	// This timestamp describes conversion, not the legacy snapshot. The returned
	// legacy flag carries that unknown provenance into the recovery record.
	manifest = backupManifest{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []backupWallet{{
		ID: "legacy", Name: "Recovered wallet", Identity: identity, Mnemonic: state.Mnemonic,
		Networks: map[chain.Network]*daemon.State{state.Network.Normalized(): state},
	}}}
	return manifest, true, validateBackupManifest(&manifest)
}
