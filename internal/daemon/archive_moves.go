package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// Prepare all fallible reads and encodings before changing any active owner.
// A child additionally carries its allocation, fee/origin and immutable indexes.
func (e *Engine) stageArchive(kind, id string) error {
	if err := ValidateStateVersion(&e.s); err != nil {
		return err
	}
	record, err := e.planArchiveRecord(kind, id, "")
	if err != nil || record == nil {
		return err
	}
	records := []storage.ArchiveRecord{}
	if kind == "swaps" {
		swap := e.s.Swaps[id]
		if err := validateSwapFundingParents(&e.s, engineFillReader{e}, swap); err != nil {
			return err
		}
		if _, err := e.fillSummary(swap, false); err != nil {
			return err
		}
		if _, err := e.retainedSwapFee(swap); err != nil {
			return err
		}
		keys, err := swapIdentityKeys(swap)
		if err != nil {
			return err
		}
		for _, key := range []storage.ArchiveKey{{Kind: "fill_records", ID: id}, {Kind: "funding_fees", ID: "swap/" + id}, {Kind: "recovery_swaps", ID: id}} {
			companion, err := e.planArchiveRecord(key.Kind, key.ID, "")
			if err != nil {
				return err
			}
			if companion != nil {
				records = append(records, *companion)
			}
		}
		for _, key := range keys {
			index, err := e.planArchiveRecord("fill_keys", key, id)
			if err != nil {
				return err
			}
			if index != nil {
				records = append(records, *index)
			}
		}
	}
	records = append(records, *record)
	// Compute the complete ownership checkpoint privately too. No encoding,
	// source read or fallible counter update remains once owners start moving.
	planned := Engine{s: State{Version: e.s.Version, Network: e.s.Network}}
	if e.s.Capacity != nil {
		copy := *e.s.Capacity
		copy.Archived.Kinds = maps.Clone(copy.Archived.Kinds)
		planned.s.Capacity = &copy
	}
	for _, record := range records {
		if err := planned.archiveDelta(record, true); err != nil {
			return err
		}
	}
	if e.s.Capacity == nil {
		e.s.Capacity = planned.s.Capacity
	} else {
		*e.s.Capacity = *planned.s.Capacity
	}
	if e.archivePuts == nil {
		e.archivePuts = map[string]storage.ArchiveRecord{}
	}
	for _, record := range records {
		group, _ := archiveMap(&e.s, record.Kind, false) // Field paths validated during planning.
		e.archivePuts[archiveMoveKey(record.Kind, record.ID)] = record
		group.SetMapIndex(reflect.ValueOf(record.ID), reflect.Value{})
	}
	return nil
}

// newOwner is used only for a child's exact immutable identity index. An index
// already cold stays cold; missing current index data can be created with its
// core in this same checkpoint, never as an independent acceptance permission.
func (e *Engine) planArchiveRecord(kind, id, newOwner string) (*storage.ArchiveRecord, error) {
	if _, deleting := e.archiveDeletes[archiveMoveKey(kind, id)]; deleting {
		return nil, nil
	}
	group, err := archiveMap(&e.s, kind, false)
	if err != nil || !group.IsValid() {
		return nil, err
	}
	value := group.MapIndex(reflect.ValueOf(id))
	if !value.IsValid() && newOwner == "" {
		return nil, nil
	}
	cold, found, err := e.archiveRecord(kind, id)
	if err != nil {
		return nil, err
	}
	if found {
		if value.IsValid() {
			return nil, errors.New("archive already owns an active record")
		}
		var owner string
		if kind != "fill_keys" || json.Unmarshal(cold.Data, &owner) != nil || owner != newOwner {
			return nil, errors.New("cold identity belongs to another child")
		}
		return nil, nil
	}
	var source any = newOwner
	if value.IsValid() {
		source = value.Interface()
		if newOwner != "" && source != newOwner {
			return nil, errors.New("active identity belongs to another child")
		}
	}
	data, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(data, []byte("null")) {
		return nil, errors.New("cannot archive a null record")
	}
	return &storage.ArchiveRecord{Kind: kind, ID: id, Data: data}, nil
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
	case "fill_keys":
		if validFundingIdentityKey(id) {
			if err := validateFundingIdentity(&e.s, engineFillReader{e}, id); err != nil {
				return false, err
			}
		}
	case "swaps":
		companions = append(companions, storage.ArchiveKey{Kind: "recovery_swaps", ID: id}, storage.ArchiveKey{Kind: "funding_fees", ID: "swap/" + id})
		var swap Swap
		if err := json.Unmarshal(record.Data, &swap); err != nil {
			return false, err
		}
		if err := validateSwapFundingParents(&e.s, engineFillReader{e}, &swap); err != nil {
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
	if record.Kind == "swaps" {
		if swap := e.s.Swaps[record.ID]; swap != nil && len(swap.FundingParents) > 0 {
			swap.FundingAncestryHeld = true
			delete(e.fundingAncestryProofs, swap.ID)
		}
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
