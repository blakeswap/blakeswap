package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Restore core obligations into the active recovery state while copying cold
// history recordwise. Both passes read one pinned, completely verified source.
// The destination remains private until all transformed records validate.
func (n *backupNetwork) restore(ctx context.Context, path string, password []byte, snapshotAt int64, legacy bool) error {
	return n.withView(func(view *storage.ReadSnapshot) error {
		var active daemon.State
		originalStats, _, err := view.LoadState(&active)
		if err != nil {
			return err
		}
		if err = validateStreamedActive(&active, originalStats); err != nil {
			return err
		}
		stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
		err = view.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
			if err := validateStreamedRecord(active, record); err != nil {
				return err
			}
			promoted, err := daemon.PromoteRecoveryRecord(&active, record)
			if err != nil || promoted {
				return err
			}
			record, _ = daemon.QuarantineArchiveRecord(record)
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			size := uint64(len(raw) + 1)
			clear(raw)
			if stats.Count == math.MaxUint64 || size > math.MaxUint64-stats.Bytes {
				return errors.New("restored archive accounting overflow")
			}
			stats.Count++
			stats.Bytes += size
			stats.Kinds[record.Kind]++
			return nil
		})
		if err != nil {
			return err
		}
		if active.Capacity == nil {
			active.Capacity = &daemon.CapacityRecord{}
		}
		active.Capacity.Archived = stats
		if err = daemon.PrepareStreamedRecovery(ctx, &active, stats, view, snapshotAt, legacy); err != nil {
			return err
		}
		if err = validateStreamedActive(&active, stats); err != nil {
			return err
		}
		vault, err := storage.Open(path, password)
		if err != nil {
			return err
		}
		err = vault.ImportArchive(ctx, active, stats, func(write func(storage.ArchiveRecord) error) error {
			return view.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
				cold, keep := daemon.QuarantineArchiveRecord(record)
				if !keep {
					return nil
				}
				if err := validateStreamedRecord(active, cold); err != nil {
					return err
				}
				return write(cold)
			})
		})
		if err == nil {
			err = daemon.ValidateVaultProtocolStateContext(ctx, vault, &active)
		}
		closeErr := vault.Close()
		if err == nil {
			err = closeErr
		}
		return err
	})
}
