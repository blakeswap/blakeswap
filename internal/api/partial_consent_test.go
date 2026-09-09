package api

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Exercise private prepare normalization and the real public service mapper
// with the same one-use authority. No signing or external fixture is involved.
func TestPartialFillV1ConsentBindsEveryNewInput(t *testing.T) {
	maker := &pb.CreateOfferRequest{Sell: "btc", SellAmount: 9007199254740993, BuyAmount: 1000001, FillMode: "partial", MinFill: 400000, MaxFill: 600000, FeeBudgets: map[string]int64{"btc": 9007199254740993, "blake": 20000}, BountyBudgets: map[string]int64{"btc": 0, "blake": 0}, FundingFee: 6500, TowerPubkey: "reviewed-provider", ExpectedNetwork: "regtest"}
	taker := &pb.TakeOfferRequest{Maker: "maker", Id: "parent", Quantity: 400000, ParentRevision: math.MaxUint64, FundingFee: 6500, OwnerFeeCap: 20000, TowerPubkey: "reviewed-provider", ExpectedNetwork: "regtest"}
	confirm := &pb.ConfirmTradeRequest{RequestId: "saved-request", Token: "reviewed-token", Revision: "reviewed-digest", ExpectedWallet: "alice", ExpectedNetwork: "regtest"}
	cases := []struct {
		name, method string
		input        proto.Message
		change       func(proto.Message)
	}{
		{"mode", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).FillMode = "whole" }},
		{"minimum", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).MinFill++ }},
		{"maximum", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).MaxFill-- }},
		{"funding", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).FundingFee++ }},
		{"fee-budget", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).FeeBudgets["btc"]-- }},
		{"explicit-zero-bounty", "offer.create", maker, func(p proto.Message) { delete(p.(*pb.CreateOfferRequest).BountyBudgets, "btc") }},
		{"bounty-budget", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).BountyBudgets["btc"]++ }},
		{"provider", "offer.create", maker, func(p proto.Message) { p.(*pb.CreateOfferRequest).TowerPubkey = "changed" }},
		{"quantity", "swap.take", taker, func(p proto.Message) { p.(*pb.TakeOfferRequest).Quantity++ }},
		{"parent-revision", "swap.take", taker, func(p proto.Message) { p.(*pb.TakeOfferRequest).ParentRevision-- }},
		{"parent-maker", "swap.take", taker, func(p proto.Message) { p.(*pb.TakeOfferRequest).Maker = "other" }},
		{"parent-id", "swap.take", taker, func(p proto.Message) { p.(*pb.TakeOfferRequest).Id = "other" }},
		{"network", "swap.take", taker, func(p proto.Message) { p.(*pb.TakeOfferRequest).ExpectedNetwork = "mainnet" }},
		{"receipt-id", "trade.confirm", confirm, func(p proto.Message) { p.(*pb.ConfirmTradeRequest).RequestId = "new" }},
		{"quote-token", "trade.confirm", confirm, func(p proto.Message) { p.(*pb.ConfirmTradeRequest).Token = "new" }},
		{"quote-revision", "trade.confirm", confirm, func(p proto.Message) { p.(*pb.ConfirmTradeRequest).Revision = "new" }},
		{"wallet", "trade.confirm", confirm, func(p proto.Message) { p.(*pb.ConfirmTradeRequest).ExpectedWallet = "bob" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := protojson.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := NormalizeSensitiveAction(tc.method, raw)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(tc.input)
			if !bytes.Equal(normalized, encoded) {
				t.Fatal("private normalization differs from public protobuf encoding")
			}
			digest, err := daemon.ActionDigest(normalized)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := authorization.New("isolated-session", time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer authority.Close()
			action := authorization.Action{Installation: "installation", Wallet: "alice", WalletKey: "wallet-key", Network: "regtest", Epoch: "epoch", Method: tc.method, Digest: digest}
			challenge, err := authority.Prepare(action)
			if err != nil {
				t.Fatal(err)
			}
			if err = authority.Approve(challenge); err != nil {
				t.Fatal(err)
			}
			accepted := 0
			service := &Service{Command: func(ctx context.Context, request daemon.Request) (any, error) {
				current := action
				current.Method = request.Method
				current.Digest, err = daemon.ActionDigest(request.Params)
				if err != nil {
					return nil, err
				}
				if err := authority.Consume(ctx, current); err != nil {
					return nil, err
				}
				accepted++
				return map[string]any{}, nil
			}}
			invoke := func(input proto.Message) error {
				ctx := authorization.WithGrant(context.Background(), challenge.ID)
				switch v := input.(type) {
				case *pb.CreateOfferRequest:
					_, e := service.CreateOffer(ctx, v)
					return e
				case *pb.TakeOfferRequest:
					_, e := service.TakeOffer(ctx, v)
					return e
				case *pb.ConfirmTradeRequest:
					_, e := service.ConfirmTrade(ctx, v)
					return e
				}
				t.Fatal("missing service invocation")
				return nil
			}
			changed := proto.Clone(tc.input)
			tc.change(changed)
			if invoke(changed) == nil || accepted != 0 {
				t.Fatal("changed terms consumed reviewed consent")
			}
			// A mismatch burns its grant. Only a newly approved exact challenge
			// permits the original action, and that challenge is one-use too.
			challenge, err = authority.Prepare(action)
			if err != nil {
				t.Fatal(err)
			}
			if err = authority.Approve(challenge); err != nil {
				t.Fatal(err)
			}
			if err := invoke(tc.input); err != nil {
				t.Fatal(err)
			}
			if invoke(tc.input) == nil || accepted != 1 {
				t.Fatal("one-use grant replay executed")
			}
		})
	}
}
func TestPartialFillV1ConsentRejectsLossyAndUnknownInputs(t *testing.T) {
	for _, tc := range []struct{ method, raw string }{
		{"swap.take", `{"quantity":1.5}`}, {"swap.take", `{"parent_revision":"18446744073709551616"}`}, {"swap.take", `{"parent_revision":"-1"}`},
		{"offer.create", `{"fee_budgets":{"btc":"9223372036854775808"}}`}, {"offer.create", `{"bounty_budgets":{"btc":0.5}}`}, {"trade.confirm", `{"skip_consent":true}`},
	} {
		if _, err := NormalizeSensitiveAction(tc.method, json.RawMessage(tc.raw)); err == nil {
			t.Errorf("accepted invalid typed %s", tc.method)
		}
	}
	if _, err := NormalizeSensitiveAction("fills.list", json.RawMessage(`{}`)); err == nil {
		t.Fatal("read-only history became sensitive authority")
	}
}
