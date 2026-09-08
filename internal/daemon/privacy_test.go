package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func privacyOffer(t *testing.T, maker *Engine, bps int64) protocol.Offer {
	t.Helper()
	raw, _ := json.Marshal(walletWholeParams(chain.Blake, 500000, 600000, 2000, 0, bps))
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
		public, err := o.PublicJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(public) != event.Content {
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
		// Exercise the notification serializer directly for each terminal/reserved
		// projection; publishing a parent no longer accepts an arbitrary Status edit.
		for _, status := range []string{"reserved", "cancelled", "filled"} {
			o.Status, o.Available, o.TowerBPS = status, 0, bps
			notification, err := maker.signOffer(o, nostr.Now())
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := protocol.DecodeOffer(notification, time.Now().Unix())
			if err != nil || decoded.Status != status {
				t.Fatal("invalid notification projection", status, err)
			}
			maker.queueEvent(notification)
			assertNoProtection(t, notification.Content)
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
			taker.nodes[chain.BTC] = &sendBackend{receiveBackend: btc}
			raw, _ := json.Marshal(map[string]any{"maker": o.Maker, "id": o.ID, "quantity": o.SellAmount, "parent_revision": o.Revision, "tower_bps": takerBPS, "funding_fee": 2000, "owner_fee_cap": 0})
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

func TestRetiredOfferCacheIsRefusedWithoutProviderConfig(t *testing.T) {
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
	maker.s.OfferTowers = map[string]protocol.Tower{}
	before := protocol.Digest(maker.s)
	if err := maker.scrubOfferCache(); err == nil || !strings.Contains(err.Error(), "incompatible saved offer") {
		t.Fatal("retired owned schema was not refused", err)
	}
	if protocol.Digest(maker.s) != before {
		t.Fatal("refusal rewrote the owned offer, parent, outbox or private commitments")
	}
	observer, _, _ := sendFixture(t)
	observer.ingestOffer(old)
	if len(observer.Status().Orders) != 0 {
		t.Fatal("retired public offer accepted")
	}
	if err := maker.scrubOfferCache(); err == nil || protocol.Digest(maker.s) != before {
		t.Fatal("repeated refusal changed the preserved source", err)
	}
}

func TestTakerProtectionRequiresOnlyItsRefundToMeetEconomicMinimum(t *testing.T) {
	e, _, _ := sendFixture(t)
	e.Config.Tower = discoveryEngine(t).ownTower()
	o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 100000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 100000, Max: 100000}, ID: transport.RandomID(), Maker: discoveryEngine(t).identity.Public().Hex(), Network: chain.Regtest, Sell: chain.Blake, SellAmount: 100000, BuyAmount: 1000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	if tower, err := e.selectProtection(o, 50, "", false); err != nil || tower.BPS != 50 {
		t.Fatal("valid taker refund rejected because of peer leg", err)
	}
	if _, err := e.selectProtection(o, 50, "", true); err == nil {
		t.Fatal("maker's dust rescue accepted")
	}
	o.SellAmount, o.BuyAmount = o.BuyAmount, o.SellAmount
	o.Available, o.FillPolicy.Min, o.FillPolicy.Max = o.SellAmount, o.SellAmount, o.SellAmount
	if _, err := e.selectProtection(o, 50, "", false); err == nil {
		t.Fatal("taker's dust refund accepted")
	}
	if _, err := e.selectProtection(o, -1, "", false); err == nil {
		t.Fatal("negative protection fee accepted")
	}
	o.SellAmount, o.BuyAmount = 1000000, 1000000
	o.Available, o.FillPolicy.Min, o.FillPolicy.Max = o.SellAmount, o.SellAmount, o.SellAmount
	e.Config.Tower.Scripts[chain.BTC] = "0014"
	if _, err := e.selectProtection(o, 50, "", true); err == nil {
		t.Fatal("invalid payout accepted")
	}
}
