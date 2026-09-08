package api

import (
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
	"google.golang.org/protobuf/proto"
)

func TestRealInventoryStrategyTradeAndRefund(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		sell   chain.ID
		refund bool
	}{{"sell-btc", chain.BTC, false}, {"sell-blake", chain.Blake, false}, {"stopped-refund", chain.BTC, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newReviewedTradeFixture(t)
			client, ctx := h.clients["maker"], h.contexts["maker"]
			// Separate real confirmed inputs leave a physically unlocked reserve
			// after either side advertises its offer; pending change is not reused.
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				if err := h.nodes[id].WithWallet("faucet").Call(h.ctx, "sendtoaddress", nil, h.status("maker").Addresses[string(id)], chain.Coins(1000000)); err != nil {
					t.Fatal(err)
				}
				h.mine(id, 2)
			}
			h.tick()
			before := h.status("maker")
			side := &pb.StrategySide{Target: 50000000, MinimumReserve: 500000, MaxExposure: 1000000, MinOffer: 500000, MaxOffer: 500000, VolumeLimit: 2000000, FundingFee: 6500, MaxFundingFee: 10000}
			c := &pb.StrategyConfig{Id: transport.RandomID(), Wallet: "maker", Network: "regtest", Btc: proto.Clone(side).(*pb.StrategySide), Blake: proto.Clone(side).(*pb.StrategySide), Rate: &pb.AutomationRate{Numerator: 1, Denominator: 1}, MinRate: &pb.AutomationRate{Numerator: 1, Denominator: 2}, MaxRate: &pb.AutomationRate{Numerator: 2, Denominator: 1}, SpreadBps: 100, MinSpreadBps: 100, MaxSpreadBps: 100, Lifetime: 3600, Cadence: 60, MaxConcurrent: 2, BtcFeeBudget: 300000, BlakeFeeBudget: 300000, Reference: "fixed", MaxConsecutiveFailures: 3, MaxReplacementFailures: 2, FailureRateBps: 8000}
			edit := &pb.StrategyEdit{Config: c, Enabled: true}
			review, err := client.ReviewStrategy(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			edit.ReviewDigest = review.ReviewDigest
			strategy, err := client.SaveStrategy(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			var order *pb.MarketOrder
			for i := 0; i < 6 && order == nil; i++ {
				h.tick()
				market, err := client.ListMarket(ctx, &pb.MarketQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest", Owner: "mine", Status: "open"})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range market.Records {
					if row.Offer.Sell == string(scenario.sell) && row.Publication == "relay_acknowledged" {
						order = row
					}
				}
			}
			if order == nil || order.Offer.SellAmount != 500000 {
				list, _ := client.ListStrategies(ctx, &pb.AutomationQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest"})
				t.Fatal("no acknowledged strategy quote", list)
			}
			buy := int64(505000)
			if scenario.sell == chain.Blake {
				buy = 505051
			}
			if order.Offer.BuyAmount != buy {
				t.Fatal("exact spread rounding changed", order)
			}
			swapID, _ := h.confirm("taker", h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: order.Offer.Maker, Id: order.Offer.Id, Sell: string(scenario.sell), SellAmount: 500000, BuyAmount: buy, FundingFee: 6500, OwnerFeeCap: 20000}))
			find := func(name string) *pb.Swap {
				for _, s := range h.status(name).Swaps {
					if s.Id == swapID {
						return s
					}
				}
				return nil
			}
			// Stop immediately after both funding transactions exist, before the
			// short funding is mined. The refund scenario never gives the taker an
			// eligible first-revelation observation.
			funded := false
			for i := 0; i < 20; i++ {
				h.tick()
				s := find("maker")
				if s != nil && s.Short.Txid != "" {
					if _, err := h.nodes[scenario.sell].Transaction(h.ctx, s.Short.Txid); err == nil {
						funded = true
						break
					}
				}
				h.minePending()
			}
			if !funded {
				t.Fatal("strategy trade never funded", h.status("maker").Swaps)
			}
			accepted := proto.Clone(find("maker")).(*pb.Swap)
			strategy, err = client.StopStrategy(ctx, &pb.StopStrategyRequest{Id: c.Id, ExpectedWallet: "maker", ExpectedNetwork: "regtest", ExpectedRevision: strategy.Revision, Stop: true})
			if err != nil || strategy.Enabled {
				t.Fatal(strategy, err)
			}
			if scenario.refund {
				for _, leg := range []*pb.HTLC{accepted.Long, accepted.Short} {
					height, err := h.nodes[chain.ID(leg.Chain)].Height(h.ctx)
					if err != nil {
						t.Fatal(err)
					}
					if height <= leg.RefundLocktime {
						h.mine(chain.ID(leg.Chain), leg.RefundLocktime-height+1)
					}
				}
			}
			want := "completed"
			if scenario.refund {
				want = "refunded"
			}
			complete := func() bool {
				a, b := find("maker"), find("taker")
				return a != nil && b != nil && a.Stage == want && b.Stage == want && a.LongConfirmations >= 2 && a.ShortConfirmations >= 2
			}
			for i := 0; i < 40 && !complete(); i++ {
				h.tick()
				h.minePending()
			}
			if !complete() {
				t.Fatal("stopped strategy settlement failed", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			h.tick()
			maker := find("maker")
			if maker.Short.Amount != 500000 || maker.Long.Amount != buy || maker.Short.Txid != accepted.Short.Txid || maker.Long.Txid != accepted.Long.Txid {
				t.Fatal("stop changed accepted contract", maker)
			}
			if scenario.refund && maker.SecretRevealed {
				t.Fatal("refund scenario revealed secret")
			}
			fundingFee := actualStrategyFee(t, h, scenario.sell, maker.Short.Txid, 500000)
			if fundingFee != 6500 {
				t.Fatal("actual funding fee", fundingFee)
			}
			settlementChain, settlementID, settlementPrincipal := scenario.sell.Other(), maker.ClaimTxid, buy
			if scenario.refund {
				settlementChain, settlementID, settlementPrincipal = scenario.sell, maker.RefundTxid, int64(500000)
			}
			settlementFee := actualStrategyFee(t, h, settlementChain, settlementID, 0)
			record, err := h.nodes[settlementChain].Transaction(h.ctx, settlementID)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := contract.Parse(record.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if len(tx.TxOut) != 1 || tx.TxOut[0].Value != settlementPrincipal-settlementFee {
				t.Fatal("actual settlement payout", tx)
			}
			// T08's actual ledger must agree with node fees, not the strategy's
			// 20,000-sat conservative settlement authorization.
			var report *pb.StrategyView
			for i := 0; i < 12; i++ {
				h.tick()
				report, err = client.ReportStrategy(ctx, &pb.StrategyReportRequest{Id: c.Id, ExpectedWallet: "maker", ExpectedNetwork: "regtest", ExpectedRevision: strategy.Revision})
				if err != nil {
					t.Fatal(err)
				}
				paid, received := report.Inventory[string(scenario.sell)], report.Inventory[string(scenario.sell.Other())]
				if scenario.refund && paid.KnownFees == fundingFee+settlementFee && received.KnownFees == 0 {
					break
				}
				if !scenario.refund && paid.KnownFees == fundingFee && received.KnownFees == settlementFee {
					break
				}
			}
			paid, received := report.Inventory[string(scenario.sell)], report.Inventory[string(scenario.sell.Other())]
			if scenario.refund {
				if paid.KnownFees != fundingFee+settlementFee || paid.ConfirmedVolume != 0 || received.KnownFees != 0 {
					t.Fatal("refund ledger differs", report)
				}
			} else if paid.KnownFees != fundingFee || received.KnownFees != settlementFee || paid.ConfirmedVolume != 500000 {
				t.Fatal("trade ledger differs", report)
			}
			after := h.status("maker")
			wantPaid := before.Funds[string(scenario.sell)].TotalConfirmed - int64(500000) - fundingFee
			wantReceived := before.Funds[string(scenario.sell.Other())].TotalConfirmed + buy - settlementFee
			if scenario.refund {
				wantPaid = before.Funds[string(scenario.sell)].TotalConfirmed - fundingFee - settlementFee
				wantReceived = before.Funds[string(scenario.sell.Other())].TotalConfirmed
			}
			if after.Funds[string(scenario.sell)].TotalConfirmed != wantPaid || after.Funds[string(scenario.sell.Other())].TotalConfirmed != wantReceived {
				t.Fatal("confirmed inventory differs from actual payouts/fees", after.Funds, wantPaid, wantReceived)
			}
			h.restart("maker")
			client, ctx = h.clients["maker"], h.contexts["maker"]
			list, err := client.ListStrategies(ctx, &pb.AutomationQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest"})
			if err != nil || len(list.Strategies) != 1 {
				t.Fatal(list, err)
			}
			restored := list.Strategies[0]
			if restored.Enabled || restored.Inventory[string(scenario.sell)].CommittedVolume != 500000 || restored.Inventory[string(scenario.sell)].CommittedFees != 26500 || restored.Inventory[string(scenario.sell.Other())].CommittedFees != 20000 {
				t.Fatal("restart reset gross authorization", restored)
			}
			for i := 0; i < 3; i++ {
				h.tick()
			}
			market, err := client.ListMarket(ctx, &pb.MarketQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest", Owner: "mine", Status: "open"})
			if err != nil || len(market.Records) != 0 {
				t.Fatal("stopped restart created quotes", market, err)
			}
			t.Logf("%s principal=500000 received=%d actual funding=%d settlement=%d; inventory/ledger and immutable committed restart verified", scenario.name, buy, fundingFee, settlementFee)
		})
	}
}

func actualStrategyFee(t *testing.T, h *reviewedTradeFixture, id chain.ID, txid string, principal int64) int64 {
	t.Helper()
	record, err := h.nodes[id].Transaction(h.ctx, txid)
	if err != nil || record.Confirmations < 2 {
		t.Fatal("transaction not confirmed", txid, err)
	}
	tx, err := contract.Parse(record.Hex)
	if err != nil {
		t.Fatal(err)
	}
	if principal > 0 && tx.TxOut[0].Value != principal {
		t.Fatal("actual funded principal", tx.TxOut[0].Value)
	}
	var inputs, outputs int64
	for _, in := range tx.TxIn {
		parent, err := h.nodes[id].Transaction(h.ctx, in.PreviousOutPoint.Hash.String())
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
	if inputs < outputs {
		t.Fatal("negative fee")
	}
	return inputs - outputs
}
