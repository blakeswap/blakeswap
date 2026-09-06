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
		if got, err := protocol.DecodeOffer(event, time.Now().Unix()); err != nil || got.Version != 2 || got.TowerBPS != 0 || got.Tower != nil {
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
			if ms.Terms.Version != 2 || ts.Terms == nil || protocol.Digest(ms.Terms) != protocol.Digest(ts.Terms) {
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

func TestLegacyOfferMigrationKeepsAcceptedTermsAndRemovesRetryLeaks(t *testing.T) {
	maker, _, _ := sendFixture(t)
	provider := discoveryEngine(t)
	maker.Config.Tower = provider.ownTower()
	o := privacyOffer(t, maker, 50)
	o.Version = 0
	o.Tower = nil
	raw, _ := json.Marshal(o)
	legacy := maker.s.Offers[o.ID]
	legacy.Content = string(raw)
	if err := transport.Sign(&legacy, maker.identity); err != nil {
		t.Fatal(err)
	}
	maker.s.Offers[o.ID], maker.s.Book[o.Maker+":"+o.ID] = legacy, legacy
	maker.s.OfferTowers = nil
	maker.queueEvent(legacy)
	// An already accepted v1 swap keeps its signed wire encoding and policy.
	keys, _ := maker.swapKeys(transport.RandomID())
	r := protocol.Request{ID: transport.RandomID(), OfferEvent: legacy, Taker: nostr.Generate().Public().Hex(), Hash: transport.RandomID(), Keys: keys}
	terms, err := protocol.NewTerms(r, keys, map[chain.ID]uint32{chain.BTC: 200, chain.Blake: 200}, maker.Config.Tower.PubKey, maker.Config.Tower.Scripts)
	if err != nil {
		t.Fatal(err)
	}
	maker.s.Swaps[r.ID] = &Swap{ID: r.ID, Role: "maker", Request: r, Terms: &terms}
	before := protocol.Digest(terms)
	if err := maker.migrateOfferPrivacy(); err != nil {
		t.Fatal(err)
	}
	if protocol.Digest(maker.s.Swaps[r.ID].Terms) != before || maker.s.Swaps[r.ID].protection().BPS != 50 {
		t.Fatal("legacy accepted contract changed")
	}
	if maker.Status().Orders[0].TowerBPS != 50 {
		t.Fatal("migration lost local maker policy")
	}
	for _, d := range maker.s.Outbox {
		if d.Event.Kind == transport.OfferKind {
			assertNoProtection(t, d.Event.Content)
		}
	}
	observer, _, _ := sendFixture(t)
	observer.ingestOffer(legacy)
	if observer.Status().Orders[0].Tower != nil || observer.Status().Orders[0].TowerBPS != 0 {
		t.Fatal("legacy API disclosure")
	}
	raw, _ = json.Marshal(map[string]string{"maker": o.Maker, "id": o.ID})
	if _, err := observer.Command(context.Background(), Request{Method: "swap.take", Params: raw}); err == nil {
		t.Fatal("new legacy trade accepted")
	}
	after := maker.s.Offers[o.ID].ID
	if err := maker.migrateOfferPrivacy(); err != nil || maker.s.Offers[o.ID].ID != after {
		t.Fatal("migration not idempotent", err)
	}
}
