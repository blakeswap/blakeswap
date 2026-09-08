package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestParentFillRetainedIndexConsistencyRejectsMissingOrAliasedKeys(t *testing.T) {
	for _, change := range []string{"current", "missing index", "request key alias", "index key alias", "conflicting index"} {
		t.Run(change, func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.Blake)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			key := "key/" + r.Keys[chain.BTC]
			switch change {
			case "missing index":
				delete(saved.FillKeys, "hash/"+r.Hash)
			case "request key alias":
				saved.Swaps[r.ID].Request.Keys[chain.BTC] = strings.ToUpper(r.Keys[chain.BTC])
				saved.Swaps[r.ID].Terms.Request = saved.Swaps[r.ID].Request
			case "index key alias":
				delete(saved.FillKeys, key)
				saved.FillKeys["key/"+strings.ToUpper(r.Keys[chain.BTC])] = r.ID
			case "conflicting index":
				saved.FillKeys[key] = protocol.Digest("another-child")
			}
			// Authenticated malformed data bypasses only the typed test writer;
			// production preflight/activation must still reject it unchanged.
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "source.db")
			password := []byte("disposable-retained-index")
			v, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Save(data); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = PreflightStateVersion(path, password)
			if (err == nil) != (change == "current") {
				t.Fatalf("readonly index check: %v", err)
			}
			v, _, err = openCurrentStateVault(path, password)
			if v != nil {
				v.Close()
			}
			if (err == nil) != (change == "current") {
				t.Fatalf("exclusive index check: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected index changed source bytes")
			}
		})
	}
}

func TestParentFillTakerRetainsAcceptedPeerKeysBeforeTerms(t *testing.T) {
	for _, collision := range []bool{false, true} {
		name := "new peer keys"
		if collision {
			name = "known other child key"
		}
		t.Run(name, func(t *testing.T) {
			makerEngine, maker, now := fillAdmissionEngine(t, chain.Blake)
			r := admissionRequest(t, makerEngine, maker, 400000)
			if err := applyFillRequest(t, makerEngine, r, now); err != nil {
				t.Fatal(err)
			}
			terms := makerEngine.s.Swaps[r.ID].Terms
			e, _ := receiveEngine(t)
			e.Config.Mode = "trader"
			e.s.Version, e.s.Network = StateVersion, chain.Regtest
			s := &Swap{ID: r.ID, Role: "taker", Request: r, Stage: "request queued"}
			e.s.Swaps = map[string]*Swap{r.ID: s}
			if err := e.retainSwapIdentity(s); err != nil {
				t.Fatal(err)
			}
			peerKey := "key/" + terms.MakerKeys[chain.BTC]
			if collision {
				e.s.FillKeys[peerKey] = protocol.Digest("previous-child")
			}
			before := protocol.Digest(e.s)
			raw, err := json.Marshal(terms)
			if err != nil {
				t.Fatal(err)
			}
			message := transport.Message{Version: transport.MessageVersion, Type: "accepted", SwapID: r.ID, Body: raw}
			err = e.handle(maker.Public().Hex(), message)
			if collision {
				if err == nil || protocol.Digest(e.s) != before {
					t.Fatal("conflicting accepted peer key changed child terms or identity state")
				}
				return
			}
			if err != nil || s.Terms == nil || e.s.FillKeys[peerKey] != r.ID {
				t.Fatalf("accepted peer identity missing: %v", err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if saved.FillKeys[peerKey] != r.ID || saved.Swaps[r.ID].Terms == nil {
				t.Fatal("peer keys and accepted terms were not retained together")
			}
			before = protocol.Digest(e.s)
			if err := e.handle(maker.Public().Hex(), message); err != nil || protocol.Digest(e.s) != before {
				t.Fatal("exact accepted retry changed retained identities")
			}
		})
	}
}

func TestParentFillIdentityArchiveLookupDoesNotReactivateOldChild(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.BTC)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	retained := *e.s.Swaps[r.ID]
	if err := e.stageArchive("swaps", r.ID); err != nil {
		t.Fatal(err)
	}
	for key := range e.s.FillKeys {
		if err := e.stageArchive("fill_keys", key); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.retainSwapIdentity(&retained); err != nil {
		t.Fatal(err)
	}
	if e.s.Swaps[r.ID] != nil || len(e.s.FillKeys) != 0 || len(e.archiveDeletes) != 0 {
		t.Fatal("identity read restored old runtime authority or duplicated cold keys")
	}
}
