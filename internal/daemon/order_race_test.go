package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestDistinctTakersCompeteForOneDurableMakerReservation(t *testing.T) {
	maker, _, _ := sendFixture(t)
	raw, _ := json.Marshal(map[string]any{"sell": "blake", "sell_amount": 100000, "buy_amount": 200000, "fill_mode": "whole", "min_fill": 100000, "max_fill": 100000, "funding_fee": 2000, "owner_fee_cap": 20000, "fee_budgets": map[string]int64{"blake": 22000, "btc": 20000}, "bounty_budgets": map[string]int64{"btc": 0, "blake": 0}})
	result, err := maker.Command(context.Background(), Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	advertised := maker.s.Offers[offer.ID]
	requests := make([]nostr.Event, 2)
	identities := map[string]bool{}
	for i := range requests {
		taker := nostr.Generate()
		request := automationChildRequest(t, maker, advertised)
		request.Taker = taker.Public().Hex()
		for _, identity := range []string{request.ID, request.Hash, request.Keys[offer.Sell], request.Keys[offer.Sell.Other()]} {
			if identities[identity] {
				t.Fatal("competitors share a child identity")
			}
			identities[identity] = true
		}
		makerKeys, err := maker.swapKeys(request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := protocol.NewTerms(request, makerKeys, maker.heights); err != nil {
			t.Fatal("independent competitor is not valid before the reservation race", err)
		}
		body, _ := json.Marshal(request)
		requests[i], err = transport.Wrap(taker, maker.identity.Public(), transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "request", SwapID: request.ID, Body: body})
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
	parent := saved.ParentOrders[offer.ID]
	if parent == nil || parent.Quantities.Total != offer.SellAmount || parent.Quantities.Reserved != offer.SellAmount || parent.Quantities.Available != 0 || parent.Quantities.Committed != 0 || parent.Quantities.Filled != 0 || parent.Quantities.Released != 0 || len(saved.FillRecords) != 1 {
		t.Fatal("maker reservation was not durable")
	}
	for id, child := range saved.FillRecords {
		swap := saved.Swaps[id]
		if swap == nil || child.ParentID != offer.ID || child.RequestDigest != protocol.Digest(swap.Request) || child.Allocation.Disposition != FillReserved || child.Allocation.Quantity != offer.SellAmount || child.Allocation.EverCommitted || len(child.Inputs) != 1 || !reflect.DeepEqual(saved.CoinReservations["swap/"+id].Inputs, child.Inputs) || len(saved.CoinReservations["offer/"+offer.ID].Inputs) != 0 {
			t.Fatal("winning child lacks exclusive durable input and quantity authority")
		}
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
