package api

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

func TestStrategyTypedFullAuthorizationAndSeparateAssetReporting(t *testing.T) {
	c := daemon.StrategyConfig{ID: "strategy", Wallet: "alice", Network: chain.Regtest, BTC: daemon.StrategySide{Target: 2100000000000000, MinimumReserve: 9999999999, MaxExposure: 99999999999, MinOffer: 100000, MaxOffer: 10000000000, VolumeLimit: 2100000000000000, FundingFee: 6500, MaxFundingFee: 20000}, Blake: daemon.StrategySide{Target: 2000000, MaxOffer: 1500000}, Rate: daemon.AutomationRate{Numerator: 2099999999999999, Denominator: 2100000000000000}, MaxConsecutiveFailures: 3, MaxReplacementFailures: 2, FailureRateBPS: 7500}
	view := daemon.StrategyView{Config: c, Revision: 4, Enabled: true, RestoreHold: true, Inventory: map[chain.ID]daemon.StrategyInventory{chain.BTC: {ReservedVolume: 9999999999, CommittedVolume: 10000000000, KnownFees: 6500, KnownBounties: 8000}, chain.Blake: {ReservedFees: 20000, CommittedFees: 40000}}, Quotes: []daemon.StrategyQuote{{Sell: chain.Blake, SellAmount: 1500000, BuyAmount: 2500000, Ready: true, Reason: "exact inventory", ReferenceEvents: []string{"event"}}}}
	service := Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		switch r.Method {
		case "strategy.review", "strategy.save":
			var q daemon.StrategyEdit
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.Config.BTC != c.BTC || q.Config.Rate != c.Rate || q.ExpectedRevision != 3 || !q.Enabled || !q.AcknowledgeRestoredBudget {
				t.Fatal("authorization changed", q)
			}
			if r.Method == "strategy.review" {
				return daemon.StrategyReview{Config: c, ExpectedRevision: 3, Enabled: true, ReviewDigest: "exact", Preview: view}, nil
			}
			if q.ReviewDigest != "exact" {
				t.Fatal("missing review")
			}
			return view, nil
		case "strategy.list":
			return daemon.StrategyList{Wallet: "alice", Network: chain.Regtest, Strategies: []daemon.StrategyView{view}}, nil
		case "strategy.stop":
			var q pb.StopStrategyRequest
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if !q.Stop || q.ExpectedRevision != 4 || q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" {
				t.Fatal(&q)
			}
			return view, nil
		default:
			t.Fatal(r.Method)
			return nil, nil
		}
	}}
	raw, _ := json.Marshal(c)
	config := &pb.StrategyConfig{}
	if err := json.Unmarshal(raw, config); err != nil {
		t.Fatal(err)
	}
	edit := &pb.StrategyEdit{Config: config, ExpectedRevision: 3, Enabled: true, AcknowledgeRestoredBudget: true}
	r, err := service.ReviewStrategy(context.Background(), edit)
	if err != nil || r.Preview.Inventory["btc"].KnownFees != 6500 || r.Preview.Inventory["blake"].CommittedFees != 40000 || r.Preview.Quotes[0].BuyAmount != 2500000 {
		t.Fatal(r, err)
	}
	edit.ReviewDigest = r.ReviewDigest
	if _, err := service.SaveStrategy(context.Background(), edit); err != nil {
		t.Fatal(err)
	}
	if list, err := service.ListStrategies(context.Background(), &pb.AutomationQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest"}); err != nil || !list.Strategies[0].RestoreHold {
		t.Fatal(list, err)
	}
	if _, err := service.StopStrategy(context.Background(), &pb.StopStrategyRequest{Id: c.ID, ExpectedWallet: c.Wallet, ExpectedNetwork: "regtest", ExpectedRevision: 4, Stop: true}); err != nil {
		t.Fatal(err)
	}
}
