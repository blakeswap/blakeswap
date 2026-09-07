package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// Fingerprint only durable recovery material. Discovery refreshes, error text,
// observed confirmation counts and retry timestamps must not produce a backup
// reminder every polling tick. Exclusions are scoped to their known record paths;
// stages and nested authorization policy remain part of the recovery material. New durable fields are covered by default, including additional
// signed variants and authorization policies introduced by future migrations.
func normalizedBackupValue(state State) (map[string]any, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	for _, key := range []string{"backup", "capacity", "archive", "relay_sync", "public_versions", "public_limited", "version", "book", "towers", "discovery_seen", "event_time", "activity_revision", "activity_observation_sequence", "activity_indexes", "activity_error"} {
		delete(value, key)
	}
	if state.Capacity != nil && len(state.Capacity.Invalidated) != 0 {
		value["archive_invalidated_settlements"] = state.Capacity.Invalidated
	}
	if state.Capacity != nil && state.Capacity.Reactivating {
		value["archive_reactivation_required"] = true
	}
	stripFields := func(record any, keys ...string) {
		object, ok := record.(map[string]any)
		if !ok {
			return
		}
		for _, key := range keys {
			delete(object, key)
		}
	}
	records := func(name string, visit func(map[string]any)) {
		group, _ := value[name].(map[string]any)
		for _, record := range group {
			if object, ok := record.(map[string]any); ok {
				visit(object)
			}
		}
	}
	// Recovery readiness is recomputed from live chain evidence. Its polling
	// timestamp and explanatory issues do not change the archived obligations.
	if recovery, ok := value["recovery"].(map[string]any); ok {
		stripFields(recovery["status"], "checked_at", "issues")
	}
	// Keep the complete policy in the archive, but a no-op cadence check or
	// advisory reference refresh does not create new recovery obligations.
	// Config, revisions, holds, pending grants, charges and real actions remain.
	records("automations", func(record map[string]any) {
		stripFields(record, "next_action", "decision", "reference_events", "reference_observed")
	})
	records("maker_strategies", func(record map[string]any) { stripFields(record, "decision") })
	// Activity receipts, variants, outcomes, reorg history and provenance stay
	// covered. Only current observation polling and coverage cursors are noise;
	// these exclusions do not apply to historical outcomes or nested policy.
	records("activities", func(record map[string]any) {
		stripFields(record, "confirmations", "observed_at", "updated_at")
		observations, _ := record["observations"].([]any)
		for _, observation := range observations {
			stripFields(observation, "sequence", "confirmations", "observed_at", "error")
		}
	})
	records("swaps", func(record map[string]any) {
		stripFields(record, "error", "claim_last_attempt", "refund_last_attempt", "claim_attempt", "refund_attempt", "long_confirmations", "short_confirmations")
	})
	records("tower_jobs", func(record map[string]any) {
		stripFields(record, "error", "last_attempt", "attempt", "confirmed")
	})
	records("outbox", func(record map[string]any) { stripFields(record, "last_attempt") })
	records("sends", func(record map[string]any) {
		stripFields(record, "error", "last_attempt", "confirmations", "observe_cursor")
		for _, name := range []string{"variants", "history", "coins"} {
			list, _ := record[name].([]any)
			for _, item := range list {
				stripFields(item, "confirmations")
			}
		}
	})

	return value, nil
}

// Portable fingerprints cover the full canonical logical state. They are
// computed only for explicit backup operations; freshness polling uses the
// separately persisted semantic token, never an incremental XOR of hashes.
func BackupFingerprint(state State) (string, error) {
	if len(state.Archive) > 0 {
		complete, err := CompleteState(state)
		if err != nil {
			return "", err
		}
		state = complete
	} else if state.Capacity != nil && state.Capacity.Archived.Count > 0 {
		return "", errors.New("complete archived state is required for a portable fingerprint")
	}
	value, err := normalizedBackupValue(state)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func backupRecordGroup(value map[string]any, kind string) (map[string]any, map[string]any, string) {
	parent, name := value, kind
	switch kind {
	case "recovery_swaps", "recovery_sends", "recovery_tower_jobs", "quarantined_offers", "quarantined_outbox":
		parent, _ = value["recovery"].(map[string]any)
		switch kind {
		case "recovery_swaps":
			name = "swaps"
		case "recovery_sends":
			name = "sends"
		case "recovery_tower_jobs":
			name = "tower_jobs"
		}
	}
	group, _ := parent[name].(map[string]any)
	return group, parent, name
}
func backupLeaf(kind, id string, value any) ([32]byte, error) {
	raw, err := json.Marshal([]any{"blakeswap/backup-record/v2", kind, id, value})
	if err != nil {
		return [32]byte{}, err
	}
	defer clear(raw)
	return sha256.Sum256(raw), nil
}
func archiveRecordDigest(record storage.ArchiveRecord) ([32]byte, error) {
	var state State
	if err := mergeArchiveRecord(&state, record); err != nil {
		return [32]byte{}, err
	}
	value, err := normalizedBackupValue(state)
	if err != nil {
		return [32]byte{}, err
	}
	group, _, _ := backupRecordGroup(value, record.Kind)
	return backupLeaf(record.Kind, record.ID, group[record.ID])
}
