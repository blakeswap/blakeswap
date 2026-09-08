package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func marketOffer(t *testing.T, e *Engine, own bool, sell chain.ID, btc, blake int64, status string, expires int64) protocol.Offer {
	t.Helper()
	key := nostr.Generate()
	if own {
		key = e.identity
	}
	o := protocol.Offer{Version: protocol.Version, ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: sell, SellAmount: btc, BuyAmount: blake, Status: "open", Expires: expires, Revision: 1}
	if sell == chain.Blake {
		o.SellAmount, o.BuyAmount = blake, btc
	}
	o.FillPolicy = protocol.FillPolicy{Mode: protocol.FillWhole, Min: o.SellAmount, Max: o.SellAmount}
	o.Available = o.SellAmount
	if own {
		policy := FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}
		parent, err := newParentOrder(o, policy, FillOrderFields{FillPolicy: o.FillPolicy, FeeBudgets: map[chain.ID]int64{sell: 22000, sell.Other(): 20000}, BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}}, min(time.Now().Unix(), expires-1))
		if err != nil {
			t.Fatal(err)
		}
		if e.s.ParentOrders == nil {
			e.s.ParentOrders = map[string]*ParentOrder{}
		}
		if e.s.FillRecords == nil {
			e.s.FillRecords = map[string]*FillRecord{}
		}
		if e.s.FundingFees == nil {
			e.s.FundingFees = map[string]FeeSelection{}
		}
		e.s.ParentOrders[o.ID] = parent
		e.s.FundingFees["offer/"+o.ID] = policy
		var child *Swap
		switch status {
		case "open":
		case "cancelled":
			parent.Quantities, err = parent.Quantities.withdrawAvailable()
		case "reserved", "filled", "refunded":
			// Projection fixture only: create consistent signed child identity and
			// use the pure accounting transitions, not simulated chain evidence.
			request := fillRequestFixture(t, *parent, key, o.SellAmount)
			var allocation *FillRecord
			*parent, allocation, err = parent.reserveFill(request)
			if err != nil {
				t.Fatal(err)
			}
			allocation.Inputs = []CoinOutpoint{{TxID: transport.RandomID()}}
			keys, keyErr := e.swapKeys(request.ID)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			terms, termsErr := protocol.NewTerms(request, keys, e.heights)
			if termsErr != nil {
				t.Fatal(termsErr)
			}
			child = &Swap{ID: request.ID, Role: "maker", Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, Stage: "awaiting taker funding"}
			if status != "reserved" {
				var next FillRecord
				*parent, next, err = parent.transitionFill(*allocation, FillCommitted, false)
				if err != nil {
					t.Fatal(err)
				}
				disposition := FillFilled
				child.Stage = "completed"
				if status == "refunded" {
					disposition, child.Stage = FillReleased, "refunded"
				}
				*parent, next, err = parent.transitionFill(next, disposition, false)
				*allocation = next
			}
			e.s.FillRecords[request.ID] = allocation
			e.s.Swaps[request.ID] = child
			e.s.FundingFees["swap/"+request.ID] = policy
		default:
			t.Fatal("unsupported market fixture status", status)
		}
		if err != nil {
			t.Fatal(err)
		}
		o = parentPublicOffer(*parent)
		event, err := e.signOffer(o, nostr.Timestamp(min(time.Now().Unix(), expires-1)))
		if err != nil {
			t.Fatal(err)
		}
		parent.SignedRevision, parent.LastSignedAt = o.Revision, int64(event.CreatedAt)
		e.stageOffer(o, event)
		if err := validateParentOrder(o.ID, parent); err != nil {
			t.Fatal(err)
		}
		if child != nil {
			if err := e.retainOrderSettlement(child); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		o.Status = status
		if status != "open" {
			o.Available = 0
		}
		content, err := o.PublicJSON()
		if err != nil {
			t.Fatal(err)
		}
		ev := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", o.Network.Namespace()}}, Content: string(content)}
		if err := transport.Sign(&ev, key); err != nil {
			t.Fatal(err)
		}
		e.s.Book[o.Maker+":"+o.ID] = ev
	}
	return o
}
func queryMarket(t *testing.T, e *Engine, q MarketQuery) MarketPage {
	t.Helper()
	q.ExpectedWallet = e.Config.Name
	q.ExpectedNetwork = string(e.Config.Network)
	raw, _ := json.Marshal(q)
	p, err := e.marketPage(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestMarketExactSortingBothDirectionsAndStableTies(t *testing.T) {
	e, _ := tradeFixture(t, "maker")
	now := time.Now().Unix()
	// Products exceed int64 and these two rates round to the same display value.
	low := marketOffer(t, e, false, chain.BTC, 9_999_999_999, 10_000_000_000, "open", now+300)
	high := marketOffer(t, e, false, chain.Blake, 9_999_999_998, 9_999_999_999, "open", now+200)
	a := marketOffer(t, e, true, chain.Blake, 100_000, 200_000, "open", now+500)
	b := marketOffer(t, e, false, chain.BTC, 200_000, 400_000, "open", now+400)
	p := queryMarket(t, e, MarketQuery{Status: "all", Sort: "rate"})
	if len(p.Records) != 4 || p.Records[0].Offer.ID != low.ID || p.Records[1].Offer.ID != high.ID || p.Records[0].Rate != p.Records[1].Rate {
		t.Fatal("exact extreme-rate ordering lost", p.Records)
	}
	tied := []string{a.Maker + ":" + a.ID, b.Maker + ":" + b.ID}
	sort.Strings(tied)
	for _, desc := range []bool{false, true} {
		p = queryMarket(t, e, MarketQuery{Status: "all", Sort: "rate", Descending: desc})
		start := 2
		if desc {
			start = 0
		}
		for i, id := range tied {
			if p.Records[start+i].Offer.Maker+":"+p.Records[start+i].Offer.ID != id {
				t.Fatal("rate tie unstable", desc, p.Records)
			}
		}
	}
	for _, key := range []string{"size", "expiry"} {
		p = queryMarket(t, e, MarketQuery{Status: "all", Sort: key})
		for i := 1; i < len(p.Records); i++ {
			if marketLess(p.Records[i], p.Records[i-1], key, false) {
				t.Fatal(key)
			}
		}
	}
}
func TestMarketOwnerAndAmountFiltersPreserveWalletIntent(t *testing.T) {
	e, _ := tradeFixture(t, "maker")
	now := time.Now().Unix()
	e.marketObservedAt = now
	for _, own := range []bool{false, true} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			marketOffer(t, e, own, sell, 100_000, 200_000, "open", now+3600)
		}
	}
	for _, owner := range []string{"all", "mine", "others"} {
		for _, side := range []string{"buy_btc", "sell_btc"} {
			p := queryMarket(t, e, MarketQuery{Owner: owner, Side: side, BTCMin: 100_000, BTCMax: 100_000})
			want := 1
			if owner == "all" {
				want = 2
			}
			if len(p.Records) != want {
				t.Fatal(owner, side, p)
			}
			for _, r := range p.Records {
				sellsBTC := (r.Offer.Sell == chain.BTC) == r.Own
				if (side == "sell_btc") != sellsBTC {
					t.Fatal("owner filter reversed intent", r)
				}
			}
		}
	}
	if p := queryMarket(t, e, MarketQuery{BTCMin: 100_001}); p.Total != 0 || p.Records == nil {
		t.Fatal("empty filter", p)
	}
	p := queryMarket(t, e, MarketQuery{Limit: 1})
	next := queryMarket(t, e, MarketQuery{Limit: 1, Offset: 1, Revision: p.Revision})
	if next.Revision != p.Revision || next.Records[0].Offer.ID == p.Records[0].Offer.ID {
		t.Fatal("paging", p, next)
	}
	marketOffer(t, e, false, chain.Blake, 300_000, 600_000, "open", now+3600)
	raw, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Limit: 1, Offset: 1, Revision: p.Revision})
	if _, err := e.marketPage(raw); err == nil {
		t.Fatal("changed page accepted")
	}
}
func TestMarketDurableStatusesStaleRequestsAndFinishedRecreation(t *testing.T) {
	e, _ := tradeFixture(t, "maker")
	now := time.Now().Unix()
	for _, status := range []string{"open", "reserved", "filled", "cancelled"} {
		marketOffer(t, e, true, chain.BTC, 100_000, 200_000, status, now+600)
	}
	marketOffer(t, e, true, chain.Blake, 100_000, 200_000, "open", now-1)
	remote := marketOffer(t, e, false, chain.Blake, 100_000, 200_000, "open", now+600)
	p := queryMarket(t, e, MarketQuery{Owner: "others"})
	if len(p.Records) != 1 || p.Records[0].CanTake || p.Records[0].Availability != "stale" {
		t.Fatal(p)
	}
	e.marketObservedAt = now
	pendingID := transport.RandomID()
	keys, err := e.swapKeys(pendingID)
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{Version: protocol.Version, ID: pendingID, Taker: e.identity.Public().Hex(), Hash: transport.RandomID(), Keys: keys, Revision: remote.Revision, Quantity: remote.SellAmount, OfferEvent: e.s.Book[remote.Maker+":"+remote.ID]}
	if _, err := request.Validate(now); err != nil {
		t.Fatal(err)
	}
	e.s.Swaps[pendingID] = &Swap{ID: pendingID, Role: "taker", Stage: "request queued", Request: request}
	e.s.FundingFees["swap/"+pendingID] = FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}
	p = queryMarket(t, e, MarketQuery{Owner: "others", Status: "open"})
	if len(p.Records) != 1 || !p.Records[0].CanTake || p.Records[0].Quantities != nil || p.Records[0].Offer.Available != remote.Available || p.Records[0].Offer.Revision != remote.Revision || !reflect.DeepEqual(p.Records[0].SwapIDs, []string{pendingID}) {
		t.Fatal("a local pending child changed the remote signed parent", p)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.s.Book = map[string]nostr.Event{}
	for _, status := range []string{"open", "reserved", "filled", "cancelled", "expired"} {
		p = queryMarket(t, e, MarketQuery{Owner: "mine", Status: status})
		if p.Total != 1 {
			t.Fatal(status, p)
		}
	}
	old := marketOffer(t, e, true, chain.Blake, 100_000, 200_000, "refunded", now+600)
	var refunded *Swap
	for id, child := range e.s.FillRecords {
		if child.ParentID == old.ID {
			refunded = e.s.Swaps[id]
		}
	}
	if refunded == nil || e.s.FillRecords[refunded.ID].Allocation.Disposition != FillReleased {
		t.Fatal("refunded history lost its exact released child")
	}
	p = queryMarket(t, e, MarketQuery{Owner: "mine", Status: "refunded"})
	if p.Total != 1 || !p.Records[0].CanRecreate || p.Records[0].CanReplace || p.Records[0].CanCancel || p.Records[0].Quantities.Released != old.SellAmount || !reflect.DeepEqual(p.Records[0].SwapIDs, []string{refunded.ID}) {
		t.Fatal(p)
	}
	fields := OrderActionFields{OrderAction: "recreate", SourceOfferID: old.ID, SourceEventID: e.s.Offers[old.ID].ID.Hex()}
	if _, err := e.orderSource(fields, now); err != nil {
		t.Fatal(err)
	}
	parent := e.s.ParentOrders[old.ID]
	nextParent, nextChild, err := parent.transitionFill(*e.s.FillRecords[refunded.ID], FillCommitted, false)
	if err != nil {
		t.Fatal(err)
	}
	*parent, *e.s.FillRecords[refunded.ID] = nextParent, nextChild
	refunded.Stage = "refunding"
	if _, err := e.orderSource(fields, now); err == nil {
		t.Fatal("unfinished swap recreated")
	}
}
func TestMarketCustomExpiryAndInvalidFilters(t *testing.T) {
	for _, delta := range []int64{-1, 8 * 86400, 1234} {
		e, p := tradeFixture(t, "maker")
		p.Expires = time.Now().Unix() + delta
		raw, _ := json.Marshal(p)
		q, err := e.quoteTrade(context.Background(), raw)
		if delta == 1234 {
			if err != nil || q.OfferExpires != p.Expires || !q.Ready {
				t.Fatal(q, err)
			}
			r := confirmQuote(t, e, confirmation(q))
			o, _ := historicalOffer(e.s.Offers[r.ID])
			if o.Expires != p.Expires {
				t.Fatal(o)
			}
		} else if err == nil && q.Ready {
			t.Fatal("invalid expiry", q)
		}
	}
	e, _ := tradeFixture(t, "maker")
	for _, q := range []MarketQuery{{Owner: "bad"}, {Side: "btc"}, {BTCMin: -1}, {BTCMax: 100_000_000_000}, {Sort: "random"}, {Status: "gone"}, {Limit: 501}, {Offset: -1}, {ExpectedWallet: "other"}} {
		if q.ExpectedWallet == "" {
			q.ExpectedWallet = e.Config.Name
		}
		q.ExpectedNetwork = "regtest"
		raw, _ := json.Marshal(q)
		if _, err := e.marketPage(raw); err == nil {
			t.Fatal("invalid query", q)
		}
	}
}
func TestOrderCancellationRacesAcceptanceAndRejectsStaleCopies(t *testing.T) {
	for _, ordering := range []string{"cancel-first", "accept-first", "race"} {
		t.Run(ordering, func(t *testing.T) {
			e, p, old := managedSource(t)
			from, msg := orderRequest(t, e, old)
			originalInputs := e.s.CoinReservations["offer/"+p.SourceOfferID].Inputs
			raw, _ := json.Marshal(map[string]string{"id": p.SourceOfferID, "expected_wallet": e.Config.Name, "expected_event_id": old.ID.Hex()})
			var cancelErr, acceptErr error
			cancel := func() { e.mu.Lock(); defer e.mu.Unlock(); _, cancelErr = e.cancelOffer(raw) }
			accept := func() { e.mu.Lock(); defer e.mu.Unlock(); acceptErr = e.handle(from, msg) }
			switch ordering {
			case "cancel-first":
				cancel()
				accept()
			case "accept-first":
				accept()
				cancel()
			default:
				var wg sync.WaitGroup
				wg.Add(2)
				start := make(chan struct{})
				go func() { defer wg.Done(); <-start; cancel() }()
				go func() { defer wg.Done(); <-start; accept() }()
				close(start)
				wg.Wait()
			}
			if acceptErr != nil {
				t.Fatal(acceptErr)
			}
			if cancelErr != nil {
				// Acceptance can publish a newer revision before cancellation reaches
				// the lock. Refresh its exact current event, then close the remainder.
				if !strings.Contains(cancelErr.Error(), "order changed") || e.s.Offers[p.SourceOfferID].ID == old.ID {
					t.Fatal(cancelErr)
				}
				raw, _ = json.Marshal(map[string]string{"id": p.SourceOfferID, "expected_wallet": e.Config.Name, "expected_event_id": e.s.Offers[p.SourceOfferID].ID.Hex()})
				cancel()
				if cancelErr != nil {
					t.Fatal(cancelErr)
				}
			}
			parent := e.s.ParentOrders[p.SourceOfferID]
			if !parent.Quantities.Closed || parent.Quantities.Available != 0 {
				t.Fatal("cancellation did not close available authority")
			}
			if child := e.s.FillRecords[msg.SwapID]; child != nil {
				if ordering == "cancel-first" || len(e.s.Swaps) != 1 || child.Allocation.Disposition != FillReserved || child.Allocation.Quantity != p.SellAmount || parent.Quantities.Reserved != p.SellAmount || parent.Quantities.Released != 0 || len(e.s.CoinReservations) != 1 || !reflect.DeepEqual(child.Inputs, originalInputs) || !reflect.DeepEqual(e.s.CoinReservations["swap/"+msg.SwapID].Inputs, child.Inputs) {
					t.Fatal("closing the parent changed its accepted child or inputs")
				}
			} else if ordering == "accept-first" || len(e.s.Swaps) != 0 || len(e.s.CoinReservations) != 0 || parent.Quantities.Released != p.SellAmount || parent.Quantities.Reserved != 0 {
				t.Fatal("cancelled available quantity became an accepted allocation")
			}
			before := protocol.Digest([]any{e.s.ParentOrders, e.s.FillRecords, e.s.CoinReservations})
			count := len(e.s.Swaps)
			if err := e.handle(from, msg); err != nil {
				t.Fatal(err)
			}
			other, stale := orderRequest(t, e, old)
			if err := e.handle(other, stale); err != nil {
				t.Fatal(err)
			}
			if len(e.s.Swaps) != count || e.s.Swaps[stale.SwapID] != nil || protocol.Digest([]any{e.s.ParentOrders, e.s.FillRecords, e.s.CoinReservations}) != before {
				t.Fatal("retry or stale signed copy changed the retained allocation")
			}
		})
	}
}
