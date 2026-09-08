package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/storage"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func strategyColdHistory(t *testing.T) (*Engine, *MakerStrategy, map[chain.ID]*activityArchiveBackend, string) {
	t.Helper()
	e, p := strategyFixture(t)
	e.s.Swaps = map[string]*Swap{}
	order, swap := transport.RandomID(), transport.RandomID()
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	child.Charges[order] = automationCharge(child.Config, order, 202000, 4000)
	charge := child.Charges[order]
	charge.State = "committed"
	charge.ExposureSettled = &StrategyExposureProof{SwapID: swap, Sell: chain.Blake, FundingTxID: strings.Repeat("1", 64), PeerFundingTxID: strings.Repeat("2", 64), SpendTxID: strings.Repeat("3", 64), PeerSpendTxID: strings.Repeat("4", 64), CheckedAt: time.Now().Unix() - 3600}
	e.s.Activities = map[string]Activity{}
	if e.s.Capacity == nil {
		e.s.Capacity = &CapacityRecord{}
	}
	e.s.Capacity.HistoryCoverage = map[chain.ID]HistoryCoverage{}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	backends := map[chain.ID]*activityArchiveBackend{}
	now := time.Now().Unix()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		backend := &activityArchiveBackend{generation: 2, hashes: map[uint32]string{1: "included-block", 400: "retained-history-prefix", 500: "current-tip"}}
		e.nodes[id] = backend
		backends[id] = backend
		e.chainGeneration[id] = 2
		e.chainFresh[id] = true
		e.chainObserved[id] = now
		e.archiveCurrent[id] = recoveryCheckpoint{Height: 500, Hash: "current-tip", Generation: 2}
		e.s.Capacity.HistoryCoverage[id] = HistoryCoverage{ID: transport.RandomID(), Height: 400, Hash: "retained-history-prefix"}
	}
	for _, row := range []Activity{
		{ID: "funding-row", Kind: "swap_funding", Chain: chain.Blake, TxID: strings.Repeat("1", 64), Principal: 200000, Fee: 4000, FeeKnown: true, FeePayer: "wallet", Movement: true},
		{ID: "claim-row", Kind: "swap_claim", Chain: chain.BTC, TxID: strings.Repeat("4", 64), Fee: 3200, FeeKnown: true, FeePayer: "wallet", Movement: true},
	} {
		row.Version = 1
		row.Network = chain.Regtest
		row.OrderID = order
		row.SwapID = swap
		row.Status = "confirmed"
		row.Confirmations = 400
		row.BlockHash = "included-block"
		row.ObservedAt = now - 3600
		row.Generation = 1
		row.ArchiveCoverage = e.s.Capacity.HistoryCoverage[row.Chain].ID
		row.Observations = []ActivityObservation{{TxID: row.TxID, Status: "confirmed", Confirmations: 400, Height: 1, BlockHash: row.BlockHash, ObservedAt: row.ObservedAt, Generation: 1}}
		e.s.Activities[row.ID] = row
		if err := e.stageArchive("activities", row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, p, backends, order
}
func coldStrategyReport(e *Engine, p *MakerStrategy) (StrategyView, error) {
	raw, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network), "expected_revision": p.Revision})
	return e.strategyReport(context.Background(), raw)
}
func TestStrategyReportOldColdRowsRequireTheirOwnCurrentCoverage(t *testing.T) {
	for _, mode := range []string{"current", "outage", "contradicted", "missing-history", "unrelated-new-core-anchor", "generation-change", "changed-binding", "held-proof"} {
		t.Run(mode, func(t *testing.T) {
			e, p, backends, _ := strategyColdHistory(t)
			switch mode {
			case "held-proof":
				for _, c := range e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)].Charges {
					c.ExposureSettled.Held = true
				}
			case "outage":
				backends[chain.BTC].failure = errors.New("unavailable")
			case "contradicted":
				backends[chain.BTC].hashes[400] = "other-fork"
			case "generation-change":
				backends[chain.BTC].afterRead = func(uint32) { backends[chain.BTC].generation = 3 }
			case "missing-history":
				e.s.Capacity.HistoryCoverage = nil
			case "unrelated-new-core-anchor":
				e.s.Capacity.HistoryCoverage = nil
				e.s.Capacity.Anchors = map[chain.ID]ArchiveAnchor{chain.BTC: {Height: 500, Hash: "current-tip"}, chain.Blake: {Height: 500, Hash: "current-tip"}}
			case "changed-binding":
				prefix := e.s.Capacity.HistoryCoverage[chain.BTC]
				prefix.ID = transport.RandomID()
				e.s.Capacity.HistoryCoverage[chain.BTC] = prefix
			}
			if err := e.vault.Save(e.s); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(e.s)
			v, err := coldStrategyReport(e, p)
			if mode != "current" {
				if err == nil || v.ReportIncluded {
					t.Fatal("unverified cold history reported as complete", mode)
				}
				return
			}
			if err != nil || !v.ReportIncluded || v.Inventory[chain.Blake].ConfirmedVolume != 200000 || v.Inventory[chain.Blake].KnownFees != 4000 || v.Inventory[chain.BTC].KnownFees != 3200 {
				t.Fatal("old authenticated rows lost actual volume/fees", v, err)
			}
			after, _ := json.Marshal(e.s)
			if string(before) != string(after) {
				t.Fatal("report changed authority or freshness")
			}
			var archived Activity
			if ok, err := e.archivedValue("activities", "funding-row", &archived); err != nil || !ok || archived.ObservedAt > time.Now().Unix()-3500 || archived.Generation != 1 {
				t.Fatal("report freshened old row provenance", err)
			}
		})
	}
}
func TestHistoryCoverageRejectsContradictionRepairAndMutationDuringScan(t *testing.T) {
	for _, mode := range []string{"contradiction-repaired", "generation", "hold-repaired"} {
		t.Run(mode, func(t *testing.T) {
			e, _, backends, _ := strategyColdHistory(t)
			seen := false
			_, err := e.VisitActivities(context.Background(), func(a Activity, cold bool) error {
				if !cold || seen {
					return nil
				}
				seen = true
				switch mode {
				case "contradiction-repaired":
					backends[chain.BTC].hashes[400] = "competing-prefix"
					backends[chain.BTC].afterRead = func(height uint32) {
						if height == 400 {
							backends[chain.BTC].hashes[400] = "retained-history-prefix"
						}
					}
				case "generation":
					backends[chain.BTC].generation = 3
				case "hold-repaired":
					e.mu.Lock()
					defer e.mu.Unlock()
					if err := e.invalidateArchive("observed competing prefix"); err != nil {
						return err
					}
					if err := e.reactivateArchive(); err != nil {
						return err
					}
					if err := e.save(); err != nil {
						return err
					}
					if e.s.Capacity.Reactivating {
						t.Fatal("fixture did not repair its temporary hold")
					}
				}
				return nil
			})
			if !seen || err == nil {
				t.Fatal("mixed or contradicted history view escaped final fence", mode, err)
			}
		})
	}
}
func TestStreamedRecoveryPreservesAdvisoryCoverageWithoutResumingAuthority(t *testing.T) {
	e, p, _, order := strategyColdHistory(t)
	before := e.s.Capacity.HistoryCoverage[chain.BTC]
	e.s.Capacity.Anchors = map[chain.ID]ArchiveAnchor{chain.BTC: {Height: 500, Hash: "current-tip"}}
	if err := PrepareStreamedRecovery(context.Background(), &e.s, e.s.Capacity.Archived, vaultFillReader{e.vault}, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Capacity.Anchors) != 0 || e.s.Capacity.HistoryCoverage[chain.BTC] != before {
		t.Fatal("import conflated advisory prefix with live settlement monitoring")
	}
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	if child.Enabled || !child.RestoreHold || child.Charges[order].State != "committed" || !child.Charges[order].ExposureSettled.Held {
		t.Fatal("cold report coverage resumed imported authority")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if _, err := e.VisitActivities(context.Background(), func(a Activity, cold bool) error {
		if cold && a.ArchiveVerified {
			seen++
		}
		return nil
	}); err != nil || seen != 2 {
		t.Fatal("retained advisory inclusion could not be revalidated", seen, err)
	}
}
func TestHistoryCoverageExtensionCannotAdoptOlderUnrelatedRows(t *testing.T) {
	e, _, backends, _ := strategyColdHistory(t)
	prior := e.s.Capacity.HistoryCoverage[chain.BTC]
	next, err := e.nextHistoryCoverage(context.Background(), chain.BTC)
	if err != nil || next.ID != prior.ID || next.Height != 500 {
		t.Fatal("canonical descendant lost continuity", err)
	}
	backends[chain.BTC].hashes[400] = "competing-prefix"
	next, err = e.nextHistoryCoverage(context.Background(), chain.BTC)
	if err == nil || next.ID != "" || !e.s.Capacity.Reactivating {
		t.Fatal("competing history was not held for bounded repair", err)
	}
	var row Activity
	if _, err := e.archivedValue("activities", "claim-row", &row); err != nil {
		t.Fatal(err)
	}
	if activityCovered(row, next) {
		t.Fatal("old row borrowed unrelated later prefix")
	}
}

type historySettlementBackend struct {
	*activityArchiveBackend
	transactions map[string]chain.Transaction
}

func (b *historySettlementBackend) Transaction(ctx context.Context, id string) (chain.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return chain.Transaction{}, err
	}
	tx, ok := b.transactions[id]
	if !ok {
		return chain.Transaction{}, &chain.RPCError{Code: -5}
	}
	return tx, nil
}
func (b *historySettlementBackend) HistoryTransaction(ctx context.Context, id string, oldHeight uint32, oldHash string) (chain.HistoryTransaction, error) {
	tx, err := b.Transaction(ctx, id)
	if err != nil {
		return chain.HistoryTransaction{}, err
	}
	tx.BlockHash = b.hashes[tx.Height]
	return chain.HistoryTransaction{Transaction: tx, Source: "private-fixture", Generation: b.generation, PreviousBlockChanged: oldHash != "" && oldHash != tx.BlockHash}, nil
}

func TestImportedStrategyRecoversCoreAndHistoricalProofWhilePolicyStaysHeld(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint("repair-", repair), func(t *testing.T) {
			e, swap, base, secret := isolatedFixtureSell(t, "maker", chain.Blake)
			_, template := strategyFixture(t)
			e.Config.Name = "restored-strategy"
			e.Config.Mode = "trader"
			cfg := template.Config
			cfg.Wallet = e.Config.Name
			cfg.Network = e.Config.Network
			p := &MakerStrategy{Config: cfg, WalletKey: e.identity.Public().Hex(), Revision: 1, Enabled: true}
			e.s.MakerStrategies = map[string]*MakerStrategy{cfg.ID: p}
			e.s.Automations = map[string]*AutomationPolicy{}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				c := cfg.policy(id)
				e.s.Automations[c.ID] = &AutomationPolicy{Config: c, WalletKey: p.WalletKey, Revision: 1, Enabled: true, Charges: map[string]*AutomationCharge{}}
			}
			offer := swap.Terms.Offer()
			if offer.Maker != e.identity.Public().Hex() {
				t.Fatal("fixture maker differs from the owned parent")
			}
			// The common fixture already binds current request/terms to its
			// committed maker allocation. Publish that ledger's reserved view;
			// changing the accepted OfferEvent would invalidate its exact digest.
			parent := e.s.ParentOrders[offer.ID]
			published := parentPublicOffer(*parent)
			event, err := e.signOffer(published, nostr.Now())
			if err != nil {
				t.Fatal(err)
			}
			parent.SignedRevision, parent.LastSignedAt = parent.Quantities.Revision, int64(event.CreatedAt)
			e.stageOffer(published, event)
			e.s.Outbox = map[string]*Delivery{}
			child := e.s.Automations[strategyPolicyID(cfg.ID, offer.Sell)]
			child.Charges[offer.ID] = automationCharge(child.Config, offer.ID, offer.BuyAmount, 2000)
			child.Charges[offer.ID].Volume = offer.SellAmount
			child.Charges[offer.ID].State = "committed"
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			backends := map[chain.ID]*historySettlementBackend{}
			e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				b := &historySettlementBackend{activityArchiveBackend: &activityArchiveBackend{receiveBackend: base.receiveBackend, generation: 1, hashes: map[uint32]string{1: "old-inclusion", 500: "old-prefix"}}, transactions: map[string]chain.Transaction{}}
				e.nodes[id] = b
				backends[id] = b
				e.chainFresh[id] = true
				e.chainGeneration[id] = 1
				e.chainObserved[id] = time.Now().Unix()
				e.heights[id] = 500
				e.archiveCurrent[id] = recoveryCheckpoint{Height: 500, Hash: "old-prefix", Generation: 1}
			}
			for _, c := range []contract.HTLC{swap.Long, swap.Short} {
				obs := recoverySpend(t, e, swap, c, false, secret)
				obs.Height = 1
				obs.Confirmations = 500
				all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = obs
				raw := swap.ShortFunding
				if c.Chain == swap.Long.Chain {
					raw = swap.LongFunding
					swap.SelfClaim = contract.Hex(obs.Tx)
					swap.SelfClaims = []string{swap.SelfClaim}
				}
				backends[c.Chain].transactions[c.TxID] = chain.Transaction{TxID: c.TxID, Hex: raw, Height: 1, Confirmations: 500, BlockHash: "old-inclusion"}
				backends[c.Chain].transactions[obs.TxID] = chain.Transaction{TxID: obs.TxID, Hex: contract.Hex(obs.Tx), Height: 1, Confirmations: 500, BlockHash: "old-inclusion"}
			}
			if err := e.advanceSwap(context.Background(), swap, all); err != nil {
				t.Fatal(err)
			}
			if swap.Stage != "completed" || child.Charges[offer.ID].ExposureSettled == nil {
				t.Fatal("positive ordinary scanner did not record original proof")
			}
			e.s.Outbox = map[string]*Delivery{}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				e.observeActivityChain(context.Background(), id)
			}
			for id, row := range e.s.Activities {
				row.ObservedAt -= 3600
				for n := range row.Observations {
					row.Observations[n].ObservedAt -= 3600
				}
				e.s.Activities[id] = row
			}
			if err := e.compactArchive(context.Background(), all, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if e.s.Capacity.Archived.Kinds["activities"] < 2 {
				t.Fatal("actual indexed funding/claim did not become cold")
			}
			previous := maps.Clone(e.s.Capacity.HistoryCoverage)
			restoreHistoryFixture(t, e)
			swap = e.s.Swaps[swap.ID]
			p = e.s.MakerStrategies[cfg.ID]
			child = e.s.Automations[strategyPolicyID(cfg.ID, offer.Sell)]
			e.strategyVerifiedSwaps = nil
			if !child.Charges[offer.ID].ExposureSettled.Held || !p.RestoreHold || p.Enabled {
				t.Fatal("import failed to separate proof and policy holds")
			}
			if len(e.s.Capacity.Anchors) != 0 {
				t.Fatal("import retained live obligation anchors")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if repair {
				for _, id := range []chain.ID{chain.BTC, chain.Blake} {
					backends[id].generation = 2
					backends[id].hashes[1] = "new-inclusion"
					backends[id].hashes[500] = "new-prefix"
					e.chainGeneration[id] = 2
				}
				if err := e.refreshArchiveCheckpoint(context.Background(), chain.BTC); err != nil {
					t.Fatal(err)
				}
				if !e.s.Capacity.Reactivating || e.recoveryTradingReady() == nil {
					t.Fatal("contradicted imported history did not hold new work")
				}
				if err := e.reactivateArchive(); err != nil {
					t.Fatal(err)
				}
				if e.s.Capacity.Reactivating || e.s.Capacity.Archived.Kinds["activities"] != 0 {
					t.Fatal("bounded fixture history did not reactivate")
				}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
			}
			e.recoveryCheckpoints = map[chain.ID]recoveryCheckpoint{}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				if err := e.refreshArchiveCheckpoint(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				e.recoveryCheckpoints[id] = e.archiveCurrent[id]
			}
			if err := e.advanceSwap(context.Background(), swap, all); err != nil {
				t.Fatal(err)
			}
			e.reconcileRecovery(all, nil)
			if child.Charges[offer.ID].ExposureSettled.Held || !p.RestoreHold || p.Enabled {
				t.Fatal("fresh recovery either lost proof or resumed policy")
			}
			// Restored core reconciliation can reactivate its advisory rows.
			// Run their ordinary current observer before allowing cold entry.
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				e.observeActivityChain(context.Background(), id)
			}
			if err := e.compactArchive(context.Background(), all, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if e.s.Swaps[swap.ID] != nil {
				t.Fatal("positively recovered core did not become cold")
			}
			if repair {
				for id, old := range previous {
					if e.s.Capacity.HistoryCoverage[id].ID == old.ID {
						t.Fatal("repaired row reused contradicted binding")
					}
				}
			}
			v, err := coldStrategyReport(e, p)
			if err != nil || !v.ReportIncluded || v.Inventory[offer.Sell].ConfirmedVolume != offer.SellAmount || v.Inventory[offer.Sell].KnownFees <= 0 || v.Inventory[offer.Sell.Other()].KnownFees <= 0 {
				t.Fatal("restored held policy lost positively reverified history report", v, err)
			}
			if !p.RestoreHold || p.Enabled || child.Charges[offer.ID].Volume != offer.SellAmount || child.Charges[offer.ID].State != "committed" {
				t.Fatal("report reset consumed authority")
			}
			if e.strategyHealth(p, time.Now().Unix()) == nil {
				t.Fatal("completed historical report authorized new automation")
			}
			if repair {
				var row Activity
				if found, err := e.archivedValue("activities", "swap/"+swap.ID+"/funding", &row); err != nil || !found {
					t.Fatal("reverified funding history missing", err)
				}
				retained := false
				for _, old := range row.History {
					if old.BlockHash == "old-inclusion" {
						retained = true
					}
				}
				if !retained {
					t.Fatal("reverification erased the old confirmed outcome")
				}
			}
		})
	}
}

// Exercise the same core promotion, publication quarantine and private atomic
// import primitives used by desktop, retaining the original vault unchanged.
func restoreHistoryFixture(t *testing.T, e *Engine) {
	t.Helper()
	source, err := e.vault.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var active State
	if _, _, err := source.LoadState(&active); err != nil {
		t.Fatal(err)
	}
	var cold []storage.ArchiveRecord
	stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
	if err := source.VisitArchive(context.Background(), func(record storage.ArchiveRecord) error {
		promoted, err := PromoteRecoveryRecord(&active, record)
		if err != nil || promoted {
			return err
		}
		record, _ = QuarantineArchiveRecord(record)
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		stats.Count++
		stats.Bytes += uint64(len(raw) + 1)
		stats.Kinds[record.Kind]++
		record.Data = append(json.RawMessage{}, record.Data...)
		cold = append(cold, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	active.Capacity.Archived = stats
	if err := PrepareStreamedRecovery(context.Background(), &active, stats, source, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	v, err := storage.Open(filepath.Join(t.TempDir(), "restored.db"), []byte("private-restored-history-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	if err := v.ImportArchive(context.Background(), active, stats, func(write func(storage.ArchiveRecord) error) error {
		for _, row := range cold {
			if err := write(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.vault = v
	e.s = active
}

func TestHistoryCoverageValidationKeepsMissingProofAndRejectsMalformedBindings(t *testing.T) {
	for _, mode := range []string{"missing", "valid", "invalid-chain", "invalid-id", "empty-hash", "oversize-hash", "zero-height", "bad-row-binding"} {
		t.Run(mode, func(t *testing.T) {
			s := State{Version: StateVersion, Network: chain.Regtest}
			if mode != "missing" {
				s.Capacity = &CapacityRecord{HistoryCoverage: map[chain.ID]HistoryCoverage{chain.BTC: {ID: transport.RandomID(), Height: 144, Hash: strings.Repeat("f", 64)}}}
			}
			if s.Capacity != nil {
				p := s.Capacity.HistoryCoverage[chain.BTC]
				switch mode {
				case "invalid-chain":
					delete(s.Capacity.HistoryCoverage, chain.BTC)
					s.Capacity.HistoryCoverage[chain.ID("other")] = p
				case "invalid-id":
					p.ID = "short"
				case "empty-hash":
					p.Hash = ""
				case "oversize-hash":
					p.Hash = strings.Repeat("f", 129)
				case "zero-height":
					p.Height = 0
				case "bad-row-binding":
					s.Activities = map[string]Activity{"row": {ArchiveCoverage: "bad"}}
				}
				if mode != "invalid-chain" {
					s.Capacity.HistoryCoverage[chain.BTC] = p
				}
			}
			before, _ := json.Marshal(s)
			err := ValidateHistoryCoverage(&s)
			if (err == nil) != (mode == "missing" || mode == "valid") {
				t.Fatal("coverage validation", err)
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Fatal("validation mutated facts")
			}
		})
	}
}
