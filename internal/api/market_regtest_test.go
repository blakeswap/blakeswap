package api

import (
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestRealManagedOrderThroughTypedAPI(t *testing.T) {
	h := newReviewedTradeFixture(t)
	market := func(name string) *pb.MarketPage {
		t.Helper()
		p, err := h.clients[name].ListMarket(h.contexts[name], &pb.MarketQuery{ExpectedWallet: name, ExpectedNetwork: "regtest", Owner: "mine", Status: "all"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	find := func(p *pb.MarketPage, id string) *pb.MarketOrder {
		t.Helper()
		for _, row := range p.Records {
			if row.Offer.Id == id {
				return row
			}
		}
		t.Fatal("order missing", id, p)
		return nil
	}
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			expiry := time.Now().Unix() + 1234
			first := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", Sell: string(sell), SellAmount: 1_000_000, BuyAmount: 2_000_000, FundingFee: 6500, OwnerFeeCap: 20000, Expires: expiry})
			oldID, _ := h.confirm("maker", first)
			old := find(market("maker"), oldID)
			if old.Offer.Expires != expiry || old.Publication != "local_committed" {
				t.Fatal("custom expiry/local commit", old)
			}
			h.tick()
			old = find(market("maker"), oldID)
			if old.Publication != "relay_acknowledged" || old.AcknowledgedAt == 0 {
				t.Fatal("positive publication ack missing", old)
			}
			// Keep the taker's authenticated old event deliberately stale while the
			// maker durably cancels/replaces its own source and transfers reservation.
			replacement := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", Sell: string(sell), SellAmount: 1_500_000, BuyAmount: 2_500_000, FundingFee: 6500, OwnerFeeCap: 20000, Expires: expiry + 60, OrderAction: "replace", SourceOfferId: oldID, SourceEventId: old.EventId})
			newID, request := h.confirm("maker", replacement)
			orders := market("maker")
			cancelled := find(orders, oldID)
			created := find(orders, newID)
			if cancelled.Status != "cancelled" || cancelled.ReplacedBy != newID || cancelled.Publication != "local_committed" || created.Replaces != oldID || newID == oldID {
				t.Fatal("replacement lineage", orders)
			}
			stale := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: old.Offer.Maker, Id: oldID, Sell: string(sell), SellAmount: 1_000_000, BuyAmount: 2_000_000, FundingFee: 6500, OwnerFeeCap: 20000})
			staleID, _ := h.confirm("taker", stale)
			for i := 0; i < 4; i++ {
				h.tick()
			}
			rejected := false
			for _, s := range h.status("taker").Swaps {
				if s.Id == staleID {
					rejected = s.Stage == "rejected" && s.GetLong().GetTxid() == "" && s.GetShort().GetTxid() == ""
				}
			}
			if !rejected {
				t.Fatal("stale signed offer was not rejected before funding", h.status("taker").Swaps)
			}
			for _, s := range h.status("maker").Swaps {
				if s.Id == staleID {
					t.Fatal("stale maker acceptance", s)
				}
			}
			created = find(market("maker"), newID)
			if created.Publication != "relay_acknowledged" || find(market("maker"), oldID).Publication != "relay_acknowledged" {
				t.Fatal("both replacement publications not acknowledged")
			}
			tq := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: created.Offer.Maker, Id: newID, Sell: string(sell), SellAmount: 1_500_000, BuyAmount: 2_500_000, FundingFee: 6500, OwnerFeeCap: 20000})
			swapID, _ := h.confirm("taker", tq)
			complete := func() bool {
				for _, name := range []string{"maker", "taker"} {
					found := false
					for _, s := range h.status(name).Swaps {
						if s.Id == swapID {
							found = s.Stage == "completed"
						}
					}
					if !found {
						return false
					}
				}
				return true
			}
			for i := 0; i < 40 && !complete(); i++ {
				h.tick()
				h.minePending()
			}
			if !complete() {
				t.Fatal("replacement did not settle", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			for _, name := range []string{"maker", "taker"} {
				for _, swap := range h.status(name).Swaps {
					if swap.Id != swapID {
						continue
					}
					if swap.GetShort().GetChain() != string(sell) || swap.GetShort().GetAmount() != 1_500_000 || swap.GetLong().GetChain() != string(sell.Other()) || swap.GetLong().GetAmount() != 2_500_000 || swap.FundingFee != 6500 || swap.OwnerFeeCap != 20000 || swap.TowerPaid != 0 {
						t.Fatal("settlement did not preserve exact reviewed replacement terms", name, swap)
					}
					funded := swap.Short
					if name == "taker" {
						funded = swap.Long
					}
					node := h.nodes[chain.ID(funded.Chain)]
					record, err := node.Transaction(h.ctx, funded.Txid)
					if err != nil || record.Confirmations < 2 {
						t.Fatal("replacement funding not positively confirmed", name, err, record)
					}
					tx, err := contract.Parse(record.Hex)
					if err != nil || int(funded.Vout) >= len(tx.TxOut) || tx.TxOut[funded.Vout].Value != funded.Amount {
						t.Fatal("actual contract principal differs from reviewed replacement", name, err)
					}
					var fee int64
					for _, input := range tx.TxIn {
						prev, err := node.Transaction(h.ctx, input.PreviousOutPoint.Hash.String())
						if err != nil {
							t.Fatal(err)
						}
						previous, err := contract.Parse(prev.Hex)
						if err != nil || int(input.PreviousOutPoint.Index) >= len(previous.TxOut) {
							t.Fatal("invalid funding prevout", err)
						}
						fee += previous.TxOut[input.PreviousOutPoint.Index].Value
					}
					for _, output := range tx.TxOut {
						fee -= output.Value
					}
					if fee != 6500 {
						t.Fatal("actual replacement funding fee differs from review", name, fee)
					}
				}
			}
			h.restart("maker")
			filled := find(market("maker"), newID)
			if filled.Status != "filled" || !filled.CanRecreate || filled.CanReplace || len(filled.SwapIds) != 1 || filled.SwapIds[0] != swapID {
				t.Fatal("durable finished order link", filled)
			}
			a, err := h.clients["maker"].ConfirmTrade(h.contexts["maker"], request)
			b, err2 := h.clients["maker"].ConfirmTrade(h.contexts["maker"], request)
			if err != nil || err2 != nil || a.GetState() != "accepted" || !proto.Equal(a, b) {
				t.Fatal("replacement retry", a, b, err, err2)
			}
			t.Logf("%s old=%s replacement=%s stale_request=%s accepted_swap=%s settled with durable links", sell, oldID, newID, staleID, swapID)
		})
	}
}
