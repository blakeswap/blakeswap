package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

const exactFillAmount int64 = 9007199254740993
const exactParentRevision uint64 = 18446744073709551615

func TestPartialFillCreationAndQuoteMapping(t *testing.T) {
	fields := daemon.FillOrderFields{
		FillPolicy:    protocol.FillPolicy{Mode: "partial", Min: 400000, Max: 600000},
		FeeBudgets:    map[chain.ID]int64{chain.BTC: exactFillAmount, chain.Blake: 20000},
		BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 12345},
	}
	for _, action := range []string{"", "replace", "recreate"} {
		t.Run("quote/"+action, func(t *testing.T) {
			service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
				var q daemon.TradeQuoteRequest
				if err := json.Unmarshal(r.Params, &q); err != nil {
					t.Fatal(err)
				}
				if r.Method != "trade.quote" || q.Kind != "maker" || q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" || q.OrderAction != action || q.SourceOfferID != "source" || q.SourceEventID != "signed" || q.FillPolicy != fields.FillPolicy || q.FeeBudgets[chain.BTC] != exactFillAmount || q.BountyBudgets[chain.Blake] != 12345 {
					t.Fatal("creation review lost exact terms", r.Method, q)
				}
				if v, present := q.BountyBudgets[chain.BTC]; !present || v != 0 {
					t.Fatal("explicit zero bounty disappeared")
				}
				return daemon.TradeQuote{FillOrderFields: q.FillOrderFields, FillTakeFields: daemon.FillTakeFields{ParentRevision: exactParentRevision}, OrderActionFields: q.OrderActionFields,
					Token: "review", Revision: "frozen", Wallet: q.ExpectedWallet, Network: chain.Regtest, Kind: "maker", Available: 900000, TotalSellAmount: 900000, FundingReserve: 13000,
					PaidChain: chain.BTC, PaidPrincipal: 900000, PaidTotal: 913000, ReceivedChain: chain.Blake, ReceivedPrincipal: 1800000,
					ExampleFill: &daemon.FillPreview{Quantity: 400000, BuyAmount: 800000, FundingFee: 6500, OwnerFeeCap: 20000,
						Outcomes: []daemon.TradeOutcome{{Kind: "owner_claim", Chain: chain.Blake, Principal: 800000, FeeMin: 2000, FeeMax: 20000, NetMin: 780000, NetMax: 798000}}}}, nil
			}}
			got, err := service.QuoteTrade(context.Background(), &pb.TradeQuoteRequest{Kind: "maker", ExpectedWallet: "alice", ExpectedNetwork: "regtest", OrderAction: action, SourceOfferId: "source", SourceEventId: "signed", FillMode: "partial", MinFill: 400000, MaxFill: 600000, FeeBudgets: map[string]int64{"btc": exactFillAmount, "blake": 20000}, BountyBudgets: map[string]int64{"btc": 0, "blake": 12345}})
			if err != nil {
				t.Fatal(err)
			}
			if got.FillMode != "partial" || got.MinFill != 400000 || got.MaxFill != 600000 || got.FeeBudgets["btc"] != exactFillAmount || got.ParentRevision != exactParentRevision || got.Available != 900000 || got.TotalSellAmount != 900000 || got.FundingReserve != 13000 || got.PaidTotal != 913000 || got.ReceivedPrincipal != 1800000 || got.Token != "review" || got.Revision != "frozen" || got.OrderAction != action {
				t.Fatal("parent economics lost", got)
			}
			if len(got.Outcomes) != 0 || got.ExampleFill == nil || got.ExampleFill.Quantity != 400000 || got.ExampleFill.BuyAmount != 800000 || got.ExampleFill.FundingFee != 6500 || got.ExampleFill.OwnerFeeCap != 20000 || len(got.ExampleFill.Outcomes) != 1 || got.ExampleFill.Outcomes[0].NetMin != 780000 {
				t.Fatal("representative child conflated with parent results", got)
			}
		})
	}
	for _, explicit := range []bool{false, true} {
		service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
			var in struct {
				daemon.FillOrderFields
				SellAmount int64 `json:"sell_amount"`
			}
			if err := json.Unmarshal(r.Params, &in); err != nil {
				t.Fatal(err)
			}
			if r.Method != "offer.create" || in.Mode != "whole" || in.Min != exactFillAmount || in.Max != exactFillAmount || in.SellAmount != exactFillAmount {
				t.Fatal("direct creation terms changed", r.Method, in)
			}
			if (in.BountyBudgets != nil) != explicit || (in.FeeBudgets != nil) != explicit {
				t.Fatal("omitted caps became explicit authority", in)
			}
			return protocol.Offer{Version: protocol.Version, FillPolicy: in.FillPolicy, Revision: exactParentRevision, Available: exactFillAmount, SellAmount: exactFillAmount}, nil
		}}
		in := &pb.CreateOfferRequest{FillMode: "whole", MinFill: exactFillAmount, MaxFill: exactFillAmount, SellAmount: exactFillAmount}
		if explicit {
			in.FeeBudgets = map[string]int64{"btc": exactFillAmount}
			in.BountyBudgets = map[string]int64{"btc": 0}
		}
		out, err := service.CreateOffer(context.Background(), in)
		if err != nil || out.GetVersion() != 1 || out.Revision != exactParentRevision || out.Available != exactFillAmount || out.FillMode != "whole" || out.MinFill != exactFillAmount || out.MaxFill != exactFillAmount {
			t.Fatal("public order projection lost cutover fields", out, err)
		}
	}
}

func TestPartialFillTakeAndOwnedQuantityMapping(t *testing.T) {
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		switch r.Method {
		case "swap.take", "trade.quote":
			var q daemon.TradeQuoteRequest
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.Quantity != exactFillAmount || q.ParentRevision != exactParentRevision || q.Maker != "maker" || q.ID != "parent" || q.ExpectedNetwork != "regtest" {
				t.Fatal("take quantity/revision substituted", q)
			}
			if r.Method == "swap.take" {
				return map[string]string{"id": "child"}, nil
			}
			return daemon.TradeQuote{FillTakeFields: q.FillTakeFields, PaidPrincipal: 456789, ReceivedPrincipal: q.Quantity, Outcomes: []daemon.TradeOutcome{{Kind: "owner_claim", Principal: q.Quantity}}}, nil
		case "market.list":
			return daemon.MarketPage{Records: []daemon.MarketOrder{{Own: true, SuggestedQuantity: 400000, Quantities: &daemon.QuantitySummary{Total: exactFillAmount, Available: exactFillAmount - 15, Reserved: 1, Committed: 2, Filled: 4, Released: 8}}, {Own: false, SuggestedQuantity: 500000}}}, nil
		case "status":
			return daemon.Status{Swaps: []daemon.PublicSwap{{ID: "child", ParentID: "parent", ParentMaker: "maker", ParentRevision: exactParentRevision, Quantity: exactFillAmount, Allocation: "retired", AllocatedQuantity: 0, AllocationKnown: true}, {ID: "foreign-child", ParentID: "parent", ParentMaker: "foreign", ParentRevision: 3, Quantity: 12345}, {ID: "released-child", ParentID: "parent", ParentMaker: "maker", ParentRevision: 4, Quantity: 23456, Allocation: "released", AllocatedQuantity: 23456, AllocationKnown: true}}}, nil
		}
		t.Fatal("unexpected method", r.Method)
		return nil, nil
	}}
	ctx := context.Background()
	if got, err := service.TakeOffer(ctx, &pb.TakeOfferRequest{Maker: "maker", Id: "parent", ExpectedNetwork: "regtest", Quantity: exactFillAmount, ParentRevision: exactParentRevision}); err != nil || got.GetId() != "child" {
		t.Fatal(got, err)
	}
	if got, err := service.QuoteTrade(ctx, &pb.TradeQuoteRequest{Kind: "taker", Maker: "maker", Id: "parent", ExpectedNetwork: "regtest", Quantity: exactFillAmount, ParentRevision: exactParentRevision}); err != nil || got.GetQuantity() != exactFillAmount || got.ParentRevision != exactParentRevision || got.ExampleFill != nil || len(got.Outcomes) != 1 || got.ReceivedPrincipal != exactFillAmount {
		t.Fatal(got, err)
	}
	market, err := service.ListMarket(ctx, &pb.MarketQuery{})
	if err != nil || len(market.GetRecords()) != 2 {
		t.Fatal(market, err)
	}
	a, b := market.Records[0], market.Records[1]
	if a.SuggestedQuantity != 400000 || a.Quantities == nil || a.Quantities.Total != exactFillAmount || a.Quantities.Available != exactFillAmount-15 || a.Quantities.Reserved != 1 || a.Quantities.Committed != 2 || a.Quantities.Filled != 4 || a.Quantities.Released != 8 || b.Quantities != nil || b.SuggestedQuantity != 500000 {
		t.Fatal("owned ledger conflated with foreign availability", market)
	}
	state, err := service.GetStatus(ctx, &emptypb.Empty{})
	if err != nil || len(state.GetSwaps()) != 3 {
		t.Fatal(state, err)
	}
	child, foreign := state.Swaps[0], state.Swaps[1]
	if child.ParentId != "parent" || child.ParentMaker != "maker" || child.ParentRevision != exactParentRevision || child.Quantity != exactFillAmount || child.Allocation != "retired" || !child.AllocationKnown || child.AllocatedQuantity != 0 || foreign.AllocationKnown || foreign.Allocation != "" || foreign.ParentMaker != "foreign" {
		t.Fatal("child identity/allocation provenance lost", state)
	}
	released := state.Swaps[2]
	if released.Quantity != 23456 || released.Allocation != "released" || released.AllocatedQuantity != released.Quantity || !released.AllocationKnown {
		t.Fatal("released allocation lost its current parent bin", released)
	}
}

func TestV1FillTransportAndUnsupportedRouteRefusal(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs-v1-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var calls atomic.Int32
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		calls.Add(1)
		var q daemon.FillQuery
		if err := json.Unmarshal(r.Params, &q); err != nil {
			t.Fatal(err)
		}
		if r.Method != "fills.list" || q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" || q.ParentMaker != "maker" || q.ParentID != "parent" || q.Offset != 37 || q.Limit != 2 {
			t.Fatal("parent identity/page request changed", r.Method, q)
		}
		if q.Revision == "stale" {
			return nil, status.Error(codes.FailedPrecondition, "fill history revision changed")
		}
		if q.Revision != "frozen" {
			t.Fatal("missing frozen page revision")
		}
		return daemon.FillPage{Wallet: "alice", Network: chain.Regtest, ParentMaker: q.ParentMaker, ParentID: q.ParentID, Revision: q.Revision, Total: 1000, NextOffset: 39, More: true,
			Records: []daemon.FillSummary{{ID: "child", ParentMaker: "maker", ParentID: "parent", ParentRevision: exactParentRevision, Quantity: exactFillAmount, BuyAmount: exactFillAmount - 1, Disposition: "committed", AllocatedQuantity: exactFillAmount, AllocationKnown: true, Stage: "awaiting chain confirmations", Archived: true, MonitoringRequired: true}, {ID: "retired", ParentMaker: "maker", ParentID: "parent", ParentRevision: 2, Quantity: 12345, BuyAmount: 34567, Disposition: "retired", AllocationKnown: true, Stage: "expired"}}}, nil
	}}
	server, err := Listen(context.Background(), filepath.Join(dir, "rpc.sock"), service)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := grpc.NewClient("unix://"+server.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+server.Endpoint.Token)
	client := pb.NewDaemonServiceClient(conn)
	query := &pb.FillQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest", ParentMaker: "maker", ParentId: "parent", Offset: 37, Limit: 2, Revision: "frozen"}
	if _, err = client.ListFills(context.Background(), query); status.Code(err) != codes.Unauthenticated {
		t.Fatal("unauthenticated history", err)
	}
	got, err := client.ListFills(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Wallet != "alice" || got.Network != "regtest" || got.ParentMaker != "maker" || got.ParentId != "parent" || got.Revision != "frozen" || got.Total != 1000 || got.NextOffset != 39 || !got.More || len(got.Records) != 2 {
		t.Fatal("page metadata lost", got)
	}
	child := got.Records[0]
	if child.ParentRevision != exactParentRevision || child.Quantity != exactFillAmount || child.BuyAmount != exactFillAmount-1 || child.AllocatedQuantity != exactFillAmount || !child.AllocationKnown || !child.Archived || !child.MonitoringRequired || child.Disposition != "committed" || child.Stage != "awaiting chain confirmations" || got.Records[1].Disposition != "retired" || got.Records[1].AllocatedQuantity != 0 || !got.Records[1].AllocationKnown {
		t.Fatal("fill row lost fields", got)
	}
	body, _ := protojson.Marshal(query)
	r, _ := http.NewRequest("POST", server.Endpoint.HTTP+"/v1/orders/fills/query", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+server.Endpoint.Token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(string(raw), err)
	}
	var httpPage pb.FillPage
	if err = protojson.Unmarshal(raw, &httpPage); err != nil || !proto.Equal(got, &httpPage) || !bytes.Contains(raw, []byte(`"9007199254740993"`)) || !bytes.Contains(raw, []byte(`"18446744073709551615"`)) {
		t.Fatal("HTTP precision/provenance mismatch", string(raw), err)
	}
	cli, err := Call(ctx, server.Endpoint.Socket, daemon.Request{Method: "fills.list", Params: body})
	var cliPage pb.FillPage
	if err != nil {
		t.Fatal(err)
	}
	if err = protojson.Unmarshal(cli, &cliPage); err != nil || !proto.Equal(got, &cliPage) {
		t.Fatal("CLI fill route mismatch", string(cli), err)
	}
	query.Revision = "stale"
	if _, err = client.ListFills(ctx, query); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("stale history silently refreshed", err)
	}
	before := calls.Load()
	if err = conn.Invoke(ctx, "/blakeswap.v2.DaemonService/GetStatus", &emptypb.Empty{}, &pb.Status{}); status.Code(err) != codes.Unimplemented {
		t.Fatal("unsupported gRPC registered", err)
	}
	for _, legacy := range []struct{ method, path string }{{"GET", "/v2/status"}, {"POST", "/v2/swaps"}, {"POST", "/v2/orders/fills/query"}} {
		r, _ = http.NewRequest(legacy.method, server.Endpoint.HTTP+legacy.path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+server.Endpoint.Token)
		r.Header.Set("Content-Type", "application/json")
		resp, err = http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatal("unsupported HTTP route registered", legacy.path, resp.StatusCode)
		}
	}
	if calls.Load() != before {
		t.Fatal("legacy/unauthorized request reached daemon")
	}
	var schema struct {
		Info  struct{ Version string }
		Paths map[string]json.RawMessage
	}
	if err = json.Unmarshal(OpenAPI, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Info.Version != "1.0" || len(schema.Paths) == 0 {
		t.Fatal("wrong API version", schema.Info)
	}
	for path := range schema.Paths {
		if !strings.HasPrefix(path, "/v1/") {
			t.Fatal("non-v1 generated route", path)
		}
	}
}

func TestPartialFillHTTPExactReviewedInputAndRejection(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs-fill-http-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var calls atomic.Int32
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		calls.Add(1)
		var q daemon.TradeQuoteRequest
		if err := json.Unmarshal(r.Params, &q); err != nil {
			t.Fatal(err)
		}
		if r.Method != "trade.quote" || q.Kind != "taker" || q.ExpectedWallet != "selected" || q.ExpectedNetwork != "regtest" || q.Maker != "maker" || q.ID != "parent" || q.Quantity != exactFillAmount || q.ParentRevision != exactParentRevision || q.FundingFee != 6500 || q.OwnerFeeCap != 20000 || q.FeeBudgets[chain.BTC] != exactFillAmount || q.FeeBudgets[chain.Blake] != 20000 {
			t.Fatal("HTTP changed reviewed input", r.Method, q)
		}
		if value, ok := q.BountyBudgets[chain.BTC]; !ok || value != 0 {
			t.Fatal("HTTP omitted explicit zero", q.BountyBudgets)
		}
		return daemon.TradeQuote{FillOrderFields: q.FillOrderFields, FillTakeFields: q.FillTakeFields, Wallet: q.ExpectedWallet, Network: chain.Regtest}, nil
	}}
	server, err := Listen(context.Background(), filepath.Join(dir, "rpc.sock"), service)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	input := `{"kind":"taker","expected_wallet":"selected","expected_network":"regtest","maker":"maker","id":"parent","quantity":"9007199254740993","parent_revision":"18446744073709551615","funding_fee":"6500","owner_fee_cap":"20000","fee_budgets":{"btc":"9007199254740993","blake":"20000"},"bounty_budgets":{"btc":"0"}}`
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"exact", input, http.StatusOK},
		{"fractional quantity", strings.Replace(input, `"quantity":"9007199254740993"`, `"quantity":"1.5"`, 1), http.StatusBadRequest},
		{"overflow revision", strings.Replace(input, `"18446744073709551615"`, `"18446744073709551616"`, 1), http.StatusBadRequest},
		{"negative revision", strings.Replace(input, `"18446744073709551615"`, `"-1"`, 1), http.StatusBadRequest},
		{"unknown field", strings.TrimSuffix(input, "}") + `,"reservation":"old"}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", server.Endpoint.HTTP+"/v1/trades/quote", strings.NewReader(test.body))
			r.Header.Set("Authorization", "Bearer "+server.Endpoint.Token)
			r.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != test.want {
				t.Fatal(response.StatusCode, string(raw), err)
			}
			if test.want == http.StatusOK {
				var q pb.TradeQuote
				if err := protojson.Unmarshal(raw, &q); err != nil || q.Quantity != exactFillAmount || q.ParentRevision != exactParentRevision || q.FeeBudgets["btc"] != exactFillAmount {
					t.Fatal("HTTP response precision lost", string(raw), err)
				}
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatal("malformed request reached daemon", calls.Load())
	}
}
