package daemon

import (
	"errors"
	"time"

	"fiatjaf.com/nostr"
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
	if s == nil || (s.Version != 1 && s.Version != 2) || snapshotAt <= 0 {
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
	if s.Capacity != nil {
		s.Capacity.Anchors = nil
		s.Capacity.Reactivating = false
	}
	if err := ValidateAutomationState(s); err != nil {
		return err
	}
	holdImportedAutomations(s)
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
	for id := range s.Swaps {
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
	r.Status = RecoveryStatus{State: "recovering", ImportedAt: r.ImportedAt, SnapshotAt: r.SnapshotAt, Legacy: r.Legacy, Issues: []RecoveryIssue{{Kind: "chains", Reason: "Waiting for complete current observations from both chains."}}, QuarantinedOffers: len(r.Offers), QuarantinedMessages: len(r.Outbox), Coverage: recoveryCoverage}
	s.Recovery = r
	return nil
}
