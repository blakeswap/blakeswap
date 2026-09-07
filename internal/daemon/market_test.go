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
	o := protocol.Offer{ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: sell, SellAmount: btc, BuyAmount: blake, Status: status, Expires: expires}
	if sell == chain.Blake {
		o.SellAmount, o.BuyAmount = blake, btc
	}
	if own {
		if err := e.publishOffer(o); err != nil {
			t.Fatal(err)
		}
	} else {
		content, _ := o.PublicJSON()
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
	if p.Records[0].Offer.ID != low.ID || p.Records[1].Offer.ID != high.ID || p.Records[0].Rate != p.Records[1].Rate {
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
	e.s.Swaps["pending"] = &Swap{ID: "pending", Role: "taker", Stage: "request queued", Request: protocol.Request{OfferEvent: e.s.Book[remote.Maker+":"+remote.ID]}}
	p = queryMarket(t, e, MarketQuery{Owner: "others", Status: "pending"})
	if len(p.Records) != 1 || p.Records[0].CanTake || !reflect.DeepEqual(p.Records[0].SwapIDs, []string{"pending"}) {
		t.Fatal(p)
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
	old := marketOffer(t, e, true, chain.Blake, 100_000, 200_000, "reserved", now-1)
	e.s.Swaps["refund"] = &Swap{ID: "refund", Role: "maker", Stage: "refunded", Request: protocol.Request{OfferEvent: e.s.Offers[old.ID]}}
	p = queryMarket(t, e, MarketQuery{Owner: "mine", Status: "refunded"})
	if p.Total != 1 || !p.Records[0].CanRecreate || p.Records[0].CanReplace || p.Records[0].CanCancel {
		t.Fatal(p)
	}
	fields := OrderActionFields{OrderAction: "recreate", SourceOfferID: old.ID, SourceEventID: e.s.Offers[old.ID].ID.Hex()}
	if _, err := e.orderSource(fields, now); err != nil {
		t.Fatal(err)
	}
	e.s.Swaps["refund"].Stage = "refunding"
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
	e, p, old := managedSource(t)
	from, msg := orderRequest(t, e, old)
	raw, _ := json.Marshal(map[string]string{"id": p.SourceOfferID, "expected_wallet": e.Config.Name, "expected_event_id": old.ID.Hex()})
	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	var cancelErr, acceptErr error
	go func() { defer wg.Done(); <-start; e.mu.Lock(); defer e.mu.Unlock(); _, cancelErr = e.cancelOffer(raw) }()
	go func() { defer wg.Done(); <-start; e.mu.Lock(); defer e.mu.Unlock(); acceptErr = e.handle(from, msg) }()
	close(start)
	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	if cancelErr == nil {
		if len(e.s.Swaps) != 0 || len(e.s.CoinReservations) != 0 {
			t.Fatal("cancel did not exclude acceptance")
		}
	} else if !strings.Contains(cancelErr.Error(), "changed") && !strings.Contains(cancelErr.Error(), "unreserved") {
		t.Fatal(cancelErr)
	} else if len(e.s.Swaps) != 1 || len(e.s.CoinReservations) != 1 {
		t.Fatal("acceptance lost reservation")
	}
	count := len(e.s.Swaps)
	if err := e.handle(from, msg); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Swaps) != count {
		t.Fatal("duplicate request changed outcome")
	}
}
