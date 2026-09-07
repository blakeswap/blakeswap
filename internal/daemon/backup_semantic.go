package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Individual digests are compared for equality, never combined algebraically.
// The semantic token is a change identifier persisted alongside a successful
// state/archive transaction. It is not the archive's integrity authenticator.
type semanticParts struct {
	Scalar   [32]byte
	Records  map[string][32]byte
	Active   string
	Complete string
}

func stateSemanticParts(state State) (semanticParts, error) {
	parts := semanticParts{Records: map[string][32]byte{}}
	value, err := normalizedBackupValue(state)
	if err != nil {
		return parts, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return parts, err
	}
	digest := sha256.Sum256(canonical)
	clear(canonical)
	parts.Active = hex.EncodeToString(digest[:])
	if state.Capacity == nil || state.Capacity.Archived.Count == 0 {
		parts.Complete = parts.Active
	}
	for kind := range archiveFields {
		group, parent, name := backupRecordGroup(value, kind)
		for id, record := range group {
			digest, err := backupLeaf(kind, id, record)
			if err != nil {
				return parts, err
			}
			parts.Records[archiveMoveKey(kind, id)] = digest
		}
		delete(parent, name)
	}
	canonical, err = json.Marshal(value)
	if err != nil {
		return parts, err
	}
	parts.Scalar = sha256.Sum256(canonical)
	clear(canonical)
	return parts, nil
}

func (e *Engine) nextSemanticToken(parts semanticParts) (string, error) {
	previous := e.semanticParts
	token := ""
	if e.s.Capacity != nil {
		token = e.s.Capacity.SemanticToken
	}
	changed := token == ""
	if previous == nil {
		// A reopen can reuse its persisted token only when the active state still
		// matches the checkpoint which issued it. This also detects an older
		// writer that changed state without understanding freshness tokens.
		changed = changed || e.s.Capacity == nil || e.s.Capacity.ActiveFingerprint != parts.Active
	} else {
		changed = changed || previous.Scalar != parts.Scalar
		for key, prior := range previous.Records {
			if current, exists := parts.Records[key]; exists {
				changed = changed || current != prior
			} else if archived, moved := e.archivePuts[key]; moved {
				current, err := archiveRecordDigest(archived)
				if err != nil {
					return "", err
				}
				changed = changed || current != prior
			} else {
				changed = true
			}
		}
		for key, current := range parts.Records {
			if _, exists := previous.Records[key]; exists {
				continue
			}
			original, moved := e.archiveOrigins[key]
			changed = changed || !moved || original != current
		}
		for key := range e.archivePuts {
			if _, existed := previous.Records[key]; !existed {
				changed = true
			}
		}
		for key := range e.archiveDeletes {
			if _, exists := parts.Records[key]; !exists {
				changed = true
			}
		}
	}
	if !changed {
		return token, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// BackupSemanticToken belongs to the immutable exported snapshot. Passing this
// token back with the export result cannot mark subsequent state as backed up.
func BackupSemanticToken(state State) string {
	if state.Capacity == nil {
		return ""
	}
	return state.Capacity.SemanticToken
}
