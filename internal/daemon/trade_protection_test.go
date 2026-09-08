package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestTakerQuoteConfirmationProtectsOnlyPaidRefund(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, partial := range []bool{false, true} {
			for _, validRefund := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/partial=%v/valid-refund=%v", sell, partial, validRefund), func(t *testing.T) {
					e, _ := tradeFixture(t, "taker")
					for _, id := range []chain.ID{chain.BTC, chain.Blake} {
						backend, ok := e.nodes[id].(*sendBackend)
						if !ok {
							backend = &sendBackend{receiveBackend: e.nodes[id].(*receiveBackend)}
						}
						backend.coins = []chain.UTXO{{TxID: protocol.Digest("taker protection coin/" + string(id)), Amount: 3000000, Confirmations: 2, Script: hex.EncodeToString(e.scripts[id])}}
						e.nodes[id] = backend
					}
					if err := e.refresh(context.Background()); err != nil {
						t.Fatal(err)
					}
					received, paid := int64(100000), int64(1000000)
					if !validRefund {
						received, paid = paid, received
					}
					maker := nostr.Generate()
					offer := protocol.Offer{Version: protocol.Version, Revision: 1, Network: chain.Regtest, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: sell, SellAmount: received, BuyAmount: paid, Available: received, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: received, Max: received}, Expires: time.Now().Unix() + 3600, Status: "open"}
					if partial {
						offer.SellAmount *= 2
						offer.BuyAmount *= 2
						offer.Available *= 2
						offer.FillPolicy.Mode = protocol.FillPartial
					}
					content, err := offer.PublicJSON()
					if err != nil {
						t.Fatal(err)
					}
					event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(content)}
					if err := transport.Sign(&event, maker); err != nil {
						t.Fatal(err)
					}
					e.s.Book = map[string]nostr.Event{offer.Maker + ":" + offer.ID: event}
					provider := discoveryEngine(t)
					provider.Config.RescueFeeBPS = 50
					if err := provider.advertiseTower(); err != nil {
						t.Fatal(err)
					}
					pubkey := provider.identity.Public().Hex()
					e.ingestTower(provider.s.Towers[pubkey])
					p := TradeQuoteRequest{Kind: "taker", ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Maker: offer.Maker, ID: offer.ID, TowerBPS: 50, TowerPubKey: pubkey, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}, FillTakeFields: FillTakeFields{Quantity: received, ParentRevision: offer.Revision}}
					raw, _ := json.Marshal(p)
					q, err := e.quoteTrade(context.Background(), raw)
					if !validRefund {
						if err == nil && q.Ready {
							t.Fatal("quote admitted the taker's dust refund bounty")
						}
						message := q.Error
						if err != nil {
							message = err.Error()
						}
						if !strings.Contains(message, "tower economic minimum") {
							t.Fatal("invalid paid refund reached a different refusal", message)
						}
						if len(e.s.Swaps) != 0 || len(e.s.TradeReceipts) != 0 {
							t.Fatal("invalid refund acquired trade authority")
						}
						return
					}
					if err != nil || !q.Ready || q.Error != "" || q.PaidPrincipal != paid || q.ReceivedPrincipal != received || q.Provider.PubKey != pubkey || q.BountyBudgets[sell.Other()] != 5000 || q.BountyBudgets[sell] != 0 {
						t.Fatal("valid own refund was not quoted", q, err)
					}
					request := confirmation(q)
					result := confirmQuote(t, e, request)
					if result.State != "accepted" || result.ID != request.RequestID || result.Error != "" {
						t.Fatal("ready taker quote rejected for peer's unprotected small leg", result)
					}
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					swap := saved.Swaps[result.ID]
					if swap == nil || swap.Request.Quantity != received || swap.Request.OfferEvent.ID != event.ID || swap.protection().PubKey != pubkey || swap.protection().BPS != 50 || saved.TradeReceipts[result.ID].Result != result {
						t.Fatal("confirmation lost exact reviewed child/provider/receipt")
					}
					if retry := confirmQuote(t, e, request); retry != result || len(e.s.Swaps) != 1 {
						t.Fatal("exact retry changed accepted authority", retry)
					}
				})
			}
		}
	}
}
