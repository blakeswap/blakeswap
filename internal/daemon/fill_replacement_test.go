package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func replacementFixture(t *testing.T) (*Engine, *sendBackend, TradeQuoteRequest, string) {
	t.Helper()
	e, p := partialQuoteFixture(t)
	b := e.nodes[chain.Blake].(*sendBackend)
	b.coins = nil
	for _, amount := range []int64{406500, 606500, 2000000} {
		b.coins = append(b.coins, chain.UTXO{TxID: transport.RandomID(), Amount: chain.Coins(amount), Script: hex.EncodeToString(e.scripts[chain.Blake]), Confirmations: 6})
	}
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := confirmQuote(t, e, confirmation(requestQuote(t, e, p)))
	r := admissionRequest(t, e, e.identity, 400000)
	now := time.Now().Unix()
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	parent := e.s.ParentOrders[result.ID]
	if e.s.FillRecords[r.ID] == nil {
		t.Fatal("fixture did not accept child")
	}
	// Advance only the fixture's per-address publication timestamp. Accepted
	// authority and all real input assignments come from the production paths.
	next, event, err := e.prepareParentPublication(*parent, now+1)
	if err != nil {
		t.Fatal(err)
	}
	if event != nil {
		*parent = next
		e.stageOffer(parentPublicOffer(next), *event)
	}
	p.OrderActionFields = OrderActionFields{OrderAction: "replace", SourceOfferID: result.ID, SourceEventID: e.s.Offers[result.ID].ID.Hex()}
	p.SellAmount, p.BuyAmount = 500000, 700001
	p.FillPolicy = protocol.FillPolicy{Mode: protocol.FillWhole, Min: 500000, Max: 500000}
	p.FeeBudgets = map[chain.ID]int64{chain.Blake: 73500, chain.BTC: 80000}
	p.BountyBudgets = map[chain.ID]int64{chain.Blake: 0, chain.BTC: 0}
	return e, b, p, r.ID
}

func TestParentFillReplacementTransfersOnlyRemainderAndUnassignedAuthorization(t *testing.T) {
	e, _, p, childID := replacementFixture(t)
	old := e.s.ParentOrders[p.SourceOfferID]
	childBefore := protocol.Digest(e.s.FillRecords[childID])
	swapBefore := protocol.Digest(e.s.Swaps[childID])
	assignedBefore := protocol.Digest(e.s.CoinReservations["swap/"+childID])
	q := requestQuote(t, e, p)
	oldPool := e.s.CoinReservations["offer/"+p.SourceOfferID]
	if len(q.Funds.Inputs) != 1 || q.Funds.Inputs[0] != oldPool.Inputs[0] {
		t.Fatal("replacement selected fresh or child-owned funds")
	}
	confirmation := confirmation(q)
	result := confirmQuote(t, e, confirmation)
	next := e.s.ParentOrders[result.ID]
	retained := e.s.ParentOrders[p.SourceOfferID]
	if next == nil || next.Quantities.Total != 500000 || next.Quantities.Available != 500000 || !retained.Quantities.Closed || retained.Quantities.Withdrawn != 500000 || retained.Quantities.Reserved != 400000 {
		t.Fatal("replacement moved accepted quantity")
	}
	if retained.Fees[chain.Blake].Limit != old.Fees[chain.Blake].Limit || retained.Fees[chain.Blake].Transferred != 73500 || retained.Fees[chain.Blake].Reserved != 26500 || retained.Fees[chain.BTC].Transferred != 80000 {
		t.Fatal("replacement duplicated reserved or consumed authorization")
	}
	if childBefore != protocol.Digest(e.s.FillRecords[childID]) || swapBefore != protocol.Digest(e.s.Swaps[childID]) || assignedBefore != protocol.Digest(e.s.CoinReservations["swap/"+childID]) {
		t.Fatal("replacement changed an accepted child")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.ParentOrders[p.SourceOfferID].Fees[chain.Blake].Transferred != 73500 || saved.OrderRecords[p.SourceOfferID].ReplacedBy != result.ID || saved.OrderRecords[result.ID].Replaces != p.SourceOfferID {
		t.Fatal("replacement chain and monetary transfer not atomic")
	}
	if _, exists := saved.CoinReservations["offer/"+p.SourceOfferID]; exists {
		t.Fatal("old unassigned input owner survived transfer")
	}
	before := protocol.Digest(e.s)
	if retry := confirmQuote(t, e, confirmation); retry != result || protocol.Digest(e.s) != before {
		t.Fatal("exact retry transferred twice")
	}
}

func TestParentFillReplacementRefusesQuantityAssetBudgetAndInputExpansion(t *testing.T) {
	for _, mode := range []string{"quantity", "asset", "reserved fee", "bounty", "missing pool", "child input", "fresh deposit"} {
		t.Run(mode, func(t *testing.T) {
			e, b, p, child := replacementFixture(t)
			switch mode {
			case "quantity":
				p.SellAmount = 600000
				p.Min, p.Max = 600000, 600000
			case "asset":
				p.Sell = chain.BTC
			case "reserved fee":
				p.FeeBudgets[chain.Blake]++
			case "bounty":
				p.BountyBudgets[chain.BTC] = 1
			case "missing pool":
				delete(e.s.CoinReservations, "offer/"+p.SourceOfferID)
			case "child input":
				e.s.CoinReservations["offer/"+p.SourceOfferID] = e.s.CoinReservations["swap/"+child]
			case "fresh deposit":
				// The original remaining pool still covers its own 6500 fee, while a
				// higher replacement fee would require the unrelated 2M deposit.
				b.coins[1].Amount = 510000
				if err := e.refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				p.FundingFee = 20000
			}
			before := protocol.Digest(e.s)
			raw, _ := json.Marshal(p)
			q, err := e.quoteTrade(context.Background(), raw)
			if err == nil && q.Ready {
				t.Fatal("expanded replacement was ready")
			}
			if protocol.Digest(e.s) != before {
				t.Fatal("failed replacement changed authority")
			}
		})
	}
}

func TestFillBudgetTransferUsesCheckedUnassignedAllowance(t *testing.T) {
	b := FillBudget{Limit: math.MaxInt64, Reserved: 20, Consumed: 30}
	next, err := b.transfer(math.MaxInt64 - 50)
	if err != nil || next.Limit != b.Limit || next.Reserved != 20 || next.Consumed != 30 {
		t.Fatal(err)
	}
	if _, err := next.transfer(1); err == nil {
		t.Fatal("transferred allowance reused")
	}
	if _, err := next.reserve(1); err == nil {
		t.Fatal("transfer remained available to child")
	}
	next, err = next.returnReserved(20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.reserve(20); err != nil {
		t.Fatal("later exact never-funded return lost its own authorization", err)
	}
	if _, err = next.transfer(21); err == nil {
		t.Fatal("consumed amount was transferred")
	}
}

func TestParentFillReplacementPreservesConsumedBudgetBothDirections(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			p, maker := fillParentFixture(t, sell, 125)
			p, child, err := p.reserveFill(fillRequestFixture(t, p, maker, 400000))
			if err != nil {
				t.Fatal(err)
			}
			p, committed, err := p.transitionFill(*child, FillCommitted, false)
			if err != nil {
				t.Fatal(err)
			}
			p.SignedRevision = p.Quantities.Revision
			offer := p.Offer
			offer.ID = transport.RandomID()
			offer.SellAmount, offer.BuyAmount = 600000, 800001
			offer.Available = 600000
			offer.FillPolicy = protocol.FillPolicy{Mode: protocol.FillWhole, Min: 600000, Max: 600000}
			limits := FillOrderFields{FillPolicy: offer.FillPolicy, FeeBudgets: map[chain.ID]int64{}, BountyBudgets: map[chain.ID]int64{}}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				limits.FeeBudgets[id] = p.Fees[id].Limit - p.Fees[id].Consumed
				limits.BountyBudgets[id] = p.Bounties[id].Limit - p.Bounties[id].Consumed
			}
			next, err := newParentOrder(offer, p.FundingPolicy, limits, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			retired, err := p.planReplacement(next)
			if err != nil {
				t.Fatal(err)
			}
			if retired.Quantities.Committed != 400000 || retired.Quantities.Withdrawn != 600000 || committed.Allocation.Disposition != FillCommitted {
				t.Fatal("funded child allocation changed")
			}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				if retired.Fees[id].Consumed != p.Fees[id].Consumed || retired.Bounties[id].Consumed != p.Bounties[id].Consumed || retired.Fees[id].Transferred+retired.Fees[id].Consumed != p.Fees[id].Limit || retired.Bounties[id].Transferred+retired.Bounties[id].Consumed != p.Bounties[id].Limit {
					t.Fatal("per-asset transfer recreated consumed permission")
				}
			}
		})
	}
}
