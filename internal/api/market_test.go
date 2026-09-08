package api

import (
	"context"
	"encoding/json"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"testing"
)

func TestMarketTypedFieldsAndOrderReviewBinding(t *testing.T) {
	fields := daemon.OrderActionFields{OrderAction: "replace", SourceOfferID: "old", SourceEventID: "signed"}
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		switch r.Method {
		case "market.list":
			var q daemon.MarketQuery
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" || q.Owner != "mine" || q.Side != "buy_btc" || q.BTCMin != 9_999_999_999 || q.BTCMax != 10_000_000_000 || q.Sort != "rate" || !q.Descending || q.Offset != 3 || q.Revision != "page" {
				t.Fatal(q)
			}
			return daemon.MarketPage{Wallet: "alice", Network: chain.Regtest, Revision: "page", Total: 5, NextOffset: 4, More: true, ObservedAt: 123, AllRelays: true, Records: []daemon.MarketOrder{{Offer: protocol.Offer{ID: "order", Network: chain.Regtest}, EventID: "signed", Own: true, Side: "buy_btc", BTCAmount: 9_999_999_999, BlakeAmount: 10_000_000_000, Rate: "1.00000000", Status: "cancelled", Publication: "relay_acknowledged", AcknowledgedAt: 456, ReplacedBy: "new", SwapIDs: []string{"swap"}, ActivityID: "order/order", CanRecreate: true}}}, nil
		case "trade.quote":
			var q daemon.TradeQuoteRequest
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.OrderActionFields != fields || q.Expires != 1234567890 {
				t.Fatal(q)
			}
			return daemon.TradeQuote{OrderActionFields: q.OrderActionFields, OfferExpires: q.Expires}, nil
		case "fee.quote":
			var q daemon.FeeQuoteRequest
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.ExpectedWallet != "alice" || q.SourceOfferID != "old" || q.SourceEventID != "signed" {
				t.Fatal(q)
			}
			return daemon.FeeQuote{}, nil
		case "offer.cancel":
			var q map[string]any
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q["expected_wallet"] != "alice" || q["expected_event_id"] != "signed" || q["expected_network"] != "regtest" {
				t.Fatal(q)
			}
			return protocol.Offer{ID: "old", Status: "cancelled"}, nil
		default:
			t.Fatal(r.Method)
			return nil, nil
		}
	}}
	ctx := context.Background()
	p, err := service.ListMarket(ctx, &pb.MarketQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", Owner: "mine", Side: "buy_btc", BtcMin: 9_999_999_999, BtcMax: 10_000_000_000, Sort: "rate", Descending: true, Offset: 3, Limit: 1, Revision: "page"})
	if err != nil || p.GetRecords()[0].BtcAmount != 9_999_999_999 || p.Records[0].ReplacedBy != "new" || p.Records[0].SwapIds[0] != "swap" || !p.Records[0].CanRecreate || !p.More || !p.AllRelays || p.ObservedAt != 123 {
		t.Fatal(p, err)
	}
	q, err := service.QuoteTrade(ctx, &pb.TradeQuoteRequest{Kind: "maker", Expires: 1234567890, OrderAction: "replace", SourceOfferId: "old", SourceEventId: "signed"})
	if err != nil || q.GetOrderAction() != "replace" || q.SourceEventId != "signed" || q.SourceOfferId != "old" || q.OfferExpires != 1234567890 {
		t.Fatal(q, err)
	}
	if _, err := service.QuoteFee(ctx, &pb.FeeQuoteRequest{ExpectedWallet: "alice", SourceOfferId: "old", SourceEventId: "signed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CancelOffer(ctx, &pb.CancelOfferRequest{Id: "old", ExpectedWallet: "alice", ExpectedNetwork: "regtest", ExpectedEventId: "signed"}); err != nil {
		t.Fatal(err)
	}
}
