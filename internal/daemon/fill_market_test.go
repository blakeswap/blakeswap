package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func fillMarket(t *testing.T, e *Engine) MarketPage {
	t.Helper()
	if e.Config.Name == "" {
		e.Config.Name = "fill-market"
	}
	raw, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Status: "all", Owner: "all"})
	result, err := e.Command(context.Background(), Request{Method: "market.list", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return result.(MarketPage)
}

func TestParentFillMarketKeepsRemainingParentOpenAndCancellable(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, sell)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			parent := e.s.ParentOrders[e.s.FillRecords[r.ID].ParentID]
			e.syncOrderRecords()
			rows := fillMarket(t, e).Records
			if len(rows) != 1 {
				t.Fatal("missing own parent")
			}
			row := rows[0]
			if row.Quantities == nil || row.Quantities.Available != 600000 || row.Quantities.Reserved != 400000 || row.Status != "open" || !row.CanCancel || row.CanRecreate || len(row.SwapIDs) != 1 || row.SwapIDs[0] != r.ID {
				t.Fatal("one accepted child closed or hid its remaining parent")
			}
			if _, err := parent.Offer.FillPolicy.Quote(parent.Offer.SellAmount, parent.Offer.BuyAmount, parent.Quantities.Available, row.SuggestedQuantity); err != nil {
				t.Fatal("suggested quantity leaves an invalid tail", err)
			}
			// Projection fixture: set a valid terminal allocation through the pure
			// accounting helpers. This tests the view, not chain settlement proof.
			next, child, err := parent.transitionFill(*e.s.FillRecords[r.ID], FillCommitted, false)
			if err != nil {
				t.Fatal(err)
			}
			next, child, err = next.transitionFill(child, FillFilled, false)
			if err != nil {
				t.Fatal(err)
			}
			*parent = next
			*e.s.FillRecords[r.ID] = child
			e.s.Swaps[r.ID].Stage = "completed"
			if err := e.retainOrderSettlement(e.s.Swaps[r.ID]); err != nil {
				t.Fatal(err)
			}
			if len(e.s.OrderRecords[parent.Offer.ID].Settlements) != 0 {
				t.Fatal("partial parent accumulated lifetime child IDs")
			}
			if err := e.stageArchive("swaps", r.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			row = fillMarket(t, e).Records[0]
			if row.Status != "open" || row.Quantities.Filled != 400000 || row.Quantities.Available != 600000 || !row.CanCancel || row.CanRecreate {
				t.Fatal("one cold completed child completed the parent")
			}
			page := queryFills(t, e, FillQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", ParentMaker: maker.Public().Hex(), ParentID: parent.Offer.ID, Limit: 1})
			if page.Total != 1 || page.Records[0].ID != r.ID || !page.Records[0].Archived {
				t.Fatal("parent lost complete paged cold child history")
			}
			parent.RestoreHold = true
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			held := fillMarket(t, e).Records[0]
			if held.Availability != "recovery_hold" || held.CanCancel || held.CanReplace || held.Quantities.Available != 600000 {
				t.Fatal("saved restore hold became market authority or erased bins")
			}
			parent.RestoreHold = false
			// Cancellation is local immediately even when signed availability waits.
			raw, _ := json.Marshal(map[string]string{"id": parent.Offer.ID, "expected_event_id": e.s.Offers[parent.Offer.ID].ID.Hex()})
			if _, err := e.cancelOffer(raw); err != nil {
				t.Fatal(err)
			}
			row = fillMarket(t, e).Records[0]
			if row.Status != "cancelled" || row.Quantities.Released != 600000 || row.Quantities.Filled != 400000 || row.CanCancel || row.CanReplace {
				t.Fatal("pending publication hid durable cancellation")
			}
		})
	}
}

func TestParentFillRemoteMarketUsesSignedAvailabilityAndLegalSuggestion(t *testing.T) {
	for _, total := range []int64{900000, 1100000} {
		t.Run(protocol.Digest(total)[:6], func(t *testing.T) {
			e, _, _ := fillAdmissionEngine(t, chain.BTC)
			e.s.OrderRecords = nil
			e.s.Offers = nil
			parent, maker := fillParentFixture(t, chain.Blake, 0)
			parent.Offer.SellAmount = total
			parent.Offer.BuyAmount = total + 1
			parent.Offer.Available = total
			parent.Offer.FillPolicy = protocol.FillPolicy{Mode: protocol.FillPartial, Min: 400000, Max: 600000}
			parent.Quantities.Total, parent.Quantities.Available = total, total
			parent.Economics = parent.Offer.EconomicsDigest()
			suggested, err := parent.Offer.FillPolicy.Suggested(total, total+1, total)
			if err != nil {
				t.Fatal(err)
			}
			request := fillRequestFixture(t, parent, maker, suggested.Sell)
			request.Taker = e.identity.Public().Hex()
			e.s.Book = map[string]nostr.Event{parent.Offer.Maker + ":" + parent.Offer.ID: request.OfferEvent}
			e.s.Swaps[request.ID] = &Swap{ID: request.ID, Role: "taker", Request: request, Stage: "awaiting maker acceptance"}
			e.marketObservedAt = time.Now().Unix()
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			row := fillMarket(t, e).Records[0]
			if row.Own || row.Quantities != nil || row.Status != "open" || !row.CanTake {
				t.Fatal("local pending child hid remote remaining availability")
			}
			if _, err := parent.Offer.FillPolicy.Quote(total, total+1, total, row.SuggestedQuantity); err != nil {
				t.Fatal("invalid default tail", err)
			}
		})
	}
}

func TestParentFillColdWholeMarketPreservesExactParentAndChildWithoutActivation(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	var parent *ParentOrder
	for _, p := range e.s.ParentOrders {
		parent = p
	}
	parent.Offer.FillPolicy = protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}
	parent.Economics = parent.Offer.EconomicsDigest()
	request := fillRequestFixture(t, *parent, maker, 1000000)
	e.stageOffer(parent.Offer, request.OfferEvent)
	if err := applyFillRequest(t, e, request, now); err != nil {
		t.Fatal(err)
	}
	parent = e.s.ParentOrders[parent.Offer.ID]
	e.syncOrderRecords()
	if row := fillMarket(t, e).Records[0]; !row.CanCancel || row.CanReplace || row.CanRecreate {
		t.Fatal("all-reserved parent cannot be closed against later returns")
	}
	if err := e.withdrawParentAvailable(parent.Offer.ID, now); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[request.ID]
	e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
	if err := e.advanceSwap(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.retainOrderSettlement(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"swaps", "offers", "order_records", "offer_towers", "parent_orders"} {
		id := parent.Offer.ID
		if kind == "swaps" {
			id = s.ID
		}
		if err := e.stageArchive(kind, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	before := protocol.Digest(e.s)
	row := fillMarket(t, e).Records[0]
	if row.Quantities == nil || row.Quantities.Released != 1000000 || len(row.SwapIDs) != 1 || row.SwapIDs[0] != s.ID || row.CanTake || row.CanCancel || row.CanReplace {
		t.Fatal("cold whole parent lost exact ledger or terminal link")
	}
	if protocol.Digest(e.s) != before || e.s.ParentOrders[parent.Offer.ID] != nil || e.s.Swaps[s.ID] != nil {
		t.Fatal("market reactivated cold authority")
	}
	record := OrderRecord{}
	if _, err := e.archivedValue("order_records", parent.Offer.ID, &record); err != nil {
		t.Fatal(err)
	}
	e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
		if kind == "parent_orders" {
			return storage.ArchiveRecord{}, false, errors.New("unreadable parent")
		}
		return e.vault.ReadArchive(kind, id)
	}
	if _, err := e.marketOrder(record.Offer, record.EventID, record, now); err == nil {
		t.Fatal("parent read failure became a guessed projection")
	}
	e.archiveRead = nil
}

func TestParentFillMarketRetirementRefusesMismatchedCompanionBeforeMetadata(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[r.ID]
	s.Stage = "completed"
	e.s.FillRecords[r.ID].RequestDigest = protocol.Digest("different request")
	before := protocol.Digest(e.s)
	if err := e.retainOrderSettlement(s); err == nil {
		t.Fatal("mismatched child became terminal parent metadata")
	}
	if protocol.Digest(e.s) != before {
		t.Fatal("failed metadata validation mutated authority")
	}
}
