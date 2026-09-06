package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint only durable recovery material. Discovery refreshes, error text,
// display stages and retry timestamps must not produce a backup reminder every
// polling tick. New durable fields are covered by default, including additional
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
	var strip func(any)
	strip = func(item any) {
		switch item := item.(type) {
		case map[string]any:
			for _, key := range []string{"error", "stage", "last_attempt", "attempt", "claim_last_attempt", "refund_last_attempt", "claim_attempt", "refund_attempt", "confirmed", "confirmations", "long_confirmations", "short_confirmations"} {
				delete(item, key)
			}
			for _, child := range item {
				strip(child)
			}
		case []any:
			for _, child := range item {
				strip(child)
			}
		}
	}
	strip(value)
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer clear(canonical)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
