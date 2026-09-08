package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var archiveBucket = []byte("archive-v1")
var archiveStatsKey = []byte("archive-stats-v1")

// ArchiveRecord is the portable representation. Data is embedded JSON, not
// base64: Bytes accounts for the complete encoded record, including its identity.
// On disk both the identity and the data are encrypted.
type ArchiveRecord struct {
	Kind string          `json:"kind"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type ArchiveKey struct{ Kind, ID string }
type ArchiveBatch struct {
	Put    []ArchiveRecord
	Delete []ArchiveKey
}
type ArchiveStats struct {
	Count uint64            `json:"count"`
	Bytes uint64            `json:"bytes"`
	Kinds map[string]uint64 `json:"kinds"`
}

func (r ArchiveRecord) valid() bool {
	return r.Kind != "" && len(r.Kind) <= 64 && !strings.ContainsRune(r.Kind, 0) && r.ID != "" && len(r.ID) <= 1024 && json.Valid(r.Data) && !bytes.Equal(bytes.TrimSpace(r.Data), []byte("null"))
}

func (v *Vault) archiveIndex(kind, id string) []byte {
	mac := hmac.New(sha256.New, v.archiveKey)
	mac.Write([]byte("kind\x00" + kind))
	index := mac.Sum(nil)
	mac.Reset()
	mac.Write([]byte("record\x00" + kind + "\x00" + id))
	return mac.Sum(index)
}
func (v *Vault) seal(raw, aad []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, raw, aad), nil
}
func (v *Vault) unseal(sealed, aad []byte) ([]byte, error) {
	if len(sealed) < v.aead.NonceSize() {
		return nil, errors.New("truncated encrypted archive")
	}
	return v.aead.Open(nil, sealed[:v.aead.NonceSize()], sealed[v.aead.NonceSize():], aad)
}
func archiveAAD(index []byte) []byte {
	return append([]byte("blakeswap/archive-record/v1\x00"), index...)
}
func (v *Vault) decodeArchive(index, sealed []byte) (ArchiveRecord, uint64, error) {
	var record ArchiveRecord
	raw, err := v.unseal(sealed, archiveAAD(index))
	if err != nil {
		return record, 0, err
	}
	defer clear(raw)
	if err = json.Unmarshal(raw, &record); err != nil {
		return record, 0, err
	}
	if !record.valid() || !hmac.Equal(v.archiveIndex(record.Kind, record.ID), index) {
		return record, 0, errors.New("archive record identity mismatch")
	}
	return record, uint64(len(raw) + 1), nil // include portable array separator
}
func (v *Vault) archiveStats(tx *bolt.Tx) (ArchiveStats, error) {
	stats := ArchiveStats{Kinds: map[string]uint64{}}
	sealed := tx.Bucket(bucket).Get(archiveStatsKey)
	if sealed == nil {
		if b := tx.Bucket(archiveBucket); b != nil {
			key, _ := b.Cursor().First()
			if key != nil {
				return stats, errors.New("archive metadata missing")
			}
		}
		return stats, nil
	}
	raw, err := v.unseal(sealed, []byte("blakeswap/archive-stats/v1"))
	if err != nil {
		return stats, err
	}
	defer clear(raw)
	if err = json.Unmarshal(raw, &stats); err != nil {
		return stats, err
	}
	if stats.Kinds == nil {
		return stats, errors.New("invalid archive metadata")
	}
	return stats, nil
}

func (v *Vault) ArchiveStats() (ArchiveStats, error) {
	var stats ArchiveStats
	err := v.db.View(func(tx *bolt.Tx) (err error) { stats, err = v.archiveStats(tx); return })
	return stats, err
}

// CommitArchive atomically publishes the active state and archive changes.
// Repeating an identical put is safe after a crash; a different record at the
// same identity is rejected. Reactivation explicitly deletes its archive copy
// in the transaction which restores active ownership. maxBytes, when nonzero,
// bounds the complete JSON state plus the encoded portable archive records.
func (v *Vault) CommitArchive(state any, batch ArchiveBatch, maxBytes uint64) (ArchiveStats, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return ArchiveStats{}, err
	}
	defer clear(raw)
	stateSize := uint64(len(raw))
	sealed, err := v.seal(raw, []byte("blakeswap/state/v1"))
	if err != nil {
		return ArchiveStats{}, err
	}
	puts := make(map[string][]byte, len(batch.Put))
	putRecords := make(map[string]ArchiveRecord, len(batch.Put))
	var writeSize uint64
	for _, record := range batch.Put {
		if !record.valid() {
			return ArchiveStats{}, errors.New("invalid archive record")
		}
		index := string(v.archiveIndex(record.Kind, record.ID))
		if _, exists := puts[index]; exists {
			return ArchiveStats{}, errors.New("duplicate archive identity in transaction")
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return ArchiveStats{}, err
		}
		defer clear(encoded)
		puts[index] = encoded
		putRecords[index] = record
		if writeSize > math.MaxUint64-uint64(len(encoded)) {
			return ArchiveStats{}, errors.New("archive write accounting overflow")
		}
		writeSize += uint64(len(encoded))
	}
	// bbolt may copy the old tree and allocate replacement pages before freeing
	// them. Reserve room for both encrypted payloads and page/allocation overhead.
	if stateSize > math.MaxUint64-writeSize {
		return ArchiveStats{}, errors.New("archive checkpoint accounting overflow")
	}
	if err := v.CheckDiskSpace(stateSize+writeSize, 4); err != nil {
		return ArchiveStats{}, err
	}
	var committed ArchiveStats
	err = v.db.Update(func(tx *bolt.Tx) error {
		stats, err := v.archiveStats(tx)
		if err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(archiveBucket)
		if err != nil {
			return err
		}
		for _, key := range batch.Delete {
			index := v.archiveIndex(key.Kind, key.ID)
			if _, exists := puts[string(index)]; exists {
				return errors.New("archive transaction cannot both put and delete an identity")
			}
			if previous := b.Get(index); previous != nil {
				record, size, err := v.decodeArchive(index, previous)
				if err != nil {
					return err
				}
				if stats.Count == 0 || stats.Bytes < size || stats.Kinds[record.Kind] == 0 {
					return errors.New("inconsistent archive metadata")
				}
				stats.Count--
				stats.Bytes -= size
				stats.Kinds[record.Kind]--
				if err = b.Delete(index); err != nil {
					return err
				}
			}
		}
		for index, encoded := range puts {
			if previous := b.Get([]byte(index)); previous != nil {
				old, err := v.unseal(previous, archiveAAD([]byte(index)))
				if err != nil {
					return err
				}
				equal := bytes.Equal(old, encoded)
				clear(old)
				if !equal {
					return errors.New("archive identity already belongs to different evidence")
				}
				continue
			}
			size := uint64(len(encoded) + 1)
			if stats.Count == math.MaxUint64 || stats.Bytes > math.MaxUint64-size || stats.Kinds[putRecords[index].Kind] == math.MaxUint64 {
				return errors.New("archive checkpoint accounting overflow")
			}
			stats.Count++
			stats.Bytes += size
			stats.Kinds[putRecords[index].Kind]++
			ciphertext, err := v.seal(encoded, archiveAAD([]byte(index)))
			if err != nil {
				return err
			}
			if err = b.Put([]byte(index), ciphertext); err != nil {
				return err
			}
		}
		if maxBytes > 0 && (stateSize > maxBytes || stats.Bytes > maxBytes-stateSize) {
			return errors.New("wallet recovery data exceeds its portable backup budget")
		}
		if checkpoint, ok := state.(interface{ ValidateArchiveCheckpoint(ArchiveStats) error }); ok {
			if err := checkpoint.ValidateArchiveCheckpoint(stats); err != nil {
				return err
			}
		}
		encodedStats, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		statsCiphertext, err := v.seal(encodedStats, []byte("blakeswap/archive-stats/v1"))
		clear(encodedStats)
		if err != nil {
			return err
		}
		if err = tx.Bucket(bucket).Put(archiveStatsKey, statsCiphertext); err != nil {
			return err
		}
		if err = tx.Bucket(bucket).Put([]byte("state"), sealed); err != nil {
			return err
		}
		committed = stats
		return nil
	})
	return committed, err
}

func (v *Vault) ReadArchive(kind, id string) (ArchiveRecord, bool, error) {
	var record ArchiveRecord
	found := false
	err := v.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(archiveBucket)
		if b == nil {
			return nil
		}
		index := v.archiveIndex(kind, id)
		sealed := b.Get(index)
		if sealed == nil {
			return nil
		}
		var err error
		record, _, err = v.decodeArchive(index, sealed)
		found = err == nil
		return err
	})
	return record, found, err
}

// ArchivePage reads at most limit records. The cursor is an opaque authenticated
// index position, not a plaintext ID. A page does not hold a database transaction
// after return; callers freezing a query use LoadComplete instead.
func (v *Vault) ArchivePage(kind, after string, limit int) ([]ArchiveRecord, string, error) {
	if limit < 1 || limit > 500 {
		return nil, "", errors.New("archive page must contain 1 to 500 records")
	}
	prefix := v.archiveIndex(kind, "")[:32]
	var cursor []byte
	if after != "" {
		var err error
		cursor, err = hex.DecodeString(after)
		if err != nil || len(cursor) != 64 || (kind != "" && !bytes.HasPrefix(cursor, prefix)) {
			return nil, "", errors.New("invalid archive cursor")
		}
	}
	var records []ArchiveRecord
	next := ""
	err := v.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(archiveBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		key, value := c.First()
		if cursor != nil {
			key, value = c.Seek(cursor)
			if bytes.Equal(key, cursor) {
				key, value = c.Next()
			}
		} else if kind != "" {
			key, value = c.Seek(prefix)
		}
		for key != nil && (kind == "" || bytes.HasPrefix(key, prefix)) {
			if len(records) == limit {
				return nil
			}
			record, _, err := v.decodeArchive(key, value)
			if err != nil {
				return err
			}
			records = append(records, record)
			next = hex.EncodeToString(key)
			key, value = c.Next()
		}
		next = ""
		return nil
	})
	return records, next, err
}

// LoadComplete freezes the state and every archive record at one committed
// transaction. It is for explicit backup/export, not routine status polling.
func (v *Vault) LoadComplete(out any, maxBytes uint64) ([]ArchiveRecord, ArchiveStats, error) {
	snapshot, err := v.Freeze()
	if err != nil {
		return nil, ArchiveStats{}, err
	}
	defer snapshot.Close()
	return snapshot.LoadComplete(out, maxBytes)
}

// ReadSnapshot pins a committed bbolt read transaction. A caller can acquire it
// while holding a short wallet lock, then deserialize its bounded history after
// releasing that lock. Close is required and does not close the wallet vault.
type ReadSnapshot struct {
	vault *Vault
	tx    *bolt.Tx
}

func (v *Vault) Freeze() (*ReadSnapshot, error) {
	tx, err := v.db.Begin(false)
	if err != nil {
		return nil, err
	}
	return &ReadSnapshot{vault: v, tx: tx}, nil
}
func (s *ReadSnapshot) Close() error { return s.tx.Rollback() }
func (s *ReadSnapshot) LoadComplete(out any, maxBytes uint64) ([]ArchiveRecord, ArchiveStats, error) {
	return s.vault.loadCompleteTransaction(s.tx, out, maxBytes)
}

func (v *Vault) loadCompleteTransaction(tx *bolt.Tx, out any, maxBytes uint64) ([]ArchiveRecord, ArchiveStats, error) {
	var records []ArchiveRecord
	var stats ArchiveStats
	err := func() error {
		var err error
		stats, err = v.archiveStats(tx)
		if err != nil {
			return err
		}
		raw, err := v.unseal(tx.Bucket(bucket).Get([]byte("state")), []byte("blakeswap/state/v1"))
		if err != nil {
			return err
		}
		defer clear(raw)
		if maxBytes > 0 && (uint64(len(raw)) > maxBytes || stats.Bytes > maxBytes-uint64(len(raw))) {
			return errors.New("wallet snapshot exceeds complete export budget")
		}
		if err = json.Unmarshal(raw, out); err != nil {
			return err
		}
		actual := ArchiveStats{Kinds: map[string]uint64{}}
		if b := tx.Bucket(archiveBucket); b != nil {
			if err = b.ForEach(func(index, sealed []byte) error {
				record, size, err := v.decodeArchive(index, sealed)
				if err != nil {
					return err
				}
				actual.Count++
				actual.Bytes += size
				actual.Kinds[record.Kind]++
				records = append(records, record)
				return nil
			}); err != nil {
				return err
			}
		}
		if actual.Count != stats.Count || actual.Bytes != stats.Bytes {
			return errors.New("archive count or byte checkpoint mismatch")
		}
		for kind, count := range stats.Kinds {
			if actual.Kinds[kind] != count {
				return fmt.Errorf("archive category checkpoint mismatch: %s", kind)
			}
		}
		return nil
	}()
	return records, stats, err
}
