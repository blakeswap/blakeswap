package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestDistinctTakersCompeteForOneDurableMakerReservation(t *testing.T) {
	maker, _, _ := sendFixture(t)
	raw, _ := json.Marshal(map[string]any{"sell": "blake", "sell_amount": 100000, "buy_amount": 200000})
	result, err := maker.Command(context.Background(), Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	advertised := maker.s.Offers[offer.ID]
	requests := make([]nostr.Event, 2)
	for i := range requests {
		taker := nostr.Generate()
		id := transport.RandomID()
		keys, err := maker.swapKeys(id)
		if err != nil {
			t.Fatal(err)
		}
		request := protocol.Request{ID: id, OfferEvent: advertised, Taker: taker.Public().Hex(), Hash: strings.Repeat("12", 32), Keys: keys}
		body, _ := json.Marshal(request)
		requests[i], err = transport.Wrap(taker, maker.identity.Public(), transport.Message{Version: 1, ID: transport.RandomID(), Type: "request", SwapID: id, Body: body})
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, event := range requests {
		wg.Add(1)
		go func(event nostr.Event) {
			defer wg.Done()
			maker.mu.Lock()
			defer maker.mu.Unlock()
			failures <- maker.receive(event)
		}(event)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(maker.s.Swaps) != 1 {
		t.Fatal("multiple takers obtained the order")
	}
	accepted, rejected := 0, 0
	for _, delivery := range maker.s.Outbox {
		if delivery.Type == "accepted" {
			accepted++
		}
		if delivery.Type == "rejected" {
			rejected++
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatal("expected one acceptance and one rejection", accepted, rejected)
	}
	var saved State
	if _, err := maker.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	var reserved protocol.Offer
	if json.Unmarshal([]byte(saved.Offers[offer.ID].Content), &reserved) != nil || reserved.Status != "reserved" || saved.Swaps[reserved.Reservation] == nil {
		t.Fatal("maker reservation was not durable")
	}
	maker.s = saved
	for _, event := range requests {
		maker.mu.Lock()
		err := maker.receive(event)
		maker.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(maker.s.Swaps) != 1 {
		t.Fatal("delivery replay after reopen created a competing trade")
	}
	for _, swap := range maker.s.Swaps {
		if swap.ShortFunding != "" || swap.LongFunding != "" {
			t.Fatal("reservation handshake unexpectedly funded")
		}
	}
}
