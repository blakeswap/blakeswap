package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestParentFillRecoveryHoldsPublisherAndPreservesEveryQuantityAndCharge(t *testing.T) {
	for _, retired := range []bool{false, true} {
		e, maker, now := fillAdmissionEngine(t, chain.Blake)
		r := admissionRequest(t, e, maker, 400000)
		if err := applyFillRequest(t, e, r, now); err != nil {
			t.Fatal(err)
		}
		s, f := e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
		if retired {
			e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil {
				t.Fatal(err)
			}
		}
		p := e.s.ParentOrders[f.ParentID]
		ledger, allocation, inputs := p.Quantities, f.Allocation, protocol.Digest(f.Inputs)
		fees, bounties := protocol.Digest(p.Fees), protocol.Digest(p.Bounties)
		if err := PrepareRecovery(&e.s, now, false); err != nil {
			t.Fatal(err)
		}
		if !p.RestoreHold || !f.ImportedUncertain || p.Quantities != ledger || f.Allocation != allocation || protocol.Digest(f.Inputs) != inputs || protocol.Digest(p.Fees) != fees || protocol.Digest(p.Bounties) != bounties || f.FundingDisabled != retired {
			t.Fatal("import changed retained quantity, input, charge or irreversible refusal")
		}
		e.reconcileReservations()
		if _, remains := e.s.CoinReservations["offer/"+p.Offer.ID]; remains {
			t.Fatal("quarantined parent re-reserved its old available input pool")
		}
		if !retired && len(e.s.CoinReservations["swap/"+s.ID].Inputs) == 0 {
			t.Fatal("import dropped the unresolved child's exact input reservation")
		}
		if err := e.publishPendingParents(); err != nil || len(e.s.Offers) != 0 || len(e.s.Outbox) != 0 {
			t.Fatal("import resumed a parent publisher", err)
		}
		if err := applyFillRequest(t, e, r, now+2); err == nil && !retired {
			t.Fatal("unresolved imported child replay resumed acceptance")
		}
		// Re-export/re-import and ordinary policy reauthorization cannot clear
		// these distinct origin facts or turn returned quantity into allocation.
		raw, err := json.Marshal(e.s)
		if err != nil {
			t.Fatal(err)
		}
		var copy State
		if err := json.Unmarshal(raw, &copy); err != nil {
			t.Fatal(err)
		}
		if err := PrepareRecovery(&copy, now+3, false); err != nil {
			t.Fatal(err)
		}
		if !copy.ParentOrders[p.Offer.ID].RestoreHold || copy.FillRecords[s.ID].Allocation != allocation || copy.FillRecords[s.ID].FundingDisabled != retired {
			t.Fatal("re-export lost parent hold or original child allocation")
		}
	}
}

// Called after actual acceptance, retirement, authenticated late funding and
// a signature-verified peer refund in the lifecycle test, in both directions.
func assertRetiredImportPeerRecovery(t *testing.T, e *Engine, s *Swap, f *FillRecord, obs chain.Observation, now int64) {
	t.Helper()
	if err := PrepareRecovery(&e.s, now+3, false); err != nil {
		t.Fatal(err)
	}
	parentBefore := protocol.Digest(e.s.ParentOrders[f.ParentID])
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	point := chain.OutpointKey(s.Long.TxID, s.Long.Vout)
	all[s.Long.Chain][point] = obs
	if err := e.advanceSwap(context.Background(), s, all); err != nil || !e.recoverySwapResolved(s, all) {
		t.Fatal("positive peer refund did not resolve the imported irreversible refusal", err)
	}
	originalSwap, originalChild := *s, *f
	for _, problem := range []string{"outage", "unknown scan", "absent", "mempool", "wrong transaction", "bad signature", "own funding", "known secret", "refusal missing", "committed", "changed request"} {
		*s, *f = originalSwap, originalChild
		e.chainFresh[s.Long.Chain] = true
		all[s.Long.Chain] = map[string]chain.Observation{point: obs}
		switch problem {
		case "outage":
			e.chainFresh[s.Long.Chain] = false
		case "unknown scan":
			all[s.Long.Chain] = nil
		case "absent":
			all[s.Long.Chain] = map[string]chain.Observation{}
		case "mempool":
			copy := obs
			copy.Confirmations = 0
			all[s.Long.Chain][point] = copy
		case "wrong transaction":
			copy := obs
			copy.TxID = f.ParentID
			all[s.Long.Chain][point] = copy
		case "bad signature":
			copy := obs
			copy.Tx = obs.Tx.Copy()
			copy.Tx.TxIn[0].Witness[0][0] ^= 1
			all[s.Long.Chain][point] = copy
		case "own funding":
			s.ShortFunding = "00"
		case "known secret":
			s.SecretObserved = true
		case "refusal missing":
			f.FundingDisabled = false
		case "committed":
			f.Allocation.EverCommitted = true
		case "changed request":
			s.Request.Quantity++
		}
		if e.recoverySwapResolved(s, all) {
			t.Fatal("incomplete or contradictory retirement evidence resolved recovery", problem)
		}
	}
	*s, *f = originalSwap, originalChild
	e.chainFresh[s.Long.Chain] = true
	obs.Confirmations = archiveSettlementDepth
	all[s.Long.Chain] = map[string]chain.Observation{point: obs}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		previous := e.nodes[id].(*receiveBackend)
		e.nodes[id] = &activityArchiveBackend{receiveBackend: previous, generation: 1, hashes: map[uint32]string{400: "retained-current-tip"}}
		e.archiveCurrent[id] = recoveryCheckpoint{Height: 400, Hash: "retained-current-tip", Generation: 1}
		e.heights[id] = 400
	}
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if e.s.Swaps[s.ID] != nil || e.s.FillRecords[s.ID] != nil {
		t.Fatal("deep positive peer refund required a nonexistent own output before archival")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	retainedParent, err := e.retainedParentOrder(f.ParentID)
	if err != nil || !e.recoverySwapResolved(s, all) || protocol.Digest(retainedParent) != parentBefore {
		t.Fatal("cold refusal evidence was lost or retirement reallocated quantity", err)
	}
	if e.s.ParentOrders[f.ParentID] != nil || !retainedParent.RestoreHold || retainedParent.Quantities.Available != retainedParent.Quantities.Total {
		t.Fatal("retired imported parent kept hot lifetime authority or lost its saved hold")
	}
	if _, err := e.activateArchived("swaps", s.ID); err != nil {
		t.Fatal(err)
	}
	s, f = e.s.Swaps[s.ID], e.s.FillRecords[s.ID]
	if !e.restoredSwap(s.ID) || !f.FundingDisabled || f.Allocation.currentQuantity() != 0 || s.ShortFunding != "" {
		t.Fatal("reactivation lost imported origin or irreversible zero allocation")
	}
}
