package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// RecoveryRecord survives restart and re-export. Original obligations remain
// listed after their display stage changes, so reconciliation cannot silently
// forget a stale snapshot's unresolved contracts. Quarantined messages and offers
// retain their signed bytes for inspection but never re-enter live publication.
type RecoveryRecord struct {
	// These holds survive a known canonical contradiction until fresh positive
	// settlement evidence resolves the affected original obligation. Display
	// history alone must not authorize stopping its monitoring after a restart.
	InvalidatedSettlements map[string]bool        `json:"invalidated_settlements,omitempty"`
	ImportedAt             int64                  `json:"imported_at"`
	SnapshotAt             int64                  `json:"snapshot_at"`
	Legacy                 bool                   `json:"legacy"`
	Swaps                  map[string]bool        `json:"swaps"`
	Sends                  map[string]bool        `json:"sends"`
	TowerJobs              map[string]bool        `json:"tower_jobs"`
	Offers                 map[string]nostr.Event `json:"quarantined_offers"`
	Outbox                 map[string]*Delivery   `json:"quarantined_outbox"`
	Status                 RecoveryStatus         `json:"status"`
}

type RecoveryIssue struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type RecoveryStatus struct {
	State               string          `json:"state"`
	ImportedAt          int64           `json:"imported_at"`
	SnapshotAt          int64           `json:"snapshot_at"`
	Legacy              bool            `json:"legacy"`
	CheckedAt           int64           `json:"checked_at"`
	Issues              []RecoveryIssue `json:"issues"`
	QuarantinedOffers   int             `json:"quarantined_offers"`
	QuarantinedMessages int             `json:"quarantined_messages"`
	Coverage            string          `json:"coverage"`
}

const recoveryCoverage = "Recovery checks the obligations recorded in this file. An older snapshot may omit later activity or random secrets; keep newer backups and the original installation until recovery is complete. Old orders and queued publications stay quarantined."

// PrepareRecovery is called on the private imported copy before installing its
// vault. It intentionally never erases signed transactions, secret knowledge,
// receipts, pending payments or earlier recovery holds.
func PrepareRecovery(s *State, snapshotAt int64, legacy bool) error {
	if err := ValidateStateVersion(s); err != nil {
		return err
	}
	if snapshotAt <= 0 {
		return errors.New("invalid recovery snapshot")
	}
	if err := ValidateArchiveState(*s); err != nil {
		return err
	}
	if len(s.Archive) > 0 {
		complete, err := CompleteState(*s)
		if err != nil {
			return err
		}
		*s = complete
	}
	return prepareRecoveryActive(s, snapshotAt, legacy)
}

// PrepareStreamedRecovery is for a fully authenticated private staged import.
// Every core obligation must have been promoted before this call. source is the
// same pinned, authenticated checkpoint used for that promotion; ownership
// indexes and quarantined history stay cold. Its lifetime covers this entire
// completed check. No cold lookup grants publication or settlement authority.
func PrepareStreamedRecovery(ctx context.Context, s *State, stats storage.ArchiveStats, source FillStateReader, snapshotAt int64, legacy bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateProtocolState(s); err != nil {
		return err
	}
	if snapshotAt <= 0 || len(s.Archive) != 0 {
		return errors.New("invalid streamed recovery snapshot")
	}
	if err := s.ValidateArchiveCheckpoint(stats); err != nil {
		return err
	}
	for kind, count := range stats.Kinds {
		if count > 0 && recoveryCoreKind(kind) {
			return errors.New("streamed recovery omits a core obligation")
		}
		if count > 0 && (kind == "offers" || kind == "outbox") {
			return errors.New("streamed recovery retains live publication archive")
		}
	}
	if source == nil {
		return errors.New("streamed recovery requires its authenticated archive reader")
	}
	if err := ValidateFillConservation(ctx, s, recoveryFillReader{s, source}, ""); err != nil {
		return err
	}
	return prepareRecoveryActive(s, snapshotAt, legacy)
}

// The source still contains promoted rows. Validate them against the promoted
// active value, then exclude them from the cold traversal so child allocations
// are counted once. Exact companion reads continue to use the same source.
// One row is decoded at a time; there is no lifetime ownership collection.
type recoveryFillReader struct {
	active *State
	source FillStateReader
}

func (r recoveryFillReader) ReadArchive(kind, id string) (storage.ArchiveRecord, bool, error) {
	return r.source.ReadArchive(kind, id)
}

func (r recoveryFillReader) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	return r.source.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if recoveryCoreKind(record.Kind) {
			group, err := archiveMap(r.active, record.Kind, false)
			if err != nil {
				return err
			}
			if !group.IsValid() {
				return errors.New("streamed recovery did not promote a source core")
			}
			value := group.MapIndex(reflect.ValueOf(record.ID))
			if !value.IsValid() {
				return errors.New("streamed recovery did not promote a source core")
			}
			original := reflect.New(value.Type())
			if err := json.Unmarshal(record.Data, original.Interface()); err != nil {
				return err
			}
			if !reflect.DeepEqual(original.Elem().Interface(), value.Interface()) {
				return errors.New("streamed recovery changed a promoted source core")
			}
			return nil
		}
		cold, _ := QuarantineArchiveRecord(record)
		return visit(cold)
	})
}

func prepareRecoveryActive(s *State, snapshotAt int64, legacy bool) error {
	if err := ValidateHistoryCoverage(s); err != nil {
		return err
	}
	if s.Capacity != nil {
		s.Capacity.Anchors = nil
		if s.Capacity.Archived.Kinds["activities"] == 0 {
			s.Capacity.HistoryCoverage = nil
		}
		s.Capacity.Reactivating = false
	}
	if err := ValidateOrderSettlements(s); err != nil {
		return err
	}
	if err := ValidateAutomationState(s); err != nil {
		return err
	}
	holdImportedAutomations(s)
	// A snapshot cannot establish that the original installation never
	// accepted another child or signed funding later. Keep every allocation
	// and monetary counter, but never resume a parent's publisher/input pool.
	for _, parent := range s.ParentOrders {
		if parent != nil {
			parent.RestoreHold = true
		}
	}
	for _, child := range s.FillRecords {
		if child != nil {
			child.ImportedUncertain = true
		}
	}
	r := s.Recovery
	if r == nil {
		r = &RecoveryRecord{SnapshotAt: snapshotAt, Legacy: legacy}
	}
	if r.Swaps == nil {
		r.Swaps = map[string]bool{}
	}
	if r.Sends == nil {
		r.Sends = map[string]bool{}
	}
	if r.TowerJobs == nil {
		r.TowerJobs = map[string]bool{}
	}
	if r.Offers == nil {
		r.Offers = map[string]nostr.Event{}
	}
	if r.Outbox == nil {
		r.Outbox = map[string]*Delivery{}
	}
	for id, swap := range s.Swaps {
		if swap != nil && len(swap.FundingParents) > 0 {
			swap.FundingAncestryHeld = true
		}
		r.Swaps[id] = true
	}
	for id := range s.Sends {
		r.Sends[id] = true
	}
	for id := range s.TowerJobs {
		r.TowerJobs[id] = true
	}
	for id, event := range s.Offers {
		r.Offers[id] = event
	}
	for id, delivery := range s.Outbox {
		r.Outbox[id] = delivery
	}
	s.Offers = map[string]nostr.Event{}
	s.Outbox = map[string]*Delivery{}
	// Market and discovery caches are advisory, never recovery authority.
	s.Book = map[string]nostr.Event{}
	r.ImportedAt = time.Now().Unix()
	r.Legacy = r.Legacy || legacy
	s.Recovery = r
	offers, messages := recoveryQuarantineCounts(*s)
	r.Status = RecoveryStatus{State: "recovering", ImportedAt: r.ImportedAt, SnapshotAt: r.SnapshotAt, Legacy: r.Legacy, Issues: []RecoveryIssue{{Kind: "chains", Reason: "Waiting for complete current observations from both chains."}}, QuarantinedOffers: offers, QuarantinedMessages: messages, Coverage: recoveryCoverage}
	return nil
}

func recoveryCoreKind(kind string) bool {
	switch kind {
	case "swaps", "sends", "tower_jobs", "recovery_swaps", "recovery_sends", "recovery_tower_jobs", "funding_fees", "parent_orders", "fill_records":
		return true
	}
	return false
}

// PromoteRecoveryRecord retains exact core records in the imported active
// checkpoint. It rejects overlap instead of choosing an authority by disk order.
func PromoteRecoveryRecord(s *State, record storage.ArchiveRecord) (bool, error) {
	if !recoveryCoreKind(record.Kind) {
		return false, nil
	}
	return true, mergeArchiveRecord(s, record)
}

func QuarantineArchiveRecord(record storage.ArchiveRecord) (storage.ArchiveRecord, bool) {
	if recoveryCoreKind(record.Kind) {
		return record, false
	}
	switch record.Kind {
	case "offers":
		record.Kind = "quarantined_offers"
	case "outbox":
		record.Kind = "quarantined_outbox"
	}
	return record, true
}

func recoveryQuarantineCounts(s State) (int, int) {
	offers, messages := 0, 0
	if s.Recovery != nil {
		offers, messages = len(s.Recovery.Offers), len(s.Recovery.Outbox)
	}
	if s.Capacity != nil {
		maxInt := uint64(^uint(0) >> 1)
		offers = int(min(maxInt, uint64(offers)+min(maxInt-uint64(offers), s.Capacity.Archived.Kinds["quarantined_offers"])))
		messages = int(min(maxInt, uint64(messages)+min(maxInt-uint64(messages), s.Capacity.Archived.Kinds["quarantined_outbox"])))
	}
	return offers, messages
}
