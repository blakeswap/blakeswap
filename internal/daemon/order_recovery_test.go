package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func quarantinedManagedSource(t *testing.T, status string) (*Engine, TradeQuoteRequest) {
	t.Helper()
	e, p, _ := managedSource(t)
	old := e.s.OrderRecords[p.SourceOfferID].Offer
	old.Status = status
	if status == "expired" {
		old.Status = "open"
		old.Expires = time.Now().Unix() - 10
	}
	if err := e.publishOffer(old); err != nil {
		t.Fatal(err)
	}
	p.OrderActionFields = OrderActionFields{OrderAction: "recreate", SourceOfferID: old.ID, SourceEventID: e.s.Offers[old.ID].ID.Hex()}
	p.Expires = time.Now().Unix() + 600
	// Model a pre-T07 backup without its new durable order records or a
	// confirmation receipt for the source created by the legacy offer API.
	delete(e.s.TradeReceipts, old.ID)
	e.s.OrderRecords = nil
	e.s.Activities = nil
	e.s.ActivityVersion = 1
	markRestored(t, e)
	return e, p
}

func readyManagedRecovery(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
}

func TestOrderRecoveryHistoricalRecreationUsesNewAuthority(t *testing.T) {
	for _, status := range []string{"expired", "cancelled", "filled"} {
		t.Run(status, func(t *testing.T) {
			e, p := quarantinedManagedSource(t, status)
			original := e.s.Recovery.Offers[p.SourceOfferID]
			quarantined, _ := json.Marshal(e.s.Recovery.Outbox)
			page := queryMarket(t, e, MarketQuery{Owner: "mine", Status: "all"})
			if len(page.Records) != 1 || page.Records[0].CanRecreate || page.Records[0].CanCancel || page.Records[0].CanReplace || page.Records[0].Publication != "unknown" || page.Records[0].CreatedAt != 0 {
				t.Fatal("quarantined history backfill/hold", page)
			}
			if e.s.Activities[activityID("order", p.SourceOfferID)].CreatedAt != 0 {
				t.Fatal("restore fabricated an original order creation time")
			}
			raw, _ := json.Marshal(p)
			if _, err := e.quoteTrade(context.Background(), raw); err == nil {
				t.Fatal("recovering wallet permitted a new order review")
			}
			readyManagedRecovery(t, e)
			page = queryMarket(t, e, MarketQuery{Owner: "mine", Status: "all"})
			if len(page.Records) != 1 || !page.Records[0].CanRecreate || page.Records[0].CanReplace || page.Records[0].CanCancel {
				t.Fatal("ready historical recreation unavailable", page)
			}
			replace := p
			replace.OrderAction = "replace"
			if _, err := e.orderSource(replace.OrderActionFields, time.Now().Unix()); err == nil {
				t.Fatal("quarantine became replacement authority")
			}
			cancel, _ := json.Marshal(map[string]string{"id": p.SourceOfferID, "expected_event_id": p.SourceEventID, "expected_wallet": e.Config.Name})
			if _, err := e.cancelOffer(cancel); err == nil {
				t.Fatal("quarantine became cancellation authority")
			}
			q := requestQuote(t, e, p)
			request := confirmation(q)
			result := confirmQuote(t, e, request)
			if result.State != "accepted" || result.ID == p.SourceOfferID || len(e.s.Offers) != 1 || e.s.Offers[p.SourceOfferID].ID != (nostr.ID{}) || e.s.OrderRecords[result.ID].RecreatedFrom != p.SourceOfferID {
				t.Fatal("recreation failed to create independent authority", result)
			}
			if got := e.s.OrderRecords[result.ID].Offer; got.Expires != p.Expires || got.SellAmount != p.SellAmount || got.BuyAmount != p.BuyAmount {
				t.Fatal("fresh reviewed terms lost", got)
			}
			if e.s.Recovery.Offers[p.SourceOfferID].ID != original.ID || len(e.s.CoinReservations) != 1 {
				t.Fatal("old history changed or reservation duplicated")
			}
			after, _ := json.Marshal(e.s.Recovery.Outbox)
			if !bytes.Equal(quarantined, after) {
				t.Fatal("old queued publications changed")
			}
			for _, d := range e.s.Outbox {
				if d.Event.Kind == transport.OfferKind && transport.Tag(d.Event, "d") == p.SourceOfferID {
					t.Fatal("old offer reentered publication")
				}
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			e.tradeQuotes = nil
			e.tradeConfirming = nil
			if again := confirmQuote(t, e, request); again != result || len(e.s.Offers) != 1 {
				t.Fatal("recreation retry changed identity", again)
			}
		})
	}
}

func TestOrderRecoveryRejectsHistoricalIdentityReuseAndUnknownObligations(t *testing.T) {
	e, p := quarantinedManagedSource(t, "expired")
	readyManagedRecovery(t, e)
	request := confirmation(requestQuote(t, e, p))
	request.RequestID = p.SourceOfferID
	result := confirmQuote(t, e, request)
	if result.State != "rejected" || len(e.s.Offers) != 0 {
		t.Fatal("recreation reused its quarantined historical ID", result)
	}
	e.s.Recovery.Swaps[transport.RandomID()] = true
	e.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil)
	raw, _ := json.Marshal(p)
	if _, err := e.quoteTrade(context.Background(), raw); err == nil {
		t.Fatal("missing restored obligation allowed recreation")
	}
	page := queryMarket(t, e, MarketQuery{Owner: "mine", Status: "all"})
	if len(page.Records) != 1 || page.Records[0].CanRecreate {
		t.Fatal("unknown obligation advertised recreation", page)
	}
}

func TestOrderRecoveryRecreationRevalidatesSourceFundsAndCheckpoint(t *testing.T) {
	for _, fault := range []string{"event", "signature", "funds", "checkpoint", "unexpired"} {
		t.Run(fault, func(t *testing.T) {
			status := "expired"
			if fault == "unexpired" {
				status = "open"
			}
			e, p := quarantinedManagedSource(t, status)
			readyManagedRecovery(t, e)
			switch fault {
			case "event":
				p.SourceEventID = transport.RandomID()
			case "signature":
				ev := e.s.Recovery.Offers[p.SourceOfferID]
				ev.Content += " "
				e.s.Recovery.Offers[p.SourceOfferID] = ev
			case "funds":
				p.SellAmount = 2_000_000
			case "checkpoint":
				point := e.recoveryCheckpoints[chain.BTC]
				point.Hash = "different-tip"
				e.recoveryCheckpoints[chain.BTC] = point
			}
			before, _ := json.Marshal(e.s)
			raw, _ := json.Marshal(p)
			q, err := e.quoteTrade(context.Background(), raw)
			if err == nil && q.Ready {
				t.Fatal("invalid recovered recreation authorized", q)
			}
			after, _ := json.Marshal(e.s)
			if !bytes.Equal(before, after) {
				t.Fatal("failed recreation modified history or obligations")
			}
		})
	}
}
