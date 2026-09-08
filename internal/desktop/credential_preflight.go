package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/blakeswap/blakeswap/internal/credential"
)

// Run under the desktop lifecycle owner before Migrate. This phase performs
// only bounded credential acquisition and readonly authenticated vault reads.
// Migrate repeats verification before activation/removing the legacy file, and
// Engine.Open revalidates the state under exclusive non-initializing ownership.
func preflightCredentialProfiles(ctx context.Context, root, installation string, store credential.Store) error {
	entries, err := os.ReadDir(filepath.Join(root, "wallets"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !walletID.MatchString(entry.Name()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		profileRoot := filepath.Join(root, "wallets", entry.Name())
		journal, journalErr := credential.ReadJournal(profileRoot)
		var password []byte
		switch {
		case errors.Is(journalErr, os.ErrNotExist):
			password, err = credential.File(filepath.Join(profileRoot, "vault.password")).Acquire(ctx)
		case journalErr != nil:
			return journalErr
		case journal.Key.Installation != installation || journal.Key.Profile != entry.Name():
			return credential.ErrConflict
		case journal.Phase == "active" || journal.Phase == "removed":
			// Native missing/denied is a hold, never a plaintext fallback.
			password, err = store.Get(ctx, journal.Key)
		case journal.Origin == "legacy":
			password, err = credential.File(filepath.Join(profileRoot, "vault.password")).Acquire(ctx)
		default:
			return errors.New("new wallet credential installation is incomplete")
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && (len(password) < 16 || len(password) > 4096) {
			err = errors.New("native credential length is invalid")
		}
		if err == nil {
			_, err = verifyProfile(profileRoot, password)
		}
		clear(password)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
