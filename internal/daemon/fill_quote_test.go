package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
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

func TestPartialFillTakerQuoteDerivesFrozenPrivateCaps(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, cap := range []int64{0, 20000} {
			for _, bps := range []int64{0, 125} {
				t.Run(fmt.Sprintf("%s/cap%d/bps%d", sell, cap, bps), func(t *testing.T) {
					e, p := partialQuoteFixture(t)
					if sell.Other() == chain.BTC {
						backend := &sendBackend{receiveBackend: e.nodes[chain.BTC].(*receiveBackend)}
						backend.coins = []chain.UTXO{{TxID: transport.RandomID(), Amount: 1000000, Script: hex.EncodeToString(e.scripts[chain.BTC]), Confirmations: 2}}
						e.nodes[chain.BTC] = backend
						if err := e.refresh(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					maker := nostr.Generate()
					o := protocol.Offer{Version: protocol.Version, Revision: 8, Available: p.SellAmount, FillPolicy: p.FillPolicy, Network: chain.Regtest, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: sell, SellAmount: p.SellAmount, BuyAmount: p.BuyAmount, Expires: time.Now().Unix() + 600, Status: "open"}
					raw, err := o.PublicJSON()
					if err != nil {
						t.Fatal(err)
					}
					event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
					if err := transport.Sign(&event, maker); err != nil {
						t.Fatal(err)
					}
					e.s.Book[o.Maker+":"+o.ID] = event
					p.Kind, p.Maker, p.ID, p.Sell = "taker", o.Maker, o.ID, sell
					p.FillOrderFields = FillOrderFields{}
					p.Quantity, p.ParentRevision, p.OwnerFeeCap = 400000, 8, cap
					if bps > 0 {
						provider, _, _ := sendFixture(t)
						provider.Config.RescueFeeBPS = bps
						if err := provider.advertiseTower(); err != nil {
							t.Fatal(err)
						}
						e.ingestTower(provider.s.Towers[provider.identity.Public().Hex()])
						p.TowerBPS, p.TowerPubKey = bps, provider.identity.Public().Hex()
					}
					q := requestQuote(t, e, p)
					paid := sell.Other()
					wantClaim := max(int64(2000), cap)
					wantBounty := protocol.Bounty(520001, bps)
					if len(q.FeeBudgets) != 2 || len(q.BountyBudgets) != 2 || q.FeeBudgets[paid] != 26500 || q.FeeBudgets[sell] != wantClaim || q.BountyBudgets[paid] != wantBounty || q.BountyBudgets[sell] != 0 {
						t.Fatal("ready quote lacks exact role-specific one-child caps")
					}
					if _, explicit := q.BountyBudgets[sell]; !explicit {
						t.Fatal("uncovered incoming leg is absent instead of explicit zero")
					}
					q.FeeBudgets[paid], q.BountyBudgets[paid] = 1, 999999
					frozen := e.tradeQuotes[q.Token]
					if frozen.Quote.FeeBudgets[paid] != 26500 || frozen.Quote.BountyBudgets[paid] != wantBounty || len(frozen.Request.FeeBudgets) != 0 {
						t.Fatal("returned view mutated saved quote or injected remote parent caps")
					}
					confirmation := confirmation(q)
					result := confirmQuote(t, e, confirmation)
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					retained := saved.TradeReceipts[result.ID].Snapshot.Quote
					if retained.FeeBudgets[paid] != 26500 || retained.FeeBudgets[sell] != wantClaim || retained.BountyBudgets[paid] != wantBounty || retained.BountyBudgets[sell] != 0 {
						t.Fatal("confirmed receipt lost exact frozen child authorizations")
					}
					if retry := confirmQuote(t, e, confirmation); retry != result {
						t.Fatal("exact confirmation retry changed child authorization")
					}
					p.FeeBudgets = map[chain.ID]int64{chain.BTC: 1, chain.Blake: 1}
					if _, err := e.tradeSnapshot(p, time.Now().Unix()); err == nil {
						t.Fatal("taker supplied private parent budgets")
					}
				})
			}
		}
	}
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
