package daemon

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func swapIdentityKeys(s *Swap) ([]string, error) {
	if s == nil || !protocol.Hex32(s.ID) || s.Request.ID != s.ID || !protocol.Hex32(s.Request.Hash) || len(s.Request.Keys) != 2 || s.Request.Keys[chain.BTC] == s.Request.Keys[chain.Blake] {
		return nil, errors.New("invalid retained child identity")
	}
	keys := []string{"hash/" + s.Request.Hash}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if !protocol.ValidKey(s.Request.Keys[id]) {
			return nil, errors.New("noncanonical retained child key")
		}
		keys = append(keys, "key/"+s.Request.Keys[id])
	}
	if s.Terms != nil {
		if protocol.Digest(s.Terms.Request) != protocol.Digest(s.Request) || len(s.Terms.MakerKeys) != 2 {
			return nil, errors.New("retained terms changed child identity")
		}
		return fillIdentityKeys(s.Request, s.Terms.MakerKeys)
	}
	return keys, nil
}

func validateFillIdentityRecord(key, childID string) error {
	kind, value, ok := strings.Cut(key, "/")
	if !ok || !protocol.Hex32(childID) || (kind != "hash" && kind != "key") || (kind == "hash" && !protocol.Hex32(value)) || (kind == "key" && !protocol.ValidKey(value)) {
		return errors.New("invalid retained child identity index")
	}
	return nil
}

// Register both roles, including a taker's already known secret before its
// request can be published and maker keys when accepted terms first arrive.
// Exact same-child evidence is idempotent. Cold equality stays cold; no query
// reactivates old signing/publication authority or scans lifetime swap bodies.
func (e *Engine) retainSwapIdentity(s *Swap) error {
	keys, err := swapIdentityKeys(s)
	if err != nil {
		return err
	}
	var missing []string
	for _, key := range keys {
		previous := e.s.FillKeys[key]
		if previous == "" {
			if _, err := e.archivedValue("fill_keys", key, &previous); err != nil {
				return err
			}
		}
		if previous != "" && previous != s.ID {
			return errors.New("child identity was already assigned to another retained child")
		}
		if previous == "" {
			missing = append(missing, key)
		}
	}
	if e.s.FillKeys == nil {
		e.s.FillKeys = map[string]string{}
	}
	for _, key := range missing {
		e.s.FillKeys[key] = s.ID
	}
	return nil
}

func (e *Engine) retainActiveSwapIdentities() error {
	for _, s := range e.s.Swaps {
		if err := e.retainSwapIdentity(s); err != nil {
			return err
		}
	}
	return nil
}

// Opening checks index completeness against the committed archive using bounded
// point reads. Admission then needs only the small index, never a lifetime scan.
// Missing or conflicting imported indexes fail closed rather than hiding a
// secret already retained in another role or a retired cold child.
func validateVaultSwapIdentity(v *storage.Vault, state *State, s *Swap) error {
	keys, err := swapIdentityKeys(s)
	if err != nil {
		return err
	}
	for _, key := range keys {
		owner := state.FillKeys[key]
		if owner == "" {
			r, found, err := v.ReadArchive("fill_keys", key)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("retained child identity index is incomplete")
			}
			if err := json.Unmarshal(r.Data, &owner); err != nil {
				return err
			}
		}
		if owner != s.ID {
			return errors.New("retained child identity index conflicts with its signed request")
		}
	}
	return nil
}
