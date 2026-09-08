package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

// LoadState reads only the active checkpoint. Archive bodies remain in the
// pinned transaction and are consumed individually by VisitArchive.
func (s *ReadSnapshot) LoadState(out any) (ArchiveStats, uint64, error) {
	stats, err := s.vault.archiveStats(s.tx)
	if err != nil {
		return stats, 0, err
	}
	raw, err := s.vault.unseal(s.tx.Bucket(bucket).Get([]byte("state")), []byte("blakeswap/state/v1"))
	if err != nil {
		return stats, 0, err
	}
	defer clear(raw)
	return stats, uint64(len(raw)), json.Unmarshal(raw, out)
}
func archiveStatsEqual(a, b ArchiveStats) bool {
	if a.Count != b.Count || a.Bytes != b.Bytes {
		return false
	}
	for kind, count := range a.Kinds {
		if b.Kinds[kind] != count {
			return false
		}
	}
	for kind, count := range b.Kinds {
		if a.Kinds[kind] != count {
			return false
		}
	}
	return true
}

// VisitArchive borrows each decrypted record only for the callback duration.
// It verifies all records and the complete count/byte/category checkpoint before
// success. Callers retaining a record beyond the callback must copy its Data.
func (s *ReadSnapshot) VisitArchive(ctx context.Context, visit func(ArchiveRecord) error) error {
	expected, err := s.vault.archiveStats(s.tx)
	if err != nil {
		return err
	}
	actual := ArchiveStats{Kinds: map[string]uint64{}}
	if b := s.tx.Bucket(archiveBucket); b != nil {
		err = b.ForEach(func(index, sealed []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, size, err := s.vault.decodeArchive(index, sealed)
			if err != nil {
				return err
			}
			defer clear(record.Data)
			if actual.Count == math.MaxUint64 || actual.Bytes > math.MaxUint64-size {
				return errors.New("archive accounting overflow")
			}
			actual.Count++
			actual.Bytes += size
			actual.Kinds[record.Kind]++
			return visit(record)
		})
		if err != nil {
			return err
		}
	}
	if !archiveStatsEqual(expected, actual) {
		return errors.New("archive streaming completeness mismatch")
	}
	return nil
}

// ImportArchive is only for a newly created private staging vault. Records are
// committed in bounded batches so bbolt does not retain the entire ciphertext in
// one dirty transaction. The final active checkpoint is published in this vault
// only after every record, duplicate identity and declared total has validated.
// A caller must discard this whole staging vault after any error.
func (v *Vault) ImportArchive(ctx context.Context, state any, expected ArchiveStats, produce func(func(ArchiveRecord) error) error) error {
	var old json.RawMessage
	if _, err := v.Load(&old); err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(old), []byte("{}")) {
		return errors.New("archive import requires a new private staging vault")
	}
	stats, err := v.ArchiveStats()
	if err != nil {
		return err
	}
	if stats.Count != 0 {
		return errors.New("archive staging vault is not empty")
	}
	active, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if uint64(len(active)) > math.MaxUint64-expected.Bytes {
		clear(active)
		return errors.New("archive accounting overflow")
	}
	bytesNeeded := uint64(len(active)) + expected.Bytes
	clear(active)
	active = nil
	if err = v.CheckDiskSpace(bytesNeeded, 4); err != nil {
		return err
	}
	var batch []ArchiveRecord
	var batchBytes uint64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := v.db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(archiveBucket)
			if err != nil {
				return err
			}
			for _, record := range batch {
				index := v.archiveIndex(record.Kind, record.ID)
				if b.Get(index) != nil {
					return errors.New("duplicated portable archive identity")
				}
				raw, err := json.Marshal(record)
				if err != nil {
					return err
				}
				size := uint64(len(raw) + 1)
				if stats.Bytes > math.MaxUint64-size || stats.Count == math.MaxUint64 {
					clear(raw)
					return errors.New("archive accounting overflow")
				}
				sealed, err := v.seal(raw, archiveAAD(index))
				clear(raw)
				if err != nil {
					return err
				}
				// bbolt owns this slice until the transaction commits.
				if err = b.Put(index, sealed); err != nil {
					return err
				}
				if stats.Count+1 > expected.Count || size > expected.Bytes-stats.Bytes {
					return errors.New("portable archive exceeds declared totals")
				}
				stats.Count++
				stats.Bytes += size
				stats.Kinds[record.Kind]++
			}
			if err := v.advanceArchiveGeneration(tx); err != nil {
				return err
			}
			raw, err := json.Marshal(stats)
			if err != nil {
				return err
			}
			sealed, err := v.seal(raw, []byte("blakeswap/archive-stats/v1"))
			clear(raw)
			if err != nil {
				return err
			}
			return tx.Bucket(bucket).Put(archiveStatsKey, sealed)
		})
		for i := range batch {
			clear(batch[i].Data)
		}
		batch = nil
		batchBytes = 0
		return err
	}
	defer func() {
		for i := range batch {
			clear(batch[i].Data)
		}
	}()
	err = produce(func(record ArchiveRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !record.valid() {
			return errors.New("invalid streamed archive record")
		}
		record.Data = append(json.RawMessage(nil), record.Data...)
		batch = append(batch, record)
		batchBytes += uint64(len(record.Data))
		if len(batch) >= 64 || batchBytes >= 4<<20 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	if !archiveStatsEqual(stats, expected) {
		return errors.New("portable archive count or byte checkpoint mismatch")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = v.CommitArchive(state, ArchiveBatch{}, 0)
	return err
}

// CheckDiskSpace uses actual encoded byte counts, with explicit overhead for
// encrypted records and bbolt replacement pages. Arithmetic cannot wrap to turn
// an oversized selection into an apparently small allocation.
func (v *Vault) CheckDiskSpace(bytes, multiplier uint64) error {
	return CheckPathSpace(v.path, bytes, multiplier)
}
func CheckPathSpace(path string, bytes, multiplier uint64) error {
	if multiplier == 0 || bytes > (math.MaxUint64-(1<<20))/multiplier {
		return errors.New("archive disk requirement overflows")
	}
	required := bytes*multiplier + (1 << 20)
	available, err := AvailableDisk(path)
	if err != nil {
		return err
	}
	if available < required {
		return errors.New("insufficient disk space for complete private archive staging")
	}
	return nil
}

func (v *Vault) AvailableDisk() (uint64, error) { return AvailableDisk(v.path) }

func AvailableDisk(path string) (uint64, error) {
	var disk unix.Statfs_t
	if err := unix.Statfs(path, &disk); err != nil {
		return 0, err
	}
	if disk.Bsize <= 0 || uint64(disk.Bavail) > math.MaxUint64/uint64(disk.Bsize) {
		return 0, errors.New("unsupported filesystem capacity accounting")
	}
	return uint64(disk.Bavail) * uint64(disk.Bsize), nil
}
