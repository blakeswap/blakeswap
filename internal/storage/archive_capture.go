package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"

	bolt "go.etcd.io/bbolt"
)

var ErrCloneUnsupported = errors.New("filesystem does not support independent encrypted clones")
var ErrArchiveChanged = errors.New("archive changed during snapshot; retry the complete export")

// PageSnapshot owns the encrypted active checkpoint and a generation fence. It
// retains no bbolt read transaction. The caller owns a source-vault lifetime
// lease until Close; each archive page releases its read transaction before
// decryption, validation, IO to the destination, or a callback can block.
type PageSnapshot struct {
	vault      *Vault
	state      []byte
	stats      ArchiveStats
	generation []byte
	once       sync.Once
}

func (s *PageSnapshot) Close() error {
	s.once.Do(func() { clear(s.state); s.state = nil; clear(s.generation); s.generation = nil })
	return nil
}
func (s *PageSnapshot) LoadState(out any) (ArchiveStats, uint64, error) {
	raw, err := s.vault.unseal(s.state, []byte("blakeswap/state/v1"))
	if err != nil {
		return s.stats, 0, err
	}
	defer clear(raw)
	return s.stats, uint64(len(raw)), json.Unmarshal(raw, out)
}

// CaptureArchive uses an actual filesystem clone, never a convenience copy
// API which might silently copy all bytes while the writer is stopped. Passing
// allowClone=false selects the portable generation-checked fallback explicitly.
// Active-state/token preparation must precede this call under the caller's
// engine/lifecycle ownership. The write transaction excludes every vault writer
// while the committed file is cloned or the fallback checkpoint is captured.
func (v *Vault) CaptureArchive(path string, allowClone bool) (bool, *PageSnapshot, error) {
	if err := v.ensureArchiveGeneration(); err != nil {
		return false, nil, err
	}
	tx, err := v.db.Begin(true)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	if allowClone {
		if err := cloneFile(v.path, path); err == nil {
			info, statErr := os.Stat(path)
			if statErr != nil {
				_ = os.Remove(path)
				return false, nil, statErr
			}
			// Account for subsequent COW growth only when this filesystem
			// actually cloned. Unsupported filesystems use staging's own
			// encoded-size preflight, independent of old bbolt free pages.
			if err := v.CheckDiskSpace(uint64(info.Size()), 1); err != nil {
				_ = os.Remove(path)
				return false, nil, err
			}
			file, openErr := os.OpenFile(path, os.O_RDWR, 0)
			if openErr != nil {
				_ = os.Remove(path)
				return false, nil, openErr
			}
			syncErr := file.Sync()
			closeErr := file.Close()
			if syncErr != nil || closeErr != nil {
				_ = os.Remove(path)
				return false, nil, errors.Join(syncErr, closeErr)
			}
			directory, openErr := os.Open(filepath.Dir(path))
			if openErr != nil {
				_ = os.Remove(path)
				return false, nil, openErr
			}
			syncErr = directory.Sync()
			closeErr = directory.Close()
			if syncErr != nil || closeErr != nil {
				_ = os.Remove(path)
				return false, nil, errors.Join(syncErr, closeErr)
			}
			return true, nil, nil
		} else if !errors.Is(err, ErrCloneUnsupported) {
			return false, nil, err
		}
	}
	stats, err := v.archiveStats(tx)
	if err != nil {
		return false, nil, err
	}
	generation, err := v.archiveGeneration(tx)
	if err != nil {
		return false, nil, err
	}
	state := append([]byte(nil), tx.Bucket(bucket).Get([]byte("state"))...)
	return false, &PageSnapshot{vault: v, state: state, stats: stats, generation: generation}, nil
}
func (s *PageSnapshot) check(tx *bolt.Tx) error {
	current, err := s.vault.archiveGeneration(tx)
	if err != nil {
		return err
	}
	defer clear(current)
	if len(s.generation) != 32 || !bytes.Equal(current, s.generation) {
		return ErrArchiveChanged
	}
	return nil
}
func (s *PageSnapshot) VisitArchive(ctx context.Context, visit func(ArchiveRecord) error) error {
	type encryptedRecord struct{ index, sealed []byte }
	actual := ArchiveStats{Kinds: map[string]uint64{}}
	var cursor []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var page []encryptedRecord
		var pageBytes uint64
		end := false
		err := s.vault.db.View(func(tx *bolt.Tx) error {
			if err := s.check(tx); err != nil {
				return err
			}
			b := tx.Bucket(archiveBucket)
			if b == nil {
				end = true
				return nil
			}
			c := b.Cursor()
			key, value := c.First()
			if cursor != nil {
				key, value = c.Seek(cursor)
				if bytes.Equal(key, cursor) {
					key, value = c.Next()
				}
			}
			for key != nil {
				if len(page) >= 64 || (len(page) > 0 && pageBytes+uint64(len(value)) > 4<<20) {
					break
				}
				page = append(page, encryptedRecord{append([]byte(nil), key...), append([]byte(nil), value...)})
				pageBytes += uint64(len(value))
				key, value = c.Next()
			}
			end = key == nil
			return nil
		})
		if err != nil {
			return err
		}
		for _, item := range page {
			if err = ctx.Err(); err == nil {
				var record ArchiveRecord
				var size uint64
				record, size, err = s.vault.decodeArchive(item.index, item.sealed)
				if err == nil {
					if actual.Count == math.MaxUint64 || actual.Bytes > math.MaxUint64-size {
						err = errors.New("archive accounting overflow")
					} else {
						actual.Count++
						actual.Bytes += size
						actual.Kinds[record.Kind]++
						err = visit(record)
					}
				}
				clear(record.Data)
			}
			clear(item.sealed)
			if err != nil {
				for _, rest := range page {
					clear(rest.sealed)
				}
				return err
			}
			cursor = append(cursor[:0], item.index...)
		}
		if end {
			// Fence after the final callback: a writer may have changed archive pages
			// while the last copied batch was being validated or written privately.
			if err := s.vault.db.View(s.check); err != nil {
				return err
			}
			if !archiveStatsEqual(actual, s.stats) {
				return errors.New("snapshot archive completeness mismatch")
			}
			return ctx.Err()
		}
	}
}
