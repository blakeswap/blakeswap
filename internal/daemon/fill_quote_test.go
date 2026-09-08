package daemon

import (
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func partialQuoteFixture(t *testing.T) (*Engine, TradeQuoteRequest) {
	t.Helper()
	e, _, _ := sendFixture(t)
	e.s.Version, e.s.Network = StateVersion, chain.Regtest
	e.Config.Name = "partial-review"
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := TradeQuoteRequest{Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Sell: chain.Blake, SellAmount: 900000, BuyAmount: 1170001, FeeSelection: FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000}, FillOrderFields: FillOrderFields{FillPolicy: protocol.FillPolicy{Mode: protocol.FillPartial, Min: 400000, Max: 600000}, FeeBudgets: map[chain.ID]int64{chain.BTC: 100000, chain.Blake: 100000}, BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}}}
	return e, p
}

func TestPartialFillQuoteSeparatesParentReserveAndRepresentativeChild(t *testing.T) {
	e, p := partialQuoteFixture(t)
	q := requestQuote(t, e, p)
	if q.PaidPrincipal != 900000 || q.PaidTotal != 913000 || q.FundingReserve != 13000 || q.ReceivedPrincipal != 1170001 || q.Available != 900000 || q.TotalSellAmount != 900000 || len(q.Outcomes) != 0 {
		t.Fatal("parent quote misrepresented aggregate economics")
	}
	if q.ExampleFill == nil || q.ExampleFill.Quantity != 400000 || q.ExampleFill.BuyAmount != 520001 || q.ExampleFill.FundingFee != 6500 || len(q.ExampleFill.Outcomes) != 2 {
		t.Fatal("separate legal example missing")
	}
	if len(e.s.ParentOrders) != 0 || len(e.s.CoinReservations) != 0 {
		t.Fatal("quote granted parent authority")
	}
	result := confirmQuote(t, e, confirmation(q))
	parent := e.s.ParentOrders[result.ID]
	if parent == nil || parent.Offer.Mode != protocol.FillPartial || parent.Quantities.Available != 900000 || parent.Fees[chain.Blake].Limit != 100000 {
		t.Fatal("confirmed exact parent authorization missing")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.ParentOrders[result.ID] == nil || saved.TradeReceipts[result.ID].Snapshot.Quote.PaidTotal != 913000 || len(saved.CoinReservations["offer/"+result.ID].Inputs) != 1 {
		t.Fatal("review, parent and aggregate input reserve were not saved together")
	}
}

func TestPartialFillQuoteTakerBindsExactQuantityRoundingAndRevision(t *testing.T) {
	e, p := partialQuoteFixture(t)
	maker := nostr.Generate()
	o := protocol.Offer{Version: protocol.Version, Revision: 8, Available: p.SellAmount, FillPolicy: p.FillPolicy, Network: chain.Regtest, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: p.SellAmount, BuyAmount: p.BuyAmount, Expires: time.Now().Unix() + 600, Status: "open"}
	raw, err := o.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	e.s.Book[o.Maker+":"+o.ID] = event
	p.Kind, p.Maker, p.ID, p.Sell = "taker", o.Maker, o.ID, o.Sell
	p.FillOrderFields = FillOrderFields{}
	p.Quantity, p.ParentRevision = 400000, 8
	q := requestQuote(t, e, p)
	if q.Quantity != 400000 || q.ParentRevision != 8 || q.PaidPrincipal != 520001 || q.ReceivedPrincipal != 400000 || q.PaidTotal != 526501 || q.ExampleFill != nil || q.Available != 900000 || q.TotalSellAmount != 900000 {
		t.Fatal("taker quote substituted parent amounts or changed requested fill")
	}
	accepted := confirmQuote(t, e, confirmation(q))
	child := e.s.Swaps[accepted.ID]
	if child == nil || child.Request.Quantity != 400000 || child.Request.Revision != 8 || e.s.FillKeys["hash/"+child.Request.Hash] != accepted.ID || e.s.FillKeys["key/"+child.Request.Keys[chain.Blake]] != accepted.ID {
		t.Fatal("taker request was published without exact quantity and identity registration")
	}
	for _, bad := range []FillTakeFields{{Quantity: 0, ParentRevision: 8}, {Quantity: 400000, ParentRevision: 7}, {Quantity: 600000, ParentRevision: 8}} {
		changed := p
		changed.FillTakeFields = bad
		if _, err := e.tradeSnapshot(changed, time.Now().Unix()); err == nil {
			t.Fatalf("invalid quantity/revision accepted: %+v", bad)
		}
	}
	o.Revision, o.Available = 9, 500000
	raw, err = o.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event.Content = string(raw)
	event.CreatedAt++
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	e.s.Book[o.Maker+":"+o.ID] = event
	snapshot := e.tradeQuotes[q.Token]
	if err := e.validateTradeSource(snapshot, time.Now().Unix()); err == nil {
		t.Fatal("new availability revision preserved old review authority")
	}
}

func TestPartialFillQuoteFreezesExplicitPrivateBudgetMaps(t *testing.T) {
	e, p := partialQuoteFixture(t)
	s, err := e.tradeSnapshot(p, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	p.FeeBudgets[chain.BTC] = 1
	p.BountyBudgets[chain.Blake] = 999
	if s.Request.FeeBudgets[chain.BTC] != 100000 || s.Quote.FeeBudgets[chain.BTC] != 100000 || s.Request.BountyBudgets[chain.Blake] != 0 {
		t.Fatal("caller changed frozen authorization maps")
	}
	s.Quote.FeeBudgets[chain.BTC] = 2
	if s.Request.FeeBudgets[chain.BTC] != 100000 {
		t.Fatal("display map changed retained request authorization")
	}
	p.FeeBudgets = nil
	if _, err := e.tradeSnapshot(p, time.Now().Unix()); err == nil {
		t.Fatal("missing parent budget became unlimited")
	}
}
