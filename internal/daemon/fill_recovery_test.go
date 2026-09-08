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
			if err := e.advanceSwap(context.Background(), s, nil); err != nil {
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
