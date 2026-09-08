package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func quarantinedManagedSource(t *testing.T, status string) (*Engine, TradeQuoteRequest) {
	t.Helper()
	if status == "filled" {
		return quarantinedFilledManagedSource(t)
	}
	expires := int64(0)
	if status == "expired" {
		expires = time.Now().Unix() + 2
	}
	e, p, _ := managedSourceExpiry(t, expires)
	old := e.s.OrderRecords[p.SourceOfferID].Offer
	if status == "cancelled" {
		params, err := json.Marshal(map[string]string{"id": old.ID, "expected_event_id": e.s.Offers[old.ID].ID.Hex()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.cancelOffer(params); err != nil {
			t.Fatal(err)
		}
	}
	if status == "expired" || status == "cancelled" {
		until := old.Expires
		if status == "cancelled" {
			until = e.s.ParentOrders[old.ID].LastSignedAt + 1
		}
		wait := time.Until(time.Unix(until, 0))
		if wait > 5*time.Second {
			t.Fatal("unexpected fixture publication/expiry deadline")
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := e.publishPendingParents(); err != nil {
			t.Fatal(err)
		}
	}
	p.OrderActionFields = OrderActionFields{OrderAction: "recreate", SourceOfferID: old.ID, SourceEventID: e.s.Offers[old.ID].ID.Hex()}
	p.Expires = time.Now().Unix() + 600
	// Current protocol recovery may lack advisory order/activity metadata.
	// Retain the exact signed source, parent allocation and original input hold.
	delete(e.s.TradeReceipts, old.ID)
	e.s.OrderRecords = nil
	e.s.Activities = nil
	e.s.ActivityVersion = 1
	markRestored(t, e)
	return e, p
}

type managedRecoveryBackend struct{ *historySettlementBackend }

func (b *managedRecoveryBackend) Output(ctx context.Context, id string, vout uint32) (*chain.TxOut, error) {
	return (&sendBackend{receiveBackend: b.receiveBackend}).Output(ctx, id, vout)
}

// A completed source retains the original signed request and both valid
// outcomes through import. All observations are disposable private backends.
func quarantinedFilledManagedSource(t *testing.T) (*Engine, TradeQuoteRequest) {
	t.Helper()
	e, swap, _, secret := isolatedFixtureSell(t, "maker", chain.Blake)
	e.Config.Name = "alice"
	offer := swap.Terms.Offer()
	parent := e.s.ParentOrders[offer.ID]
	parent.SignedRevision, parent.LastSignedAt = offer.Revision, int64(swap.Request.OfferEvent.CreatedAt)
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	backends := map[chain.ID]*historySettlementBackend{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		base := &receiveBackend{used: map[string]bool{}, coins: []chain.UTXO{{TxID: transport.RandomID(), Amount: 4000000, Script: hex.EncodeToString(e.scripts[id]), Confirmations: 200}}}
		b := &historySettlementBackend{activityArchiveBackend: &activityArchiveBackend{receiveBackend: base, hashes: map[uint32]string{1: "test-canonical-tip", 200: "test-canonical-tip"}}, transactions: map[string]chain.Transaction{}}
		e.nodes[id], e.watch[id], backends[id] = &managedRecoveryBackend{b}, b, b
		e.chainFresh[id] = true
		e.heights[id], e.clocks[id] = 200, 200
		e.chainObserved[id] = time.Now().Unix()
	}
	for _, c := range []contract.HTLC{swap.Long, swap.Short} {
		obs := recoverySpend(t, e, swap, c, false, secret)
		obs.Height, obs.Confirmations = 1, 200
		all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = obs
		raw := swap.ShortFunding
		if c.Chain == swap.Long.Chain {
			raw = swap.LongFunding
			swap.SelfClaim = contract.Hex(obs.Tx)
		}
		backends[c.Chain].transactions[c.TxID] = chain.Transaction{TxID: c.TxID, Hex: raw, Height: 1, Confirmations: 200, BlockHash: "test-canonical-tip"}
		backends[c.Chain].transactions[obs.TxID] = chain.Transaction{TxID: obs.TxID, Hex: contract.Hex(obs.Tx), Height: 1, Confirmations: 200, BlockHash: "test-canonical-tip"}
	}
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &callbackRecoveryScanner{observations: all[chain.BTC]}, chain.Blake: &callbackRecoveryScanner{observations: all[chain.Blake]}}
	if err := e.advanceSwap(context.Background(), swap, all); err != nil {
		t.Fatal(err)
	}
	if swap.Stage != "completed" || parent.Quantities.Filled != offer.SellAmount || parent.Quantities.Committed != 0 {
		t.Fatal("completed source lacks positive child accounting")
	}
	if wait := time.Until(time.Unix(parent.LastSignedAt+1, 0)); wait > 0 {
		if wait > 2*time.Second {
			t.Fatal("unexpected fixture publication deadline")
		}
		time.Sleep(wait)
	}
	if err := e.publishParent(offer.ID); err != nil {
		t.Fatal(err)
	}
	p := TradeQuoteRequest{Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Sell: offer.Sell, SellAmount: offer.SellAmount, BuyAmount: 3000000, Expires: time.Now().Unix() + 600, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}, FillOrderFields: automationWholeFields(offer.Sell, offer.SellAmount, 3000000, 2000, 0), OrderActionFields: OrderActionFields{OrderAction: "recreate", SourceOfferID: offer.ID, SourceEventID: e.s.Offers[offer.ID].ID.Hex()}}
	e.s.OrderRecords, e.s.Activities = nil, nil
	e.s.ActivityVersion = 1
	markRestored(t, e)
	return e, p
}

func readyManagedRecovery(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	if len(e.s.Swaps) > 0 {
		var err error
		all, err = e.scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, swap := range e.s.Swaps {
			if err := e.advanceSwap(context.Background(), swap, all); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.reconcileRecovery(all, nil)
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
			if fault != "unexpired" {
				requestQuote(t, e, p) // Each fault starts from an otherwise executable review.
			}
			switch fault {
			case "event":
				p.SourceEventID = transport.RandomID()
			case "signature":
				ev := e.s.Recovery.Offers[p.SourceOfferID]
				ev.Content += " "
				e.s.Recovery.Offers[p.SourceOfferID] = ev
			case "funds":
				e.walletCoins[p.Sell] = map[string][]chain.UTXO{}
				e.balances[p.Sell] = 0
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
