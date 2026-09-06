package api

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestTradeAPIPreservesBindingAndSeparateAssetEconomics(t *testing.T) {
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		switch r.Method {
		case "trade.quote":
			var p daemon.TradeQuoteRequest
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.Kind != "taker" || p.ExpectedWallet != "alice" || p.ExpectedNetwork != "testnet" || p.FundingFee != 6500 || p.Rate != 2500 || p.Timestamp != 123 || p.OwnerFeeCap != 20000 || p.TowerPubKey != "provider" || p.SellAmount != 9007199254740993 {
				t.Fatal("request lost precision or binding", p)
			}
			return daemon.TradeQuote{Token: "token", Revision: "revision", Kind: p.Kind, Wallet: p.ExpectedWallet, WalletKey: "key", Network: chain.Testnet, OfferEventID: "event", ProviderRevision: "proof", PaidChain: chain.BTC, PaidPrincipal: 9007199254740993, PaidTotal: 9007199254747493, ReceivedChain: chain.Blake, ReceivedPrincipal: 123456789, Fees: p.FeeSelection, Provider: protocol.Tower{PubKey: p.TowerPubKey, BPS: p.TowerBPS}, Outcomes: []daemon.TradeOutcome{{Kind: "owner_claim", Chain: chain.Blake, Principal: 123456789, FeeMin: 2000, FeeMax: 20000, NetMin: 123436789, NetMax: 123454789}}, Timing: daemon.TradeTiming{Unit: "seconds", Confirmations: 6, FirstRevealer: "taker"}, Funds: daemon.FundsPreflight{State: "proven", Sufficient: true}, Ready: true}, nil
		case "trade.confirm":
			var p daemon.ConfirmTradeRequest
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.Token != "token" || p.Revision != "revision" || p.RequestID != "request" || p.ExpectedWallet != "alice" || p.ExpectedNetwork != "testnet" {
				t.Fatal(p)
			}
			return daemon.ConfirmTradeResult{ID: p.RequestID, Kind: "taker", State: "pending"}, nil
		default:
			t.Fatal(r.Method)
			return nil, nil
		}
	}}
	q, err := service.QuoteTrade(context.Background(), &pb.TradeQuoteRequest{Kind: "taker", ExpectedWallet: "alice", ExpectedNetwork: "testnet", SellAmount: 9007199254740993, FundingFee: 6500, RateSatKvb: 2500, FeeTimestamp: 123, OwnerFeeCap: 20000, TowerPubkey: "provider", TowerBps: 50})
	if err != nil || q.GetPaidPrincipal() != 9007199254740993 || q.PaidTotal != 9007199254747493 || q.ReceivedChain != "blake" || q.Fees.RateSatKvb != 2500 || q.ProviderRevision != "proof" || q.Outcomes[0].NetMin != 123436789 || q.Timing.FirstRevealer != "taker" {
		t.Fatal(q, err)
	}
	got, err := service.ConfirmTrade(context.Background(), &pb.ConfirmTradeRequest{Token: q.Token, Revision: q.Revision, RequestId: "request", ExpectedWallet: q.Wallet, ExpectedNetwork: q.Network})
	if err != nil || got.GetState() != "pending" || got.Id != "request" {
		t.Fatal(got, err)
	}
}
