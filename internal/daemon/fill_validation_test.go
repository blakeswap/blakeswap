package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func conservationFixture(t *testing.T) (*Engine, string, []string) {
	t.Helper()
	e, maker, now := fillAdmissionEngine(t, chain.BTC)
	first := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, first, now); err != nil {
		t.Fatal(err)
	}
	parent := e.s.ParentOrders[e.s.FillRecords[first.ID].ParentID]
	next, event, err := e.prepareParentPublication(*parent, now+1)
	if err != nil || event == nil {
		t.Fatal("publication", err)
	}
	*parent = next
	e.stageOffer(parentPublicOffer(next), *event)
	second := admissionRequest(t, e, maker, 600000)
	if err := applyFillRequest(t, e, second, now+1); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
		t.Fatal("valid complete source", err)
	}
	return e, parent.Offer.ID, []string{first.ID, second.ID}
}

// Each broken record still satisfies its individual format/ledger validator.
// Only an authoritative completed graph can reject these contradictions.
func TestFillConservationCompletedBoundaryRejectsCrossRecordMismatch(t *testing.T) {
	for _, problem := range []string{"quantity bins", "reserved fee", "reserved bounty", "child economics", "parent binding", "request digest", "core missing", "allocation missing", "shared input"} {
		t.Run(problem, func(t *testing.T) {
			e, parentID, ids := conservationFixture(t)
			p, f := e.s.ParentOrders[parentID], e.s.FillRecords[ids[0]]
			switch problem {
			case "quantity bins":
				p.Quantities.Reserved -= 400000
				p.Quantities.Available += 400000
			case "reserved fee":
				b := p.Fees[chain.BTC]
				b.Reserved--
				p.Fees[chain.BTC] = b
			case "reserved bounty":
				b := p.Bounties[chain.BTC]
				b.Limit++
				b.Reserved++
				p.Bounties[chain.BTC] = b
			case "child economics":
				f.BuyAmount++
			case "parent binding":
				f.ParentID = ids[1]
			case "request digest":
				f.RequestDigest = protocol.Digest("different reviewed request")
			case "core missing":
				delete(e.s.Swaps, ids[0])
			case "allocation missing":
				delete(e.s.FillRecords, ids[0])
			case "shared input":
				f.Inputs = append([]CoinOutpoint(nil), e.s.FillRecords[ids[1]].Inputs...)
				e.s.CoinReservations["swap/"+ids[0]] = e.s.CoinReservations["swap/"+ids[1]]
			}
			if err := ValidateProtocolState(&e.s); err != nil {
				t.Fatal("regression must reach completed validation", err)
			}
			before, _ := json.Marshal(e.s)
			if err := ValidateVaultProtocolState(e.vault, &e.s); err == nil {
				t.Fatal("completed boundary accepted", problem)
			}
			after, _ := json.Marshal(e.s)
			if string(before) != string(after) {
				t.Fatal("validation changed rejected source")
			}
		})
	}
}

func TestFillConservationColdChildStillOwnsParentAllocation(t *testing.T) {
	e, parentID, ids := conservationFixture(t)
	if err := e.stageArchive("swaps", ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
		t.Fatal("valid split custody", err)
	}
	p := e.s.ParentOrders[parentID]
	p.Quantities.Reserved -= 400000
	p.Quantities.Available += 400000
	if err := ValidateVaultProtocolState(e.vault, &e.s); err == nil {
		t.Fatal("cold child allocation disappeared from completed conservation")
	}
}

func TestFillConservationCompletePortableRejectsMissingChild(t *testing.T) {
	e, _, ids := conservationFixture(t)
	delete(e.s.FillRecords, ids[0])
	if _, _, err := e.s.VaultSnapshot(); err == nil {
		t.Fatal("portable installation accepted missing child allocation")
	}
	// A single archive row remains an intentionally incomplete structural check.
	raw, err := json.Marshal(e.s.Swaps[ids[1]])
	if err != nil {
		t.Fatal(err)
	}
	partial := State{Version: StateVersion, Network: chain.Regtest}
	if _, err := ValidateArchiveRecordAgainstState(partial, storage.ArchiveRecord{Kind: "swaps", ID: ids[1], Data: raw}); err != nil {
		t.Fatal("single row incorrectly requires complete graph", err)
	}
}

// Install, validate and acquire separate custody of the original exported bytes.
// The source engine/vault and its later signed funding remain untouched.
func conservationRestoredEngine(t *testing.T, source *Engine, state State) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.db")
	password := []byte("independent conservation restore")
	if err := storage.Initialize(path, password, state); err != nil {
		t.Fatal(err)
	}
	v, saved, err := openCurrentStateVault(path, password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return &Engine{Config: source.Config, s: saved, vault: v, keys: source.keys, identity: source.identity, nodes: source.nodes, watch: source.watch,
		addresses: source.addresses, scripts: source.scripts, heights: maps.Clone(source.heights), clocks: maps.Clone(source.clocks), balances: map[chain.ID]int64{},
		chainFresh: maps.Clone(source.chainFresh), chainObserved: maps.Clone(source.chainObserved), chainGeneration: maps.Clone(source.chainGeneration), chainErrors: map[chain.ID]string{},
		fillValidation: captureFillValidation(&saved, nil)}
}

func TestFillConservationRetiredZeroAndParentWithdrawal(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.Blake)
			request := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, request, now); err != nil {
				t.Fatal(err)
			}
			core := e.s.Swaps[request.ID]
			parent := e.s.ParentOrders[e.s.FillRecords[core.ID].ParentID]
			if closed {
				if err := e.withdrawParentAvailable(parent.Offer.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			e.clocks[core.Long.Chain] = core.Terms.Long.RefundHeight
			e.clocks[core.Short.Chain] = core.Terms.Short.RefundHeight
			if err := e.advanceSwap(context.Background(), core, map[chain.ID]map[string]chain.Observation{}); err != nil {
				t.Fatal(err)
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
				t.Fatal(err)
			}
			if closed {
				if parent.Quantities.Released != 1000000 || parent.Quantities.Withdrawn != 600000 {
					t.Fatal("withdrawal and child release are distinct")
				}
			} else if e.s.FillRecords[core.ID].Allocation.currentQuantity() != 0 || parent.Quantities.Available != 1000000 {
				t.Fatal("retired child still allocates quantity")
			}
			if err := e.stageArchive("swaps", core.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
				t.Fatal("cold retired/released child", err)
			}
		})
	}
}

func TestFillConservationPermanentChargesAcrossColdSettlements(t *testing.T) {
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(asset), func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, asset)
			all := fillPairOutcomes(t, e, children, secrets, false)
			for _, core := range children {
				if err := e.advanceSwap(context.Background(), core, all); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			parentID := e.s.FillRecords[children[0].ID].ParentID
			for _, core := range children {
				if err := e.stageArchive("swaps", core.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.stageArchive("parent_orders", parentID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
				t.Fatal("complete cold graph", err)
			}
			if _, err := e.activateArchived("swaps", children[0].ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal("reactivation changed custody", err)
			}
			p := e.s.ParentOrders[parentID]
			money := p.Fees[asset]
			money.Consumed--
			p.Fees[asset] = money
			if err := ValidateVaultProtocolState(e.vault, &e.s); err == nil {
				t.Fatal("cold child lost permanent monetary charge")
			}
		})
	}
}

func TestFillConservationSaveRejectsDeltaWithoutChangingCommittedCheckpoint(t *testing.T) {
	for _, problem := range []string{"quantity", "fee", "immutable input", "core terms", "assignment overlap"} {
		t.Run(problem, func(t *testing.T) {
			e, parentID, ids := conservationFixture(t)
			var before State
			if _, err := e.vault.Load(&before); err != nil {
				t.Fatal(err)
			}
			parent, child := e.s.ParentOrders[parentID], e.s.FillRecords[ids[0]]
			switch problem {
			case "quantity":
				parent.Quantities.Available += 400000
				parent.Quantities.Reserved -= 400000
			case "fee":
				b := parent.Fees[chain.BTC]
				b.Limit++
				parent.Fees[chain.BTC] = b
			case "immutable input":
				child.Inputs[0].TxID = protocol.Digest("another input")
			case "core terms":
				core := e.s.Swaps[ids[0]]
				core.Terms.Long.RefundHeight++
				core.Terms.Takeover++
				core.Terms.RevealBefore++
				core.Long = core.Terms.Long
			case "assignment overlap":
				e.s.CoinReservations["offer/"+parentID] = e.s.CoinReservations["swap/"+ids[0]]
			}
			if err := e.save(); err == nil || e.fatal == nil {
				t.Fatal("incoherent write was not stopped", err)
			}
			var after State
			if _, err := e.vault.Load(&after); err != nil {
				t.Fatal(err)
			}
			if protocol.Digest(before) != protocol.Digest(after) {
				t.Fatal("rejected write changed committed source")
			}
		})
	}
}

func TestFillConservationUnchangedDeltaDoesNotReadColdHistory(t *testing.T) {
	e, _, ids := conservationFixture(t)
	if err := e.stageArchive("swaps", ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.archiveRead = func(string, string) (storage.ArchiveRecord, bool, error) {
		t.Fatal("unchanged save read a cold companion")
		return storage.ArchiveRecord{}, false, errors.New("unexpected read")
	}
	if _, err := e.prepareFillValidation(); err != nil {
		t.Fatal(err)
	}
}

func TestFillConservationCancellationCleansEncryptedScratch(t *testing.T) {
	e, _, _ := conservationFixture(t)
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateFillConservation(ctx, &e.s, vaultFillReader{e.vault}, directory); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled validation retained scratch", err, entries)
	}
	p := e.s.ParentOrders[e.s.FillRecords[firstKey(e.s.FillRecords)].ParentID]
	p.Quantities.Reserved -= 400000
	p.Quantities.Available += 400000
	if err := ValidateFillConservation(context.Background(), &e.s, vaultFillReader{e.vault}, directory); err == nil {
		t.Fatal("expected complete graph rejection")
	}
	entries, err = os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed validation retained scratch", err, entries)
	}
}
func firstKey[V any](values map[string]V) string {
	for key := range values {
		return key
	}
	return ""
}

func TestFillConservationStartupRefusalPreservesEncryptedSource(t *testing.T) {
	e, parentID, _ := conservationFixture(t)
	parent := e.s.ParentOrders[parentID]
	parent.Quantities.Reserved -= 400000
	parent.Quantities.Available += 400000
	// Deliberately construct an authenticated inconsistent source outside Engine
	// writes; both public existing-vault readers must reject without repair.
	if _, err := e.vault.CommitArchive(e.s, storage.ArchiveBatch{}, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	password := []byte("receive-test-password")
	if err := PreflightStateVersion(path, password); err == nil {
		t.Fatal("preflight accepted inconsistent source")
	}
	if v, _, err := openCurrentStateVault(path, password); err == nil {
		v.Close()
		t.Fatal("activation accepted inconsistent source")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected source was modified")
	}
}

func TestFillConservationColdMissingCompanionFailsWithoutScratchLeak(t *testing.T) {
	e, _, ids := conservationFixture(t)
	if err := e.stageArchive("swaps", ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "read error", "wrong identity"} {
		t.Run(mode, func(t *testing.T) {
			reader := conservationFaultReader{base: vaultFillReader{e.vault}, target: ids[0], mode: mode, owned: []byte("owned failed read")}
			directory := t.TempDir()
			if err := ValidateFillConservation(context.Background(), &e.s, reader, directory); err == nil {
				t.Fatal("unreadable companion accepted")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("failure retained scratch", err)
			}
			if mode == "read error" && !bytes.Equal(reader.owned, make([]byte, len(reader.owned))) {
				t.Fatal("failed point read retained owned data")
			}
		})
	}
}

type conservationFaultReader struct {
	base         vaultFillReader
	target, mode string
	owned        []byte
}

func (r conservationFaultReader) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	return r.base.VisitArchive(ctx, visit)
}
func (r conservationFaultReader) ReadArchive(kind, id string) (storage.ArchiveRecord, bool, error) {
	if kind == "fill_records" && id == r.target {
		switch r.mode {
		case "missing":
			return storage.ArchiveRecord{}, false, nil
		case "read error":
			return storage.ArchiveRecord{Data: r.owned}, true, errors.New("injected unavailable companion")
		case "wrong identity":
			record, found, err := r.base.ReadArchive(kind, id)
			record.ID = protocol.Digest("another child")
			return record, found, err
		}
	}
	return r.base.ReadArchive(kind, id)
}

func TestFillConservationOwnSignedAuthorityRequiresPermanentCharge(t *testing.T) {
	for _, evidence := range []string{"refund", "job", "sent", "owner fee cap"} {
		t.Run(evidence, func(t *testing.T) {
			e, _, ids := conservationFixture(t)
			core := e.s.Swaps[ids[0]]
			switch evidence {
			case "refund":
				core.SelfRefunds = []string{"retained own signed refund"}
			case "job":
				core.Jobs = []protocol.Job{{}}
			case "sent":
				core.ShortSent = true
			case "owner fee cap":
				core.OwnerFeeCap = 0
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err == nil {
				t.Fatal("completed state lost exact permanent authority", evidence)
			}
			if _, err := e.prepareFillValidation(); err == nil {
				t.Fatal("unchanged allocation hid changed own authority", evidence)
			}
		})
	}
}

func TestFillConservationCheckpointOwnsNestedReviewedPolicy(t *testing.T) {
	e, parentID, _ := conservationFixture(t)
	parent := e.s.ParentOrders[parentID]
	parent.Offer.Tower = &protocol.Tower{Scripts: map[chain.ID]string{chain.BTC: "reviewed payout"}}
	checkpoint := captureFillValidation(&e.s, map[string]string{})
	before := protocol.Digest(checkpoint.parents[parentID])
	parent.Offer.Tower.Scripts[chain.BTC] = "changed payout"
	parent.Offer.Tower.BPS++
	if protocol.Digest(checkpoint.parents[parentID]) != before {
		t.Fatal("live mutation changed previously reviewed nested policy")
	}
}
