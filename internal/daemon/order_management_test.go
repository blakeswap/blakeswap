package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func managedSource(t *testing.T) (*Engine, TradeQuoteRequest, nostr.Event) {
	t.Helper()
	return managedSourceExpiry(t, 0)
}
func managedSourceExpiry(t *testing.T, expires int64) (*Engine, TradeQuoteRequest, nostr.Event) {
	t.Helper()
	e, p := tradeFixture(t, "maker")
	p.Expires = expires
	created := confirmQuote(t, e, confirmation(requestQuote(t, e, p)))
	if created.State != "accepted" {
		t.Fatal(created)
	}
	event := e.s.Offers[created.ID]
	p.OrderActionFields = OrderActionFields{OrderAction: "replace", SourceOfferID: created.ID, SourceEventID: event.ID.Hex()}
	p.BuyAmount = 300000 // Reprice exactly the unassigned remainder; its quantity is unchanged.
	return e, p, event
}

func orderRequest(t *testing.T, e *Engine, event nostr.Event) (string, transport.Message) {
	t.Helper()
	request := automationChildRequest(t, e, event)
	raw, _ := json.Marshal(request)
	return request.Taker, transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "request", SwapID: request.ID, Body: raw}
}

func TestOrderReplacementTransfersReservationAndRetriesAfterRestart(t *testing.T) {
	e, p, old := managedSource(t)
	oldReservation := e.s.CoinReservations["offer/"+p.SourceOfferID]
	before, _ := json.Marshal(e.s)
	q := requestQuote(t, e, p)
	after, _ := json.Marshal(e.s)
	if !bytes.Equal(before, after) || !reflect.DeepEqual(q.Funds.Inputs, oldReservation.Inputs) {
		t.Fatal("replacement quote changed or bypassed the existing reservation")
	}
	// Ordinary preflight still cannot spend these same reserved coins.
	raw, _ := json.Marshal(FundsPreflightRequest{Chain: chain.Blake, Amount: p.SellAmount, Fee: p.FundingFee, Inputs: q.Funds.Inputs})
	ordinary, err := e.preflightFunds(context.Background(), Request{Method: "wallet.preflight", Params: raw})
	if err != nil || ordinary.Sufficient {
		t.Fatal("public preflight borrowed a replacement reservation", ordinary, err)
	}
	request := confirmation(q)
	result := confirmQuote(t, e, request)
	if result.State != "accepted" || result.ID == p.SourceOfferID {
		t.Fatal(result)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Offers) != 2 || len(saved.CoinReservations) != 1 || !reflect.DeepEqual(saved.CoinReservations["offer/"+result.ID], oldReservation) {
		t.Fatal("replacement reservation was not transferred atomically", saved.CoinReservations)
	}
	if !saved.ParentOrders[p.SourceOfferID].Quantities.Closed || saved.ParentOrders[p.SourceOfferID].Quantities.Available != 0 || saved.ParentOrders[p.SourceOfferID].Quantities.Released != p.SellAmount || saved.OrderRecords[p.SourceOfferID].ReplacedBy != result.ID || saved.OrderRecords[result.ID].Replaces != p.SourceOfferID || saved.OrderRecords[result.ID].Publication != "local_committed" {
		t.Fatal("durable replacement lineage missing")
	}
	if saved.TradeReceipts[request.RequestID].Result != result {
		t.Fatal("accepted receipt was not in the same snapshot")
	}
	if len(saved.Activities[activityID("order", p.SourceOfferID)].RelatedIDs) == 0 {
		t.Fatal("T08 history lost replacement relation")
	}
	e.s = saved
	e.tradeQuotes, e.tradeConfirming = nil, nil
	e.s.TradeReceipts[result.ID].Snapshot.Quote.Expires = time.Now().Unix() - 1
	if again := confirmQuote(t, e, request); again != result || len(e.s.Offers) != 2 {
		t.Fatal("restart/expired retry created another order", again)
	}
	request.Revision = transport.RandomID()
	raw, _ = json.Marshal(request)
	if _, err := e.confirmTrade(context.Background(), raw); err == nil {
		t.Fatal("changed terms reused replacement identity")
	}
	from, message := orderRequest(t, e, old)
	if err := e.handle(from, message); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Swaps) != 0 {
		t.Fatal("stale relay offer was accepted after replacement")
	}
	rejected := false
	for _, d := range e.s.Outbox {
		rejected = rejected || d.Type == "rejected"
	}
	if !rejected {
		t.Fatal("stale request was not rejected")
	}
}

func TestOrderReplacementRejectsChangedSourceAndInsufficientCoins(t *testing.T) {
	for _, mode := range []string{"coins", "event", "wallet", "reserved"} {
		t.Run(mode, func(t *testing.T) {
			e, p, old := managedSource(t)
			switch mode {
			case "coins":
				e.walletCoins[chain.Blake] = map[string][]chain.UTXO{}
			case "event":
				p.SourceEventID = transport.RandomID()
			case "wallet":
				p.ExpectedWallet = "another"
			case "reserved":
				from, message := orderRequest(t, e, old)
				if err := e.handle(from, message); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(e.s)
			raw, _ := json.Marshal(p)
			q, err := e.quoteTrade(context.Background(), raw)
			if err == nil && q.Ready {
				t.Fatal("invalid replacement authorized", q)
			}
			after, _ := json.Marshal(e.s)
			if !bytes.Equal(before, after) {
				t.Fatal("failed replacement released or changed the original order")
			}
		})
	}
}

func TestOrderReplacementQuoteExpiresWithSource(t *testing.T) {
	now := time.Now().Unix()
	e, p, _ := managedSourceExpiry(t, now+20)
	o := e.s.OrderRecords[p.SourceOfferID].Offer
	p.SourceEventID = e.s.Offers[o.ID].ID.Hex()
	p.Expires = now + 600
	snapshot, err := e.tradeSnapshot(p, now)
	if err != nil || snapshot.Quote.Expires != o.Expires {
		t.Fatal("replacement quote outlives source", snapshot.Quote.Expires, o.Expires, err)
	}
	if err := e.validateTradeSource(snapshot, o.Expires); err == nil {
		t.Fatal("expired source remained replaceable")
	}
	// Recreating an expired order reviews entirely new terms and a new expiry.
	p.OrderAction = "recreate"
	snapshot, err = e.tradeSnapshot(p, o.Expires)
	if err != nil || snapshot.Quote.Expires != o.Expires+tradeQuoteLifetime {
		t.Fatal("historical expiry limited fresh recreation", snapshot.Quote.Expires, err)
	}
}

func TestOrderReplacementAndAcceptanceRace(t *testing.T) {
	e, p, old := managedSource(t)
	request := confirmation(requestQuote(t, e, p))
	from, message := orderRequest(t, e, old)
	var result ConfirmTradeResult
	var confirmErr, acceptErr error
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		raw, _ := json.Marshal(request)
		result, confirmErr = e.confirmTrade(context.Background(), raw)
	}()
	go func() {
		defer wg.Done()
		<-start
		e.mu.Lock()
		defer e.mu.Unlock()
		acceptErr = e.handle(from, message)
	}()
	close(start)
	wg.Wait()
	if confirmErr != nil || acceptErr != nil {
		t.Fatal(confirmErr, acceptErr)
	}
	if result.State == "accepted" {
		if len(e.s.Swaps) != 0 || len(e.s.Offers) != 2 || len(e.s.CoinReservations) != 1 {
			t.Fatal("replace winner left two spendable offers")
		}
	} else if result.State == "rejected" {
		parent := e.s.ParentOrders[p.SourceOfferID]
		child := e.s.FillRecords[message.SwapID]
		if len(e.s.Swaps) != 1 || len(e.s.Offers) != 1 || parent.Quantities.Available != 0 || parent.Quantities.Reserved != p.SellAmount || child == nil || child.Allocation.Disposition != FillReserved || len(child.Inputs) != 1 || !reflect.DeepEqual(e.s.CoinReservations["swap/"+message.SwapID].Inputs, child.Inputs) || len(e.s.CoinReservations["offer/"+p.SourceOfferID].Inputs) != 0 {
			t.Fatal("acceptance winner lost its durable reservation")
		}
	} else {
		t.Fatal(result)
	}
}

type replacementCrashBackend struct {
	chain.Backend
	once   sync.Once
	before func()
}

func (b *replacementCrashBackend) Output(ctx context.Context, id string, vout uint32) (*chain.TxOut, error) {
	b.once.Do(b.before)
	return b.Backend.Output(ctx, id, vout)
}

func TestOrderReplacementInterruptedBeforeAtomicCommit(t *testing.T) {
	e, p, _ := managedSource(t)
	request := confirmation(requestQuote(t, e, p))
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	backend := e.nodes[chain.Blake]
	e.nodes[chain.Blake] = &replacementCrashBackend{Backend: backend, before: func() {
		if err := e.vault.Close(); err != nil {
			t.Fatal(err)
		}
	}}
	raw, _ := json.Marshal(request)
	if _, err := e.confirmTrade(context.Background(), raw); err == nil || e.fatal == nil {
		t.Fatal("failed durable commit did not stop execution", err)
	}
	if len(e.s.Offers) != 2 {
		t.Fatal("fault did not reach final replacement commit boundary")
	}
	reopened, err := storage.Open(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var saved State
	if _, err := reopened.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Offers) != 1 || saved.OrderRecords[p.SourceOfferID].Offer.Status != "open" || len(saved.CoinReservations) != 1 || saved.TradeReceipts[request.RequestID].Result.State != "pending" {
		t.Fatal("interrupted replacement persisted half a transfer")
	}
	e = reopenedFixtureEngine(t, e, reopened, saved)
	e.nodes[chain.Blake] = backend
	e.tradeQuotes, e.tradeConfirming = nil, nil
	if result := confirmQuote(t, e, request); result.State != "accepted" || len(e.s.Offers) != 2 || len(e.s.CoinReservations) != 1 {
		t.Fatal("original pending identity did not resume safely", result)
	}
}

func TestOrderExpiredHistoryCancellationAndRecreation(t *testing.T) {
	e, p, _ := managedSourceExpiry(t, time.Now().Unix()+5)
	o := e.s.OrderRecords[p.SourceOfferID].Offer
	// Reach the configured expiry without rewriting any immutable parent field.
	time.Sleep(time.Until(time.Unix(o.Expires, 0)))
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.s.Book = map[string]nostr.Event{} // History does not depend on time-valid relay entries.
	qraw, _ := json.Marshal(MarketQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", Owner: "mine", Status: "expired"})
	page, err := e.marketPage(qraw)
	if err != nil || len(page.Records) != 1 || !page.Records[0].CanRecreate {
		t.Fatal("expired own order history missing", page, err)
	}
	eventID := page.Records[0].EventID
	raw, _ := json.Marshal(map[string]string{"id": o.ID, "expected_wallet": "alice", "expected_event_id": eventID})
	if _, err := e.cancelOffer(raw); err != nil {
		t.Fatal(err)
	}
	cancelled := e.s.Offers[o.ID].ID
	if _, err := e.cancelOffer(raw); err != nil || e.s.Offers[o.ID].ID != cancelled {
		t.Fatal("cancellation retry made another event", err)
	}
	if err := e.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.s.OrderRecords[o.ID].Publication != "local_committed" || len(e.s.Outbox) == 0 {
		t.Fatal("no configured relay was reported as an acknowledgement")
	}
	p.OrderActionFields = OrderActionFields{OrderAction: "recreate", SourceOfferID: o.ID, SourceEventID: cancelled.Hex()}
	p.Expires = time.Now().Unix() + 600
	result := confirmQuote(t, e, confirmation(requestQuote(t, e, p)))
	if result.State != "accepted" || result.ID == o.ID || e.s.OrderRecords[result.ID].RecreatedFrom != o.ID || e.s.OrderRecords[result.ID].Offer.Expires != p.Expires {
		t.Fatal("recreation/expiry did not produce fresh reviewed order", result)
	}
}
