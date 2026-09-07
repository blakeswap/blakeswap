package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint only durable recovery material. Discovery refreshes, error text,
// observed confirmation counts and retry timestamps must not produce a backup
// reminder every polling tick. Exclusions are scoped to their known record paths;
// stages and nested authorization policy remain part of the recovery material. New durable fields are covered by default, including additional
// signed variants and authorization policies introduced by future migrations.
func BackupFingerprint(state State) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	for _, key := range []string{"backup", "book", "towers", "discovery_seen", "event_time"} {
		delete(value, key)
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

	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer clear(canonical)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
