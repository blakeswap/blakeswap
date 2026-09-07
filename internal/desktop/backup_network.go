package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func (s *portableStaging) saveView(ctx context.Context, active daemon.State, stats storage.ArchiveStats, view *storage.ReadSnapshot) (*backupNetwork, backupMark, error) {
	mark := backupMark{SemanticToken: daemon.BackupSemanticToken(active)}
	digest := sha256.New()
	if err := storage.WriteJSONRecord(ctx, digest, struct {
		Domain  string               `json:"domain"`
		State   daemon.State         `json:"state"`
		Archive storage.ArchiveStats `json:"archive"`
	}{"blakeswap/complete-snapshot/v2", active, stats}); err != nil {
		return nil, mark, err
	}
	source, err := s.saveStream(ctx, active, stats, func(write func(storage.ArchiveRecord) error) error {
		return view.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
			if err := storage.WriteJSONRecord(ctx, digest, record); err != nil {
				return err
			}
			return write(record)
		})
	})
	if err != nil {
		return nil, mark, err
	}
	// This is the exact complete snapshot identity, not a freshness aggregate.
	// Location-only moves and polling changes preserve the semantic token.
	mark.Fingerprint = hex.EncodeToString(digest.Sum(nil))
	if stats.Count == 0 {
		mark.Fingerprint, err = daemon.BackupFingerprint(active)
	} else if mark.SemanticToken == "" {
		err = errors.New("archived snapshot lacks a checked semantic token")
	}
	return source, mark, err
}

// The provider owns a private immutable encrypted vault, not a decoded history
// graph. Its key remains scoped to the parent portableStaging lifetime.
type backupNetwork struct {
	path     string
	password []byte
}

func (n *backupNetwork) withView(visit func(*storage.ReadSnapshot) error) error {
	vault, err := storage.Open(n.path, n.password)
	if err != nil {
		return err
	}
	defer vault.Close()
	view, err := vault.Freeze()
	if err != nil {
		return err
	}
	defer view.Close()
	return visit(view)
}
func (n *backupNetwork) complete() (*daemon.State, error) {
	vault, err := storage.Open(n.path, n.password)
	if err != nil {
		return nil, err
	}
	defer vault.Close()
	state, err := daemon.LoadCompleteState(vault)
	return &state, err
}
func validateStreamedActive(state *daemon.State, stats storage.ArchiveStats) error {
	if state == nil || len(state.Archive) != 0 || (state.Version != 1 && state.Version != 2) || !state.Network.Valid() {
		return errors.New("streamed checkpoint must separate its archive records")
	}
	if err := state.ValidateArchiveCheckpoint(stats); err != nil {
		return err
	}
	if stats.Count > 0 && state.Version != 2 {
		return errors.New("archived checkpoint requires versioned state")
	}
	return validateActiveBackupState(state)
}
func validateStreamedRecord(state daemon.State, record storage.ArchiveRecord) error {
	partial, err := daemon.ValidateArchiveRecordAgainstState(state, record)
	if err != nil {
		return err
	}
	return validateActiveBackupState(&partial)
}
func (s *portableStaging) saveStream(ctx context.Context, active daemon.State, stats storage.ArchiveStats, produce func(func(storage.ArchiveRecord) error) error) (*backupNetwork, error) {
	if err := validateStreamedActive(&active, stats); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, fmt.Sprintf("%d.db", s.count))
	s.count++
	vault, err := storage.Open(path, s.password)
	if err != nil {
		return nil, err
	}
	err = vault.ImportArchive(ctx, active, stats, func(write func(storage.ArchiveRecord) error) error {
		return produce(func(record storage.ArchiveRecord) error {
			if err := validateStreamedRecord(active, record); err != nil {
				return err
			}
			return write(record)
		})
	})
	closeErr := vault.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return &backupNetwork{path: path, password: s.password}, nil
}
func (s *portableStaging) save(state *daemon.State) (*backupNetwork, error) {
	if err := daemon.ValidateArchiveState(*state); err != nil {
		return nil, err
	}
	active := *state
	active.Archive = nil
	stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
	if active.Capacity != nil {
		stats = active.Capacity.Archived
	}
	return s.saveStream(context.Background(), active, stats, func(write func(storage.ArchiveRecord) error) error {
		for _, record := range state.Archive {
			if err := write(record); err != nil {
				return err
			}
		}
		return nil
	})
}
