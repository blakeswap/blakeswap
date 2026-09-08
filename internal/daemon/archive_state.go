package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
)

const WalletRecoveryBudget = 64 << 20
const archiveBatchSize = 64
const activeWorkLimit = 1000

// CapacityRecord is a durable compaction checkpoint, not settlement proof.
// Anchors are checked against current canonical hashes before new admission.
// A contradiction persists Reactivating before any archive reader can fail.
type CapacityRecord struct {
	SemanticToken     string                       `json:"semantic_token"`
	ActiveFingerprint string                       `json:"active_fingerprint"`
	Invalidated       map[string]bool              `json:"invalidated,omitempty"`
	Archived          storage.ArchiveStats         `json:"archived"`
	Anchors           map[chain.ID]ArchiveAnchor   `json:"anchors"`
	HistoryCoverage   map[chain.ID]HistoryCoverage `json:"history_coverage,omitempty"`
	Reactivating      bool                         `json:"reactivating"`
	Reason            string                       `json:"reason"`
	Revision          uint64                       `json:"revision"`
}

// HistoryCoverage is advisory inclusion continuity, separate from settlement
// monitoring. Rows carry its random ID so later unrelated anchors cannot
// authenticate imported or contradicted history. It never grants authority.
type HistoryCoverage struct {
	ID     string `json:"id"`
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

type ArchiveAnchor struct {
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

func (s State) ValidateArchiveCheckpoint(stats storage.ArchiveStats) error {
	if err := ValidateStateVersion(&s); err != nil {
		return err
	}
	if err := ValidateHistoryCoverage(&s); err != nil {
		return err
	}
	if s.Capacity == nil {
		if stats.Count != 0 {
			return errors.New("archive has no active ownership checkpoint")
		}
		return nil
	}
	if stats.Count != s.Capacity.Archived.Count || stats.Bytes != s.Capacity.Archived.Bytes {
		return errors.New("active and archive ownership checkpoints disagree")
	}
	for kind, count := range stats.Kinds {
		if s.Capacity.Archived.Kinds[kind] != count {
			return errors.New("archive category checkpoint disagrees")
		}
	}
	for kind, count := range s.Capacity.Archived.Kinds {
		if stats.Kinds[kind] != count {
			return errors.New("archive category checkpoint disagrees")
		}
	}
	return nil
}

// Only named durable maps can be moved. Admission and identity lookups use these
// same names, and a complete backup reconstructs the original logical maps.
// Recovery quarantine has separate names so archived offers never acquire live
// publication authority when a restored record is queried or reactivated.
var archiveFields = map[string][]string{
	"parent_orders": {"ParentOrders"}, "fill_records": {"FillRecords"}, "fill_keys": {"FillKeys"},
	"own_public_versions": {"OwnPublicVersions"},
	"sends":               {"Sends"}, "swaps": {"Swaps"}, "tower_jobs": {"TowerJobs"},
	"offers": {"Offers"}, "order_records": {"OrderRecords"}, "seen": {"Seen"}, "seen_semantics": {"SeenSemantics"},
	"outbox": {"Outbox"}, "trade_receipts": {"TradeReceipts"}, "trade_tokens": {"TradeTokens"},
	"funding_fees": {"FundingFees"}, "offer_towers": {"OfferTowers"},
	"activities": {"Activities"}, "activity_receipts": {"ActivityReceipts"},
	"activity_transactions": {"ActivityTransactions"}, "activity_owned": {"ActivityOwned"},
	"recovery_swaps": {"Recovery", "Swaps"}, "recovery_sends": {"Recovery", "Sends"},
	"recovery_tower_jobs": {"Recovery", "TowerJobs"},
	"quarantined_offers":  {"Recovery", "Offers"}, "quarantined_outbox": {"Recovery", "Outbox"},
}

func archiveMap(state *State, kind string, create bool) (reflect.Value, error) {
	path, ok := archiveFields[kind]
	if !ok {
		return reflect.Value{}, errors.New("unsupported archive record category")
	}
	value := reflect.ValueOf(state).Elem()
	for _, field := range path {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				if !create {
					return reflect.Value{}, nil
				}
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		value = value.FieldByName(field)
	}
	if create && value.IsNil() {
		value.Set(reflect.MakeMap(value.Type()))
	}
	return value, nil
}

func mergeArchiveRecord(state *State, record storage.ArchiveRecord) error {
	target, err := archiveMap(state, record.Kind, true)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(record.Data), []byte("null")) {
		return errors.New("null archived recovery record")
	}
	if record.ID == "" || len(record.ID) > 1024 {
		return errors.New("invalid archived identity")
	}
	key := reflect.ValueOf(record.ID)
	if target.MapIndex(key).IsValid() {
		return errors.New("recovery record appears in both active state and archive")
	}
	value := reflect.New(target.Type().Elem())
	if err := json.Unmarshal(record.Data, value.Interface()); err != nil {
		return errors.New("invalid archived recovery record")
	}
	if value.Elem().Kind() == reflect.Pointer && value.Elem().IsNil() {
		return errors.New("null archived recovery record")
	}
	target.SetMapIndex(key, value.Elem())
	return nil
}

// CompleteState returns an independent logical snapshot. It rejects overlapping
// ownership and invalid record categories before restore or export publication.
// Desktop then runs the same full State validators used for unarchived backups.
func CompleteState(state State) (State, error) {
	if err := ValidateStateVersion(&state); err != nil {
		return State{}, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, err
	}
	defer clear(raw)
	var complete State
	if err = json.Unmarshal(raw, &complete); err != nil {
		return State{}, err
	}
	for _, record := range complete.Archive {
		if err = mergeArchiveRecord(&complete, record); err != nil {
			return State{}, err
		}
	}
	complete.Archive = nil
	if err := ValidateProtocolState(&complete); err != nil {
		return State{}, err
	}
	if err := ValidateCompleteFillState(&complete); err != nil {
		return State{}, err
	}
	if complete.Capacity != nil {
		complete.Capacity.Archived = storage.ArchiveStats{Kinds: map[string]uint64{}}
	}
	return complete, nil
}

func ValidateArchiveState(state State) error {
	if err := ValidateProtocolState(&state); err != nil {
		return err
	}
	if len(state.Archive) == 0 {
		if state.Capacity != nil && state.Capacity.Archived.Count != 0 {
			return errors.New("backup omits its declared archived recovery records")
		}
		return ValidateCompleteFillState(&state)
	}
	if state.Capacity == nil {
		return errors.New("unsupported archived wallet state")
	}
	stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
	seen := map[string]bool{}
	for _, record := range state.Archive {
		key := archiveMoveKey(record.Kind, record.ID)
		if seen[key] {
			return errors.New("duplicated archived recovery identity")
		}
		seen[key] = true
		if _, err := archiveRecordDigest(record); err != nil {
			return err
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		stats.Count++
		stats.Bytes += uint64(len(encoded) + 1)
		clear(encoded)
		stats.Kinds[record.Kind]++
	}
	if err := state.ValidateArchiveCheckpoint(stats); err != nil {
		return err
	}
	_, err := CompleteState(state)
	return err
}

// VaultSnapshot lets storage install imported archive records in the same
// transaction as their active checkpoint without importing daemon types.
func (s State) VaultSnapshot() (any, []storage.ArchiveRecord, error) {
	if err := ValidateProtocolState(&s); err != nil {
		return nil, nil, err
	}
	if len(s.Archive) == 0 {
		if err := ValidateCompleteFillState(&s); err != nil {
			return nil, nil, err
		}
		return s, nil, nil
	}
	if _, err := CompleteState(s); err != nil {
		return nil, nil, err
	}
	if s.Capacity == nil {
		return nil, nil, errors.New("archive requires a versioned capacity checkpoint")
	}
	records := s.Archive
	s.Archive = nil
	return s, records, nil
}

// LoadCompleteState includes same-vault archives in the chosen-password portable
// path as well as legacy database backups. The outer portable envelope remains
// the final encoding limit; this read never drops records to make it fit.
func LoadCompleteState(vault *storage.Vault) (State, error) {
	var state State
	records, stats, err := vault.LoadComplete(&state, 0)
	if err != nil {
		return State{}, err
	}
	if err := ValidateStateVersion(&state); err != nil {
		return State{}, err
	}
	if len(state.Archive) != 0 {
		return State{}, errors.New("vault contains embedded and bucket archive ownership")
	}
	if stats.Count > 0 && state.Capacity == nil {
		return State{}, errors.New("archive lacks its active checkpoint")
	}
	state.Archive = records
	if state.Capacity != nil && (stats.Count != state.Capacity.Archived.Count || stats.Bytes != state.Capacity.Archived.Bytes) {
		return State{}, errors.New("active and archive checkpoints disagree")
	}
	if _, err := CompleteState(state); err != nil {
		return State{}, fmt.Errorf("incomplete recovery archive: %w", err)
	}
	if err := ValidateArchiveState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (e *Engine) archivedValue(kind, id string, out any) (bool, error) {
	record, found, err := e.archiveRecord(kind, id)
	if err != nil || !found {
		if err != nil && e.archiveRead != nil {
			e.archiveReadError = err
		}
		return found, err
	}
	if err := json.Unmarshal(record.Data, out); err != nil {
		if e.archiveRead != nil {
			e.archiveReadError = err
		}
		return false, err
	}
	return true, nil
}

// ValidateArchiveRecordAgainstState decodes one cold record and rejects active
// ownership overlap without assembling lifetime history. The returned partial
// state carries the wallet/network context for desktop structural validators.
func ValidateArchiveRecordAgainstState(active State, record storage.ArchiveRecord) (State, error) {
	if err := ValidateStateVersion(&active); err != nil {
		return State{}, err
	}
	group, err := archiveMap(&active, record.Kind, false)
	if err != nil {
		return State{}, err
	}
	if group.IsValid() && group.MapIndex(reflect.ValueOf(record.ID)).IsValid() {
		return State{}, errors.New("recovery record appears in both active state and archive")
	}
	partial := State{Version: active.Version, Network: active.Network, Mnemonic: active.Mnemonic}
	if err := mergeArchiveRecord(&partial, record); err != nil {
		return State{}, err
	}
	if err := ValidateProtocolState(&partial); err != nil {
		return State{}, err
	}
	return partial, nil
}
