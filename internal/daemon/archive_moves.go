package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func archiveMoveKey(kind, id string) string { return kind + "\x00" + id }

// A parent ledger stays hot while any local maker child can update its bins.
func (e *Engine) activeMakerParents() map[string]bool {
	parents := map[string]bool{}
	for _, swap := range e.s.Swaps {
		if swap != nil && swap.Role == "maker" && swap.Terms != nil {
			parents[swap.Terms.Offer().ID] = true
		}
	}
	return parents
}

// Repair only exact child fee companions before startup consumers run; this
// never reactivates an offer, receipt or publication authority.
func (e *Engine) restoreActiveFundingFees() error {
	owners := map[string]bool{}
	for _, swap := range e.s.Swaps {
		if swap == nil {
			continue
		}
		owner := "swap/" + swap.ID
		owners[owner] = true
	}
	for _, owner := range sortedArchiveIDs(owners) {
		if _, present := e.s.FundingFees[owner]; present {
			continue
		}
		if _, err := e.activateArchived("funding_fees", owner); err != nil {
			return fmt.Errorf("cannot restore retained funding fee: %w", err)
		}
	}
	for _, swap := range e.s.Swaps {
		if _, err := e.retainedSwapFee(swap); err != nil {
			return err
		}
	}
	return nil
}

// Stage ownership changes in memory while the engine lock is held. persistState
// commits their encrypted bucket records and active state in one transaction,
// before any protocol caller acknowledges the action. A failed commit stops the
// engine; reopen sees the preceding complete checkpoint.
func (e *Engine) archiveDelta(record storage.ArchiveRecord, add bool) error {
	// Archive placement cannot downgrade the hard-cutover state format.
	if err := ValidateStateVersion(&e.s); err != nil {
		return err
	}
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
	if err := ValidateStateVersion(&e.s); err != nil {
		return err
	}
	if kind == "swaps" && e.s.Swaps[id] != nil {
		if err := e.retainSwapIdentity(e.s.Swaps[id]); err != nil {
			return err
		}
		// Complete the core's fallible read/encoding/collision checks before
		// changing companion placement. The remaining core staging is local.
		if err := e.stageArchive("fill_records", id); err != nil {
			return err
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
	if e.archiveRead != nil {
		return e.archiveRead(kind, id)
	}
	return e.vault.ReadArchive(kind, id)
}

func (e *Engine) activateArchived(kind, id string) (bool, error) {
	var records []storage.ArchiveRecord
	visited := map[string]bool{}
	found, err := e.collectArchiveActivation(kind, id, visited, &records)
	if err != nil || !found {
		return false, err
	}
	// All core and companion formats were checked before the first promotion.
	// Companions precede their core in the shared pending transaction.
	for _, record := range records {
		if err := e.promoteArchived(record); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (e *Engine) collectArchiveActivation(kind, id string, visited map[string]bool, records *[]storage.ArchiveRecord) (bool, error) {
	key := archiveMoveKey(kind, id)
	if visited[key] {
		return true, nil
	}
	record, found, err := e.archiveRecord(kind, id)
	if err != nil || !found {
		return found, err
	}
	if _, err := ValidateArchiveRecordAgainstState(e.s, record); err != nil {
		return false, err
	}
	if _, err := archiveRecordDigest(record); err != nil {
		return false, err
	}
	visited[key] = true
	// Imported origin and fee policy must be visible before execution. This
	// finite companion graph is shared by direct mailbox and reorg activation.
	var companions []storage.ArchiveKey
	switch kind {
	case "swaps":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_swaps", ID: id}, storage.ArchiveKey{Kind: "funding_fees", ID: "swap/" + id})
		var swap Swap
		if err := json.Unmarshal(record.Data, &swap); err != nil {
			return false, err
		}
		if _, err := e.retainedSwapFee(&swap); err != nil {
			return false, err
		}
		if swap.Role == "maker" {
			fill, err := e.fillSummary(&swap, true)
			if err != nil {
				return false, err
			}
			parent := e.s.ParentOrders[fill.ParentID]
			if parent == nil {
				var cold ParentOrder
				found, err := e.archivedValue("parent_orders", fill.ParentID, &cold)
				if err != nil {
					return false, err
				}
				if !found {
					return false, errors.New("retained maker child has no parent ledger")
				}
				parent = &cold
			}
			var offered protocol.Offer
			if err := json.Unmarshal([]byte(swap.Request.OfferEvent.Content), &offered); err != nil {
				return false, err
			}
			if err := validateParentOrder(fill.ParentID, parent); err != nil {
				return false, err
			}
			if parent.Offer.Maker != fill.ParentMaker || parent.Economics != offered.EconomicsDigest() {
				return false, errors.New("retained child belongs to another parent ledger")
			}
			companions = append(companions, storage.ArchiveKey{Kind: "parent_orders", ID: fill.ParentID}, storage.ArchiveKey{Kind: "fill_records", ID: id})
		}
	case "sends":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_sends", ID: id})
	case "tower_jobs":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_tower_jobs", ID: id})
	}
	for _, companion := range companions {
		if _, err := e.collectArchiveActivation(companion.Kind, companion.ID, visited, records); err != nil {
			return false, err
		}
	}
	*records = append(*records, record)
	return true, nil
}

func (e *Engine) promoteArchived(record storage.ArchiveRecord) error {
	origin, err := archiveRecordDigest(record)
	if err != nil {
		return err
	}
	if e.archiveOrigins == nil {
		e.archiveOrigins = map[string][32]byte{}
	}
	e.archiveOrigins[archiveMoveKey(record.Kind, record.ID)] = origin
	if err = mergeArchiveRecord(&e.s, record); err != nil {
		return err
	}
	if err = e.archiveDelta(record, false); err != nil {
		return err
	}
	key := archiveMoveKey(record.Kind, record.ID)
	if _, pending := e.archivePuts[key]; pending {
		delete(e.archivePuts, key)
	} else {
		if e.archiveDeletes == nil {
			e.archiveDeletes = map[string]storage.ArchiveKey{}
		}
		e.archiveDeletes[key] = storage.ArchiveKey{Kind: record.Kind, ID: record.ID}
	}
	return nil
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
