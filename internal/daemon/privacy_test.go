package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func privacyOffer(t *testing.T, maker *Engine, bps int64) protocol.Offer {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"sell": "blake", "sell_amount": 500000, "buy_amount": 600000, "tower_bps": bps})
	result, err := maker.Command(context.Background(), Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return result.(protocol.Offer)
}
func assertNoProtection(t *testing.T, raw string) {
	t.Helper()
	for _, field := range []string{`"tower"`, `"tower_bps"`, `"tower_scripts"`, `"protection"`, `"quote"`, `"npub"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("private field %s in public/peer payload", field)
		}
	}
}
func TestOfferProtectionStaysLocalAcrossPublicationAndRestart(t *testing.T) {
	for _, bps := range []int64{0, 50} {
		maker, _, _ := sendFixture(t)
		provider := discoveryEngine(t)
		maker.Config.Tower = provider.ownTower()
		o := privacyOffer(t, maker, bps)
		event := maker.s.Offers[o.ID]
		assertNoProtection(t, event.Content)
		if got, err := protocol.DecodeOffer(event, time.Now().Unix()); err != nil || got.TowerBPS != 0 || got.Tower != nil {
			t.Fatal("invalid private offer", err)
		}
		if got := maker.Status().Orders[0]; got.TowerBPS != bps {
			t.Fatal("maker lost its private protection")
		}
		observer, _, _ := sendFixture(t)
		observer.ingestOffer(event)
		if got := observer.Status().Orders[0]; got.TowerBPS != 0 || got.Tower != nil {
			t.Fatal("observer learned maker protection")
		}
		// The same public offer has identical content with protection toggled.
		o.Tower, o.TowerBPS = nil, 0
		if err := maker.publishOffer(o); err != nil {
			t.Fatal(err)
		}
		if maker.s.Offers[o.ID].Content != event.Content {
			t.Fatal("public offer varies with protection")
		}
		if err := maker.save(); err != nil {
			t.Fatal(err)
		}
		maker.s = State{}
		if _, err := maker.vault.Load(&maker.s); err != nil {
			t.Fatal(err)
		}
		maker.Config.Tower = protocol.Tower{}
		if got := maker.Status().Orders[0]; got.TowerBPS != bps {
			t.Fatal("restart lost pinned protection")
		}
		for _, status := range []string{"reserved", "cancelled", "filled"} {
			o.Status = status
			if err := maker.publishOffer(o); err != nil {
				t.Fatal(err)
			}
			assertNoProtection(t, maker.s.Offers[o.ID].Content)
			for _, d := range maker.s.Outbox {
				if d.Event.Kind == transport.OfferKind {
					assertNoProtection(t, d.Event.Content)
				}
			}
		}
	}
}

func TestPrivateProtectionDoesNotReachCounterparty(t *testing.T) {
	for _, makerBPS := range []int64{0, 50} {
		for _, takerBPS := range []int64{0, 50} {
			maker, _, _ := sendFixture(t)
			taker, _, _ := sendFixture(t)
			provider, otherProvider := discoveryEngine(t), discoveryEngine(t)
			maker.Config.Tower, taker.Config.Tower = provider.ownTower(), otherProvider.ownTower()
			o := privacyOffer(t, maker, makerBPS)
			taker.ingestOffer(maker.s.Offers[o.ID])
			btc := taker.nodes[chain.BTC].(*receiveBackend)
			btc.coins = []chain.UTXO{{TxID: strings.Repeat("34", 32), Amount: 1000000, Script: hex.EncodeToString(taker.scripts[chain.BTC]), Confirmations: 2}}
			raw, _ := json.Marshal(map[string]any{"maker": o.Maker, "id": o.ID, "tower_bps": takerBPS})
			result, err := taker.Command(context.Background(), Request{Method: "swap.take", Params: raw})
			if err != nil {
				t.Fatal(err)
			}
			id := result.(map[string]string)["id"]
			for _, d := range taker.s.Outbox {
				if d.Type == "request" {
					_, m, err := transport.UnwrapFor(chain.Regtest.Namespace(), maker.identity, d.Event)
					if err != nil {
						t.Fatal(err)
					}
					assertNoProtection(t, string(m.Body))
					if err := maker.receive(d.Event); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, d := range maker.s.Outbox {
				if d.Type == "accepted" {
					_, m, err := transport.UnwrapFor(chain.Regtest.Namespace(), taker.identity, d.Event)
					if err != nil {
						t.Fatal(err)
					}
					assertNoProtection(t, string(m.Body))
					if err := taker.receive(d.Event); err != nil {
						t.Fatal(err)
					}
				}
			}
			ms, ts := maker.s.Swaps[id], taker.s.Swaps[id]
			if ts.Terms == nil || protocol.Digest(ms.Terms) != protocol.Digest(ts.Terms) {
				t.Fatal("parties did not agree to private terms")
			}
			if ms.protection().BPS != makerBPS || ts.protection().BPS != takerBPS {
				t.Fatal("counterparty selected our protection")
			}
			for _, party := range []*Engine{maker, taker} {
				before := protocol.Digest(party.s.Swaps[id].protection())
				if err := party.save(); err != nil {
					t.Fatal(err)
				}
				party.s = State{}
				if _, err := party.vault.Load(&party.s); err != nil {
					t.Fatal(err)
				}
				party.Config.Tower = protocol.Tower{}
				if protocol.Digest(party.s.Swaps[id].protection()) != before {
					t.Fatal("swap protection not durable")
				}
			}
			// Jobs travel only to the selected provider; the peer cannot decrypt them.
			ms = maker.s.Swaps[id]
			ms.Long.TxID, ms.Short.TxID = transport.RandomID(), transport.RandomID()
			if err := maker.prepare(ms, ms.Short); err != nil {
				t.Fatal(err)
			}
			if makerBPS > 0 && (len(ms.Jobs) != 2 || towerReady(ms)) {
				t.Fatal("protected maker must await both receipts")
			}
			for _, d := range maker.s.Outbox {
				if d.Type == "tower-job" {
					if _, _, err := transport.UnwrapFor(chain.Regtest.Namespace(), taker.identity, d.Event); err == nil {
						t.Fatal("peer decrypted maker job")
					}
					_, m, err := transport.UnwrapFor(chain.Regtest.Namespace(), provider.identity, d.Event)
					if err != nil {
						t.Fatal(err)
					}
					var job protocol.Job
					if err := json.Unmarshal(m.Body, &job); err != nil {
						t.Fatal(err)
					}
					if err := job.Validate(provider.ownTower().Scripts, makerBPS); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestRetiredOfferCacheIsWithdrawnWithoutProviderConfig(t *testing.T) {
	maker, _, _ := sendFixture(t)
	maker.Config.Tower = discoveryEngine(t).ownTower()
	o := privacyOffer(t, maker, 50)
	o.Tower = nil
	raw, _ := json.Marshal(o) // Retired schema published tower_bps.
	old := maker.s.Offers[o.ID]
	old.Content = string(raw)
	if err := transport.Sign(&old, maker.identity); err != nil {
		t.Fatal(err)
	}
	maker.s.Offers[o.ID], maker.s.Book[o.Maker+":"+o.ID] = old, old
	maker.queueEvent(old)
	maker.Config.Tower = protocol.Tower{}
	maker.s.OfferTowers = nil
	if err := maker.scrubOfferCache(); err != nil {
		t.Fatal("cache cleanup blocked wallet startup", err)
	}
	current, err := protocol.DecodeOffer(maker.s.Offers[o.ID], time.Now().Unix())
	if err != nil || current.Status != "cancelled" {
		t.Fatal("retired offer not withdrawn", err)
	}
	for _, d := range maker.s.Outbox {
		if d.Event.Kind == transport.OfferKind {
			assertNoProtection(t, d.Event.Content)
		}
	}
	observer, _, _ := sendFixture(t)
	observer.ingestOffer(old)
	if len(observer.Status().Orders) != 0 {
		t.Fatal("retired public offer accepted")
	}
	before := maker.s.Offers[o.ID].ID
	if err := maker.scrubOfferCache(); err != nil || maker.s.Offers[o.ID].ID != before {
		t.Fatal("cache cleanup is not idempotent", err)
	}
}

func TestTakerProtectionRequiresOnlyItsRefundToMeetEconomicMinimum(t *testing.T) {
	e, _, _ := sendFixture(t)
	e.Config.Tower = discoveryEngine(t).ownTower()
	o := protocol.Offer{ID: transport.RandomID(), Maker: discoveryEngine(t).identity.Public().Hex(), Network: chain.Regtest, Sell: chain.Blake, SellAmount: 100000, BuyAmount: 1000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	if tower, err := e.selectProtection(o, 50, "", false); err != nil || tower.BPS != 50 {
		t.Fatal("valid taker refund rejected because of peer leg", err)
	}
	if _, err := e.selectProtection(o, 50, "", true); err == nil {
		t.Fatal("maker's dust rescue accepted")
	}
	o.SellAmount, o.BuyAmount = o.BuyAmount, o.SellAmount
	if _, err := e.selectProtection(o, 50, "", false); err == nil {
		t.Fatal("taker's dust refund accepted")
	}
	if _, err := e.selectProtection(o, -1, "", false); err == nil {
		t.Fatal("negative protection fee accepted")
	}
	o.SellAmount, o.BuyAmount = 1000000, 1000000
	e.Config.Tower.Scripts[chain.BTC] = "0014"
	if _, err := e.selectProtection(o, 50, "", true); err == nil {
		t.Fatal("invalid payout accepted")
	}
}
