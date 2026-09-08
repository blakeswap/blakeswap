package daemon

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestPartialFillPublicQuoteDerivesParentEconomicsFromExactEvent(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, _ := partialQuoteFixture(t)
			if sell.Other() == chain.BTC {
				backend := &sendBackend{receiveBackend: e.nodes[chain.BTC].(*receiveBackend)}
				backend.coins = []chain.UTXO{{TxID: transport.RandomID(), Amount: 1000000, Script: hex.EncodeToString(e.scripts[chain.BTC]), Confirmations: 6}}
				e.nodes[chain.BTC] = backend
				if err := e.refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			parent, maker := fillParentFixture(t, sell, 0)
			signed := fillRequestFixture(t, parent, maker, 400000)
			e.s.Book[parent.Offer.Maker+":"+parent.Offer.ID] = signed.OfferEvent
			// This is the public API regtest shape, distinct from native review, which
			// also supplies redundant full-parent A/B fields. No private maker caps.
			p := TradeQuoteRequest{Kind: "taker", ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Maker: parent.Offer.Maker, ID: parent.Offer.ID, Sell: sell, FillTakeFields: FillTakeFields{Quantity: 400000, ParentRevision: 1}, FeeSelection: FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000}}
			q := requestQuote(t, e, p)
			if !q.Ready || q.PaidPrincipal != 520001 || q.ReceivedPrincipal != 400000 || q.Available != 1000000 || q.ParentRevision != 1 || q.FeeBudgets[sell.Other()] != 26500 {
				t.Fatal("public take quote did not bind exact derived child")
			}
			c := confirmation(q)
			result := confirmQuote(t, e, c)
			child := e.s.Swaps[result.ID]
			if child == nil || child.Request.Quantity != 400000 || child.Request.Revision != 1 || child.Request.OfferEvent.ID != signed.OfferEvent.ID {
				t.Fatal("confirmation substituted parent or quantity")
			}
			if again := confirmQuote(t, e, c); again != result {
				t.Fatal("exact public confirmation retry changed child")
			}
			// Supplied redundant values remain an exact stale-view check.
			for _, field := range []string{"sell", "A", "B"} {
				changed := p
				switch field {
				case "sell":
					changed.Sell = sell.Other()
				case "A":
					changed.SellAmount = parent.Offer.SellAmount + 1
				case "B":
					changed.BuyAmount = parent.Offer.BuyAmount + 1
				}
				if _, err := e.tradeSnapshot(changed, time.Now().Unix()); err == nil {
					t.Fatal("conflicting redundant parent economics accepted", field)
				}
			}
			p.SellAmount, p.BuyAmount = parent.Offer.SellAmount, parent.Offer.BuyAmount
			if _, err := e.tradeSnapshot(p, time.Now().Unix()); err != nil {
				t.Fatal("native redundant exact parent view refused", err)
			}
			p.Sell = ""
			p.SellAmount, p.BuyAmount = 0, 0
			if s, err := e.tradeSnapshot(p, time.Now().Unix()); err != nil || s.Offer.EconomicsDigest() != parent.Economics {
				t.Fatal("exact signed event could not supply its sell asset", err)
			}
			p.FeeBudgets = map[chain.ID]int64{chain.BTC: 1, chain.Blake: 1}
			if _, err := e.tradeSnapshot(p, time.Now().Unix()); err == nil {
				t.Fatal("remote maker caps became taker authority")
			}
			if q.Quantity != 400000 || q.FillPolicy.Mode != protocol.FillPartial {
				t.Fatal("quote lost explicit partial mode")
			}
		})
	}
}
