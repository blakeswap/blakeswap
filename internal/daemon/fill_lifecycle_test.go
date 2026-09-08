package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestParentFillRetirementReturnsOnlyUnfundedChildAndCannotRevive(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "open parent"
		if closed {
			name = "cancelled parent"
		}
		t.Run(name, func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.Blake)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s, f := e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
			p := e.s.ParentOrders[f.ParentID]
			if closed {
				raw, _ := json.Marshal(map[string]string{"id": p.Offer.ID, "expected_event_id": e.s.Offers[p.Offer.ID].ID.Hex()})
				if _, err := e.cancelOffer(raw); err != nil {
					t.Fatal(err)
				}
				if p.Quantities.Withdrawn != 600000 || p.Quantities.Reserved != 400000 || len(e.s.CoinReservations["swap/"+s.ID].Inputs) != 1 {
					t.Fatal("parent cancellation released accepted child resources")
				}
			}
			e.clocks[s.Long.Chain] = s.Terms.Long.RefundHeight
			e.clocks[s.Short.Chain] = s.Terms.Short.RefundHeight
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{}); err != nil {
				t.Fatal(err)
			}
			if !f.FundingDisabled || f.Allocation.EverCommitted || p.Quantities.Reserved != 0 || p.Fees[chain.Blake].Reserved != 0 || p.Fees[chain.Blake].Consumed != 0 || len(e.s.CoinReservations["swap/"+s.ID].Inputs) != 0 {
				t.Fatal("unfunded child did not irreversibly retire with its resource release")
			}
			if closed {
				if f.Allocation.Disposition != FillReleased || f.Allocation.currentQuantity() != 400000 || p.Quantities.Released != 1000000 || p.Quantities.Available != 0 {
					t.Fatal("cancelled-parent child was returned to available quantity")
				}
			} else if f.Allocation.Disposition != FillRetired || f.Allocation.currentQuantity() != 0 || p.Quantities.Available != 1000000 || p.Quantities.Released != 0 {
				t.Fatal("returned child double-counted its original quantity")
			}
			for _, d := range e.s.Outbox {
				if d.SwapID == s.ID && d.Type == "accepted" && (!d.Retired || d.Acknowledged) {
					t.Fatal("old acceptance was not retired distinctly from acknowledgment")
				}
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if saved.FillRecords[s.ID].Allocation != f.Allocation || !saved.FillRecords[s.ID].FundingDisabled {
				t.Fatal("quantity return was not persisted with irreversible refusal")
			}
			// A clock reorg and a late peer update cannot grant own funding.
			e.clocks[chain.BTC], e.clocks[chain.Blake] = 200, 200
			s.Stage = "late peer funding"
			before := protocol.Digest(*p)
			if err := e.advanceSwap(context.Background(), s, nil); err != nil || s.ShortFunding != "" || protocol.Digest(*p) != before {
				t.Fatal("returned child regained own funding after clock reorg")
			}
			if err := applyFillRequest(t, e, r, now); err != nil || protocol.Digest(*p) != before {
				t.Fatal("closed exact request allocated again")
			}
		})
	}
}

func TestParentFillRetirementRefusesUnknownOrPreviouslySignedCustody(t *testing.T) {
	for _, change := range []string{"ordinary error", "outage", "imported", "signed", "own txid", "refund bundle"} {
		t.Run(change, func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.BTC)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s := e.s.Swaps[r.ID]
			e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
			gate := e.gate(s.Terms, "fund-short")
			if !protocol.FundingWindowClosed(gate) {
				t.Fatal("fixture funding window still open")
			}
			switch change {
			case "ordinary error":
				gate = errors.New("clock unavailable")
			case "outage":
				e.chainFresh[chain.BTC] = false
			case "imported":
				e.s.FillRecords[r.ID].ImportedUncertain = true
			case "signed":
				s.ShortFunding = "saved exact bytes"
			case "own txid":
				s.Short.TxID = protocol.Digest("own funding")
			case "refund bundle":
				s.SelfRefunds = []string{"signed refund"}
			}
			before := protocol.Digest(e.s)
			if err := e.retireUnfundedMaker(s, gate); err == nil || protocol.Digest(e.s) != before {
				t.Fatal("uncertain or signed child returned quantity or money")
			}
		})
	}
}
