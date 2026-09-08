package api

import (
	"sync"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
	"google.golang.org/protobuf/proto"
)

func TestRealAutomaticOfferPolicyUpdatePreservesAcceptedTrade(t *testing.T) {
	h := newReviewedTradeFixture(t)
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			client, ctx := h.clients["maker"], h.contexts["maker"]
			rate := &pb.AutomationRate{Numerator: 2, Denominator: 1}
			if sell == chain.Blake {
				rate = &pb.AutomationRate{Numerator: 1, Denominator: 2}
			}
			config := &pb.AutomationConfig{Id: transport.RandomID(), Wallet: "maker", Network: "regtest", Sell: string(sell), SellAmount: 1000000, VolumeLimit: 2000000, Rate: rate, MinRate: &pb.AutomationRate{Numerator: 1, Denominator: 4}, MaxRate: &pb.AutomationRate{Numerator: 4, Denominator: 1}, Lifetime: 3600, Cadence: 60, MaxOpen: 1, FundingFee: 6500, MaxFundingFee: 10000, BtcFeeBudget: 100000, BlakeFeeBudget: 100000, Reference: "fixed"}
			edit := &pb.AutomationEdit{Config: config, Enabled: true}
			review, err := client.ReviewAutomation(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			edit.ReviewDigest = review.ReviewDigest
			policy, err := client.SaveAutomation(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			var order *pb.MarketOrder
			for i := 0; i < 4 && order == nil; i++ {
				h.tick()
				list, err := client.ListAutomations(ctx, &pb.AutomationQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest"})
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range list.Policies {
					if p.Config.Id == config.Id {
						policy = p
					}
				}
				market, err := client.ListMarket(ctx, &pb.MarketQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest", Owner: "mine", Status: "all"})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range market.Records {
					if row.Offer.Id == policy.CurrentOfferId && row.Publication == "relay_acknowledged" {
						order = row
					}
				}
			}
			if order == nil || order.Offer.SellAmount != 1000000 || order.Offer.BuyAmount != 2000000 || policy.Usage.ReservedVolume != 1000000 {
				t.Fatal("automatic offer missing or not exact", policy, order)
			}
			take := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: order.Offer.Maker, Id: order.Offer.Id, Sell: string(sell), SellAmount: 1000000, BuyAmount: 2000000, FundingFee: 6500, OwnerFeeCap: 20000})
			swapID, _ := h.confirm("taker", take)
			for i := 0; i < 3; i++ {
				h.tick()
			}
			accepted := false
			for _, s := range h.status("maker").Swaps {
				if s.Id == swapID {
					accepted = s.Short.Amount == 1000000 && s.Long.Amount == 2000000
				}
			}
			if !accepted {
				t.Fatal("maker did not retain accepted terms before policy edit")
			}
			// Edit the future policy while the daemon advances this accepted trade.
			edit = &pb.AutomationEdit{Config: proto.Clone(config).(*pb.AutomationConfig), ExpectedRevision: policy.Revision, Enabled: false}
			edit.Config.SellAmount = 1500000
			edit.Config.VolumeLimit = 3000000
			edit.Config.Rate = &pb.AutomationRate{Numerator: 3, Denominator: 2}
			review, err = client.ReviewAutomation(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			edit.ReviewDigest = review.ReviewDigest
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); policy, err = client.SaveAutomation(ctx, edit) }()
			h.tick()
			wg.Wait()
			if err != nil || policy.Enabled || policy.Config.SellAmount != 1500000 {
				t.Fatal("policy update failed", policy, err)
			}
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
				t.Fatal("accepted automatic trade did not settle", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			for _, name := range []string{"maker", "taker"} {
				for _, s := range h.status(name).Swaps {
					if s.Id != swapID {
						continue
					}
					if s.Short.Chain != string(sell) || s.Short.Amount != 1000000 || s.Long.Chain != string(sell.Other()) || s.Long.Amount != 2000000 || s.FundingFee != 6500 || s.OwnerFeeCap != 20000 {
						t.Fatal("policy edit changed immutable accepted terms", s)
					}
					funding := s.Short
					if name == "taker" {
						funding = s.Long
					}
					record, err := h.nodes[chain.ID(funding.Chain)].Transaction(h.ctx, funding.Txid)
					if err != nil || record.Confirmations < 2 {
						t.Fatal("funding not confirmed", record, err)
					}
					tx, err := contract.Parse(record.Hex)
					if err != nil || tx.TxOut[funding.Vout].Value != funding.Amount {
						t.Fatal("actual principal differs", err)
					}
					var inputs, outputs int64
					for _, in := range tx.TxIn {
						parent, err := h.nodes[chain.ID(funding.Chain)].Transaction(h.ctx, in.PreviousOutPoint.Hash.String())
						if err != nil {
							t.Fatal(err)
						}
						prev, err := contract.Parse(parent.Hex)
						if err != nil {
							t.Fatal(err)
						}
						inputs += prev.TxOut[in.PreviousOutPoint.Index].Value
					}
					for _, out := range tx.TxOut {
						outputs += out.Value
					}
					if inputs-outputs != 6500 {
						t.Fatal("actual funding fee changed", inputs-outputs)
					}
				}
			}
			h.restart("maker")
			client = h.clients["maker"]
			ctx = h.contexts["maker"]
			list, err := client.ListAutomations(ctx, &pb.AutomationQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range list.Policies {
				if p.Config.Id == config.Id {
					found = true
					if p.Enabled || p.Usage.CommittedVolume != 1000000 || p.Usage.ReservedVolume != 0 || p.CurrentOfferId != order.Offer.Id {
						t.Fatal("restart lost immutable charge/disable", p)
					}
					paid, received := p.Usage.CommittedBtcFees, p.Usage.CommittedBlakeFees
					if sell == chain.Blake {
						paid, received = received, paid
					}
					if paid != 26500 || received != 20000 {
						t.Fatal("wrong chain authorization charges", p.Usage)
					}
				}
			}
			if !found {
				t.Fatal("policy missing after restart")
			}
		})
	}
}
