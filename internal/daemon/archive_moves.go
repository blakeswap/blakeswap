package daemon

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/blakeswap/blakeswap/internal/storage"
)

func archiveMoveKey(kind, id string) string { return kind + "\x00" + id }

// Stage ownership changes in memory while the engine lock is held. persistState
// commits their encrypted bucket records and active state in one transaction,
// before any protocol caller acknowledges the action. A failed commit stops the
// engine; reopen sees the preceding complete checkpoint.
func (e *Engine) archiveDelta(record storage.ArchiveRecord, add bool) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	size := uint64(len(encoded) + 1)
	clear(encoded)
	if e.s.Capacity == nil {
		e.s.Capacity = &CapacityRecord{Archived: storage.ArchiveStats{Kinds: map[string]uint64{}}}
	}
	c := e.s.Capacity
	if c.Archived.Kinds == nil {
		c.Archived.Kinds = map[string]uint64{}
	}
	if add {
		c.Archived.Count++
		c.Archived.Bytes += size
		c.Archived.Kinds[record.Kind]++
	} else {
		if c.Archived.Count == 0 || c.Archived.Bytes < size || c.Archived.Kinds[record.Kind] == 0 {
			return errors.New("archive ownership checkpoint underflow")
		}
		c.Archived.Count--
		c.Archived.Bytes -= size
		c.Archived.Kinds[record.Kind]--
	}
	c.Revision++
	e.s.Version = 2 // Older readers must refuse state requiring archive buckets.
	return nil
}

func (e *Engine) stageArchive(kind, id string) error {
	key := archiveMoveKey(kind, id)
	// A just-reactivated record stays active through this checkpoint. Its next
	// compaction can replace the old evidence only after the deletion commits.
	if _, deleting := e.archiveDeletes[key]; deleting {
		return nil
	}
	group, err := archiveMap(&e.s, kind, false)
	if err != nil || !group.IsValid() {
		return err
	}
	value := group.MapIndex(reflect.ValueOf(id))
	if !value.IsValid() {
		return nil
	}
	data, err := json.Marshal(value.Interface())
	if err != nil {
		return err
	}
	record := storage.ArchiveRecord{Kind: kind, ID: id, Data: data}
	if e.vault != nil {
		if _, found, err := e.vault.ReadArchive(kind, id); err != nil {
			return err
		} else if found {
			return errors.New("archive already owns an active record")
		}
	}
	if err := e.archiveDelta(record, true); err != nil {
		return err
	}
	if e.archivePuts == nil {
		e.archivePuts = map[string]storage.ArchiveRecord{}
	}
	e.archivePuts[key] = record
	group.SetMapIndex(reflect.ValueOf(id), reflect.Value{})
	return nil
}

func (e *Engine) archiveRecord(kind, id string) (storage.ArchiveRecord, bool, error) {
	key := archiveMoveKey(kind, id)
	if record, found := e.archivePuts[key]; found {
		return record, true, nil
	}
	if _, deleting := e.archiveDeletes[key]; deleting || e.vault == nil {
		return storage.ArchiveRecord{}, false, nil
	}
	return e.vault.ReadArchive(kind, id)
}

func (e *Engine) activateArchived(kind, id string) (bool, error) {
	record, found, err := e.archiveRecord(kind, id)
	if err != nil || !found {
		return found, err
	}
	// An obligation must never become executable before its imported-origin
	// restriction and exact fee policy. These bounded companions share the same
	// pending transaction, regardless of whether a reorg or a mailbox read caused
	// reactivation. A read/decode failure leaves the core record cold.
	companions := []storage.ArchiveKey{}
	switch kind {
	case "swaps":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_swaps", ID: id}, storage.ArchiveKey{Kind: "funding_fees", ID: "swap/" + id})
		var swap Swap
		if err := json.Unmarshal(record.Data, &swap); err != nil {
			return false, err
		}
		if swap.Role == "maker" && swap.Terms != nil {
			companions = append(companions, storage.ArchiveKey{Kind: "funding_fees", ID: "offer/" + swap.Terms.Offer().ID})
		}
	case "sends":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_sends", ID: id})
	case "tower_jobs":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_tower_jobs", ID: id})
	}
	for _, key := range companions {
		if _, err := e.activateArchived(key.Kind, key.ID); err != nil {
			return false, err
		}
	}
	origin, err := archiveRecordDigest(record)
	if err != nil {
		return false, err
	}
	if e.archiveOrigins == nil {
		e.archiveOrigins = map[string][32]byte{}
	}
	e.archiveOrigins[archiveMoveKey(kind, id)] = origin
	if err = mergeArchiveRecord(&e.s, record); err != nil {
		return false, err
	}
	if err = e.archiveDelta(record, false); err != nil {
		return false, err
	}
	key := archiveMoveKey(kind, id)
	if _, pending := e.archivePuts[key]; pending {
		delete(e.archivePuts, key)
	} else {
		if e.archiveDeletes == nil {
			e.archiveDeletes = map[string]storage.ArchiveKey{}
		}
		e.archiveDeletes[key] = storage.ArchiveKey{Kind: kind, ID: id}
	}
	return true, nil
}

func (e *Engine) pendingArchive() storage.ArchiveBatch {
	batch := storage.ArchiveBatch{}
	for _, record := range e.archivePuts {
		batch.Put = append(batch.Put, record)
	}
	for _, key := range e.archiveDeletes {
		batch.Delete = append(batch.Delete, key)
	}
	return batch
}
