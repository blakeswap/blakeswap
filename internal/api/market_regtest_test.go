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
			wait := func(label string, allowance time.Duration, ready func() bool) {
				t.Helper()
				deadline := time.Now().Add(allowance)
				for !ready() && time.Now().Before(deadline) {
					h.tick()
					time.Sleep(100 * time.Millisecond)
				}
				if !ready() {
					t.Fatal("did not observe", label)
				}
			}
			visible := func(eventID string) bool {
				page, err := h.clients["taker"].ListMarket(h.contexts["taker"], &pb.MarketQuery{ExpectedWallet: "taker", ExpectedNetwork: "regtest", Owner: "others", Status: "all"})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range page.Records {
					if row.EventId == eventID {
						return true
					}
				}
				return false
			}
			expiry := time.Now().Unix() + 1234
			first := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", FillMode: "whole", MinFill: 1_000_000, MaxFill: 1_000_000, FeeBudgets: map[string]int64{string(sell): 26500, string(sell.Other()): 20000}, BountyBudgets: map[string]int64{"btc": 0, "blake": 0}, Sell: string(sell), SellAmount: 1_000_000, BuyAmount: 2_000_000, FundingFee: 6500, OwnerFeeCap: 20000, Expires: expiry})
			oldID, _ := h.confirm("maker", first)
			old := find(market("maker"), oldID)
			if old.Offer.Expires != expiry || old.Publication != "local_committed" {
				t.Fatal("custom expiry/local commit", old)
			}
			wait("positive maker publication acknowledgement", 20*time.Second, func() bool {
				old = find(market("maker"), oldID)
				return old.Publication == "relay_acknowledged" && old.AcknowledgedAt > 0
			})
			if old.Publication != "relay_acknowledged" || old.AcknowledgedAt == 0 {
				t.Fatal("positive publication ack missing", old)
			}
			// Include the normal 30s history resweep before the taker deliberately
			// retains this exact authenticated revision across the replacement.
			wait("taker's exact original signed order", 50*time.Second, func() bool { return visible(old.EventId) })
			// Keep the taker's authenticated old event deliberately stale while the
			// maker durably cancels/replaces its own source and transfers reservation.
			replacement := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", FillMode: "whole", MinFill: 1_000_000, MaxFill: 1_000_000, FeeBudgets: map[string]int64{string(sell): 26500, string(sell.Other()): 20000}, BountyBudgets: map[string]int64{"btc": 0, "blake": 0}, Sell: string(sell), SellAmount: 1_000_000, BuyAmount: 2_500_000, FundingFee: 6500, OwnerFeeCap: 20000, Expires: expiry + 60, OrderAction: "replace", SourceOfferId: oldID, SourceEventId: old.EventId})
			newID, request := h.confirm("maker", replacement)
			orders := market("maker")
			cancelled := find(orders, oldID)
			created := find(orders, newID)
			if cancelled.Status != "cancelled" || cancelled.ReplacedBy != newID || cancelled.Publication != "local_committed" || created.Replaces != oldID || newID == oldID {
				t.Fatal("replacement lineage", orders)
			}
			requireWholeReviewedParent(t, old.Offer, h.status("maker").Pubkey, 1_000_000, 2_000_000)
			stale := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: old.Offer.Maker, Id: oldID, Sell: string(sell), Quantity: 1_000_000, ParentRevision: old.Offer.Revision, FundingFee: 6500, OwnerFeeCap: 20000})
			staleID, _ := h.confirm("taker", stale)
			// A never-funded rejection may become cold during the same Tick.
			// Resolve this exact child through the API that serves both stores.
			wait("stale signed order rejection before funding", 50*time.Second, func() bool {
				detail, err := h.clients["taker"].GetRecord(h.contexts["taker"], &pb.RecordQuery{Kind: "swap", Id: staleID, ExpectedWallet: "taker", ExpectedNetwork: "regtest"})
				if err != nil || detail.GetKind() != "swap" || detail.GetId() != staleID || detail.GetSwap().GetId() != staleID {
					t.Fatal("stale request detail identity changed", detail, err)
				}
				swap := detail.Swap
				if swap.GetLong().GetTxid() != "" || swap.GetShort().GetTxid() != "" {
					t.Fatal("stale signed order acquired funding", swap)
				}
				return swap.Stage == "rejected"
			})
			for _, s := range h.status("maker").Swaps {
				if s.Id == staleID {
					t.Fatal("stale maker acceptance", s)
				}
			}
			wait("both replacement publication acknowledgements", 20*time.Second, func() bool {
				orders := market("maker")
				created = find(orders, newID)
				cancelled = find(orders, oldID)
				return created.Publication == "relay_acknowledged" && created.AcknowledgedAt > 0 && cancelled.Publication == "relay_acknowledged" && cancelled.AcknowledgedAt > 0
			})
			if created.Publication != "relay_acknowledged" || find(market("maker"), oldID).Publication != "relay_acknowledged" {
				t.Fatal("both replacement publications not acknowledged")
			}
			wait("taker's exact signed replacement", 50*time.Second, func() bool { return visible(created.EventId) })
			requireWholeReviewedParent(t, created.Offer, h.status("maker").Pubkey, 1_000_000, 2_500_000)
			tq := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: created.Offer.Maker, Id: newID, Sell: string(sell), Quantity: 1_000_000, ParentRevision: created.Offer.Revision, FundingFee: 6500, OwnerFeeCap: 20000})
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
					if swap.GetShort().GetChain() != string(sell) || swap.GetShort().GetAmount() != 1_000_000 || swap.GetLong().GetChain() != string(sell.Other()) || swap.GetLong().GetAmount() != 2_500_000 || swap.FundingFee != 6500 || swap.OwnerFeeCap != 20000 || swap.TowerPaid != 0 {
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
