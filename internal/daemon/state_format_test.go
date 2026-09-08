package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestStateCutoverRefusesLegacyAtEveryArchiveRecoveryBoundary(t *testing.T) {
	for _, version := range []int{0, 1, 2, 4} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			s := State{Version: version, Network: chain.Regtest, Mnemonic: "retained incompatible fixture"}
			before, _ := json.Marshal(s)
			checks := []func() error{
				func() error { return ValidateStateVersion(&s) },
				func() error { return s.ValidateArchiveCheckpoint(storage.ArchiveStats{}) },
				func() error { return ValidateArchiveState(s) },
				func() error { _, err := CompleteState(s); return err },
				func() error { _, _, err := s.VaultSnapshot(); return err },
				func() error { return PrepareRecovery(&s, time.Now().Unix(), false) },
				func() error { return PrepareStreamedRecovery(&s, storage.ArchiveStats{}, time.Now().Unix(), false) },
				func() error {
					_, err := ValidateArchiveRecordAgainstState(s, storage.ArchiveRecord{Kind: "seen", ID: "old", Data: json.RawMessage(`"digest"`)})
					return err
				},
			}
			for i, check := range checks {
				if err := check(); err == nil {
					t.Fatalf("boundary %d accepted state%d", i, version)
				}
				after, _ := json.Marshal(s)
				if !bytes.Equal(before, after) {
					t.Fatalf("boundary %d mutated rejected state", i)
				}
			}
		})
	}
}

func TestStateCutoverPreservesExistingVersionZeroAndLegacyVaults(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := t.TempDir()
			password := []byte("disposable-cutover-test-credential")
			passwordPath, path := filepath.Join(root, "password"), filepath.Join(root, "state.db")
			if err := os.WriteFile(passwordPath, password, 0600); err != nil {
				t.Fatal(err)
			}
			vault, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			// Construct old encoded data without calling the new State writer.
			if err := vault.Save(map[string]any{"version": version, "network": "regtest", "mnemonic": "must not be replaced", "seen": map[string]string{"preserve": "evidence"}}); err != nil {
				t.Fatal(err)
			}
			if err := vault.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{Network: chain.Regtest, Mode: "trader", Name: "cutover", DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel() // No configured nodes; require the actual format refusal.
			if engine, err := Open(ctx, c); err == nil {
				_ = engine.Close()
				t.Fatal("incompatible existing state was initialized or opened")
			} else if !strings.Contains(err.Error(), "incompatible development wallet state") {
				t.Fatalf("did not reach the format boundary: %v", err)
			}
			if err := CheckStoredNetwork(c); err == nil {
				t.Fatal("offline network guard accepted incompatible state")
			}
			if _, err := LoadStoredActions(c); err == nil {
				t.Fatal("offline action reader accepted incompatible state")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected source vault was changed: %v", err)
			}
			got, err := os.ReadFile(passwordPath)
			if err != nil || !bytes.Equal(got, password) {
				t.Fatalf("rejected source credential was changed: %v", err)
			}
		})
	}
}

func TestStateCutoverFirstCompactionKeepsStateThree(t *testing.T) {
	e := &Engine{s: State{Version: StateVersion, Network: chain.Regtest}}
	record := storage.ArchiveRecord{Kind: "seen", ID: "retained", Data: json.RawMessage(`"digest"`)}
	if err := e.archiveDelta(record, true); err != nil || e.s.Version != StateVersion {
		t.Fatalf("first compaction downgraded state: version%d %v", e.s.Version, err)
	}
	if err := e.s.ValidateArchiveCheckpoint(e.s.Capacity.Archived); err != nil {
		t.Fatal(err)
	}
	old := &Engine{s: State{Version: 2, Network: chain.Regtest}}
	if err := old.archiveDelta(record, true); err == nil || old.s.Capacity != nil || old.s.Version != 2 {
		t.Fatal("compaction relabelled or modified an incompatible state")
	}
}

func TestStateCutoverOnlyAbsentFileInitializesNewState(t *testing.T) {
	root := t.TempDir()
	password := []byte("disposable-new-cutover-credential")
	passwordPath := filepath.Join(root, "password")
	if err := os.WriteFile(passwordPath, password, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Network: chain.Regtest, Mode: "trader", Name: "new-cutover", DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	engine, _ := Open(ctx, c) // Deliberately absent nodes cannot affect local initialization.
	if engine != nil {
		_ = engine.Close()
	}
	v, err := storage.OpenReadOnly(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var s State
	if _, err := v.Load(&s); err != nil || s.Version != StateVersion || s.Mnemonic == "" {
		t.Fatalf("new vault did not initialize State3: version=%d err=%v", s.Version, err)
	}
}

func TestStateCutoverOuterMarkerCannotUpgradeLegacyChild(t *testing.T) {
	for _, version := range []int{0, 1} {
		child := &Swap{ID: "legacy-child", Role: "maker", Request: protocol.Request{Version: version}, Terms: &protocol.Terms{Version: version}}
		s := State{Version: StateVersion, Network: chain.Regtest, Swaps: map[string]*Swap{child.ID: child}}
		before, _ := json.Marshal(s)
		for i, check := range []func() error{
			func() error { return ValidateProtocolState(&s) },
			func() error { return ValidateArchiveState(s) },
			func() error { return PrepareRecovery(&s, time.Now().Unix(), false) },
			func() error { return PrepareStreamedRecovery(&s, storage.ArchiveStats{}, time.Now().Unix(), false) },
			func() error { _, _, err := s.VaultSnapshot(); return err },
		} {
			if err := check(); err == nil {
				t.Fatalf("boundary%d interpreted a legacy child under State3", i)
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("incompatible embedded child was mutated")
			}
		}
		encoded, _ := json.Marshal(child)
		active := State{Version: StateVersion, Network: chain.Regtest}
		if _, err := ValidateArchiveRecordAgainstState(active, storage.ArchiveRecord{Kind: "swaps", ID: child.ID, Data: encoded}); err == nil {
			t.Fatal("cold legacy child bypassed state cutover")
		}
	}
}

func TestStateCutoverWriterRejectsChangedSourceBeforeAnyWrite(t *testing.T) {
	for _, version := range []int{-1, 0, 1, 2, StateVersion} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := t.TempDir()
			path, replacement := filepath.Join(root, "state.db"), filepath.Join(root, "replacement.db")
			password := []byte("disposable-writer-preflight-credential")
			if err := storage.Initialize(path, password, State{Version: StateVersion, Network: chain.Regtest}); err != nil {
				t.Fatal(err)
			}
			if err := PreflightStateVersion(path, password); err != nil {
				t.Fatal(err)
			}
			if version < 0 {
				if err := os.WriteFile(replacement, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				v, err := storage.Open(replacement, password)
				if err != nil {
					t.Fatal(err)
				}
				if err := v.Save(map[string]any{"version": version, "network": "regtest"}); err != nil {
					t.Fatal(err)
				}
				if err := v.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			v, state, err := openCurrentStateVault(path, password)
			if (err == nil) != (version == StateVersion) {
				t.Fatalf("unexpected activation: version%d %v", version, err)
			}
			if v != nil {
				if state.Version != StateVersion {
					t.Fatal("wrong writer state")
				}
				if err := v.Close(); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("validation changed replacement bytes: %v", err)
			}
		})
	}
}

// A format-only fixture; signing/economic validation is tested in protocol.
func currentFormatSwap(t *testing.T, id string) *Swap {
	t.Helper()
	o := protocol.Offer{Version: protocol.Version, Network: chain.Regtest, ID: "parent", SellAmount: 1000000, BuyAmount: 1000000, Revision: 1, Available: 1000000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	id = protocol.Digest(id)
	keys := func() map[chain.ID]string {
		out := map[chain.ID]string{}
		for _, chainID := range []chain.ID{chain.BTC, chain.Blake} {
			k, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			out[chainID] = hex.EncodeToString(k.PubKey().SerializeCompressed())
		}
		return out
	}
	r := protocol.Request{ID: id, Hash: transport.RandomID(), Keys: keys(), Version: protocol.Version, Quantity: 1000000, Revision: 1, OfferEvent: nostr.Event{Content: string(raw)}}
	return &Swap{ID: id, Role: "maker", Request: r, Terms: &protocol.Terms{Version: protocol.Version, Request: r, MakerKeys: keys()}}
}

func TestStateCutoverColdProtocolCheckedBeforeAnyPromotion(t *testing.T) {
	for _, mode := range []string{"legacy core", "malformed last companion", "current"} {
		t.Run(mode, func(t *testing.T) {
			// Promotion requires the current accepted maker's exact custody,
			// in addition to the protocol marker under test.
			source, maker, now := fillAdmissionEngine(t, chain.BTC)
			request := admissionRequest(t, source, maker, 400000)
			if err := applyFillRequest(t, source, request, now); err != nil {
				t.Fatal(err)
			}
			child := *source.s.Swaps[request.ID]
			fill := source.s.FillRecords[child.ID]
			parent := source.s.ParentOrders[fill.ParentID]
			if mode == "legacy core" {
				child.Request.Version = 1
			}
			record := func(kind, id string, value any) storage.ArchiveRecord {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return storage.ArchiveRecord{Kind: kind, ID: id, Data: raw}
			}
			records := []storage.ArchiveRecord{
				record("swaps", child.ID, child),
				record("recovery_swaps", child.ID, true),
				record("parent_orders", parent.Offer.ID, parent),
				record("fill_records", child.ID, fill),
				record("funding_fees", "swap/"+child.ID, fill.FundingPolicy),
			}
			if mode == "malformed last companion" {
				records[len(records)-1].Data = json.RawMessage(`"not a fee"`)
			}
			e := &Engine{Config: source.Config, identity: maker, s: State{Version: StateVersion, Network: chain.Regtest}, archivePuts: map[string]storage.ArchiveRecord{}}
			for _, record := range records {
				e.archivePuts[archiveMoveKey(record.Kind, record.ID)] = record
				if err := e.archiveDelta(record, true); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(e.s)
			found, err := e.activateArchived("swaps", child.ID)
			if mode != "current" {
				if err == nil || found {
					t.Fatal("incompatible core or companion promoted")
				}
				after, _ := json.Marshal(e.s)
				if !bytes.Equal(before, after) || len(e.archivePuts) != len(records) || len(e.archiveOrigins) != 0 {
					t.Fatal("refused group partially promoted")
				}
			} else if err != nil || !found || e.s.Swaps[child.ID] == nil || e.s.Recovery == nil || !e.s.Recovery.Swaps[child.ID] || e.s.FundingFees["swap/"+child.ID] != fill.FundingPolicy || e.s.FillRecords[child.ID] == nil || e.s.ParentOrders[parent.Offer.ID] == nil || len(e.archivePuts) != 0 {
				t.Fatalf("coherent current activation: %v", err)
			}
		})
	}
}

func TestStateCutoverChecksPersistedColdProtocolPages(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			password := []byte("disposable-cold-cutover-credential")
			e := &Engine{s: State{Version: StateVersion, Network: chain.Regtest}}
			var records []storage.ArchiveRecord
			template, maker := fillParentFixture(t, chain.BTC, 0)
			for i := 0; i < 70; i++ {
				// Each persisted maker core carries its complete accepted custody;
				// the final negative changes only the child's protocol marker.
				parent := template.clone()
				parent.Offer.ID = protocol.Digest("cold-cutover-parent/" + strconv.Itoa(i))
				parent.Economics = parent.Offer.EconomicsDigest()
				request := fillRequestFixture(t, parent, maker, 400000)
				terms, err := protocol.NewTerms(request, currentFormatSwap(t, strconv.Itoa(i)).Terms.MakerKeys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100})
				if err != nil {
					t.Fatal(err)
				}
				parent, fill, err := parent.reserveFill(request)
				if err != nil {
					t.Fatal(err)
				}
				fill.Inputs = []CoinOutpoint{{TxID: protocol.Digest("cold-cutover-input/" + strconv.Itoa(i))}}
				child := &Swap{ID: request.ID, Role: "maker", Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, OwnerFeeCap: fill.FundingPolicy.OwnerFeeCap}
				for _, companion := range []struct {
					kind, id string
					value    any
				}{
					{"parent_orders", parent.Offer.ID, &parent}, {"fill_records", child.ID, fill}, {"funding_fees", "swap/" + child.ID, fill.FundingPolicy},
				} {
					data, err := json.Marshal(companion.value)
					if err != nil {
						t.Fatal(err)
					}
					record := storage.ArchiveRecord{Kind: companion.kind, ID: companion.id, Data: data}
					if err := e.archiveDelta(record, true); err != nil {
						t.Fatal(err)
					}
					records = append(records, record)
				}
				// Derive the valid retained index before corrupting just the
				// protocol marker for the incompatible-source negative control.
				keys, err := swapIdentityKeys(child)
				if err != nil {
					t.Fatal(err)
				}
				if legacy && i == 69 {
					child.Request.Version = 1
				}
				raw, _ := json.Marshal(child)
				record := storage.ArchiveRecord{Kind: "swaps", ID: child.ID, Data: raw}
				if err := e.archiveDelta(record, true); err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
				// Current-format retained child identity includes its derived index.
				for _, key := range keys {
					data, _ := json.Marshal(child.ID)
					index := storage.ArchiveRecord{Kind: "fill_keys", ID: key, Data: data}
					if err := e.archiveDelta(index, true); err != nil {
						t.Fatal(err)
					}
					records = append(records, index)
				}
			}
			v, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate an independently changed outer format marker, retaining
			// authenticated cold bytes; normal State writers must not create it.
			if _, err := v.CommitArchive(e.s, storage.ArchiveBatch{Put: records}, 0); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := PreflightStateVersion(path, password); (err != nil) != legacy {
				t.Fatalf("readonly cold check legacy%t: %v", legacy, err)
			}
			v, _, err = openCurrentStateVault(path, password)
			if v != nil {
				v.Close()
			}
			if (err != nil) != legacy {
				t.Fatalf("writer cold check legacy%t: %v", legacy, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("cold validation mutated source: %v", err)
			}
		})
	}
}

func TestStateCutoverValidatesActualQuarantinedOfferCategory(t *testing.T) {
	for _, version := range []int{1, protocol.Version} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			password := []byte("disposable-quarantine-format-credential")
			event := currentFormatSwap(t, "child").Request.OfferEvent
			var offer protocol.Offer
			if err := json.Unmarshal([]byte(event.Content), &offer); err != nil {
				t.Fatal(err)
			}
			offer.Version = version
			encoded, _ := json.Marshal(offer)
			event.Content = string(encoded)
			raw, _ := json.Marshal(event)
			record := storage.ArchiveRecord{Kind: "quarantined_offers", ID: offer.ID, Data: raw}
			e := &Engine{s: State{Version: StateVersion, Network: chain.Regtest}}
			if err := e.archiveDelta(record, true); err != nil {
				t.Fatal(err)
			}
			v, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.CommitArchive(e.s, storage.ArchiveBatch{Put: []storage.ArchiveRecord{record}}, 0); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := PreflightStateVersion(path, password); (err == nil) != (version == protocol.Version) {
				t.Fatalf("quarantine readonly version%d: %v", version, err)
			}
			v, _, err = openCurrentStateVault(path, password)
			if (err == nil) != (version == protocol.Version) {
				t.Fatalf("quarantine writer version%d: %v", version, err)
			}
			if v != nil {
				v.Close()
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("validation changed quarantined source")
			}
		})
	}
}
