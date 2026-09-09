package api

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/transport"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type nativeRegtestOwner struct {
	locked    atomic.Bool
	reads     atomic.Int64
	done      chan struct{}
	authority *authorization.Authority
}

// Only generated fixture wallet credentials are acquired. Native mode is entered
// before any reviewed offer/send and then reopened from the same encrypted vault.
func nativeReviewedFixture(t *testing.T) (*reviewedTradeFixture, map[string]*nativeRegtestOwner) {
	t.Helper()
	h := newReviewedTradeFixture(t)
	owners := map[string]*nativeRegtestOwner{}
	for _, name := range []string{"maker", "taker"} {
		cfg := h.configs[name]
		original := h.status(name).Pubkey
		password, err := os.ReadFile(cfg.PasswordFile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { clear(password) })
		a, err := authorization.New(transport.RandomID(), nil)
		if err != nil {
			t.Fatal(err)
		}
		owner := &nativeRegtestOwner{done: make(chan struct{}), authority: a}
		if err := a.BindLifetime(owner.done); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		cfg.CredentialMode = "native"
		cfg.Authorization = a
		cfg.Installation = transport.RandomID()
		cfg.Credential = credential.SourceFunc(func(ctx context.Context) ([]byte, error) {
			owner.reads.Add(1)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if owner.locked.Load() {
				return nil, credential.ErrLocked
			}
			return bytes.Clone(password), nil
		})
		oldFile := cfg.PasswordFile
		cfg.PasswordFile = ""
		h.configs[name] = cfg
		h.restart(name)
		h.waitReady(name)
		if h.status(name).Pubkey != original {
			t.Fatal("credential-provider transition changed generated wallet identity")
		}
		if err := os.Remove(oldFile); err != nil {
			t.Fatal(err)
		}
		owners[name] = owner
	}
	return h, owners
}

func nativeApproved(t *testing.T, h *reviewedTradeFixture, name, method string, input proto.Message) context.Context {
	t.Helper()
	raw, err := protojson.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeSensitiveAction(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	action, err := h.engines[name].AuthorizationAction(daemon.Request{Method: method, Params: normalized})
	if err != nil {
		t.Fatal(err)
	}
	a := h.configs[name].Authorization
	challenge, err := a.Prepare(action)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Approve(challenge); err != nil {
		t.Fatal(err)
	}
	return metadata.AppendToOutgoingContext(h.contexts[name], "x-blakeswap-consent", challenge.ID)
}

func nativeConfirm(t *testing.T, h *reviewedTradeFixture, name string, q *pb.TradeQuote) (string, *pb.ConfirmTradeRequest) {
	t.Helper()
	request := &pb.ConfirmTradeRequest{Token: q.Token, Revision: q.Revision, RequestId: transport.RandomID(), ExpectedWallet: name, ExpectedNetwork: "regtest"}
	if _, err := h.clients[name].ConfirmTrade(h.contexts[name], request); err == nil {
		t.Fatal("bearer alone authorized a new trade")
	}
	approved := nativeApproved(t, h, name, "trade.confirm", request)
	changed := proto.Clone(request).(*pb.ConfirmTradeRequest)
	changed.Revision = transport.RandomID()
	if _, err := h.clients[name].ConfirmTrade(approved, changed); err == nil {
		t.Fatal("changed reviewed terms consumed native permission")
	}
	if _, err := h.clients[name].ConfirmTrade(approved, request); err == nil {
		t.Fatal("mismatched consumption left native permission reusable")
	}
	got, err := h.clients[name].ConfirmTrade(nativeApproved(t, h, name, "trade.confirm", request), request)
	if err != nil || got.GetState() != "accepted" {
		t.Fatal("authorized native trade", got, err)
	}
	retry, err := h.clients[name].ConfirmTrade(h.contexts[name], request)
	if err != nil || !proto.Equal(got, retry) {
		t.Fatal("durable accepted receipt required fresh permission", retry, err)
	}
	return got.Id, request
}

func TestRealNativeRevocationRetainsFundedSettlement(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		sell   chain.ID
		refund bool
	}{
		{"claim-sell-btc", chain.BTC, false}, {"claim-sell-blake", chain.Blake, false}, {"refund", chain.BTC, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, owners := nativeReviewedFixture(t)
			mq := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", FillMode: "whole", MinFill: 1000000, MaxFill: 1000000, FeeBudgets: map[string]int64{string(scenario.sell): 26500, string(scenario.sell.Other()): 20000}, BountyBudgets: map[string]int64{"btc": 0, "blake": 0}, Sell: string(scenario.sell), SellAmount: 1000000, BuyAmount: 2000000, FundingFee: 6500, OwnerFeeCap: 20000})
			offerID, mrequest := nativeConfirm(t, h, "maker", mq)
			h.tick()
			var order *pb.Offer
			for _, o := range h.status("taker").Orders {
				if o.Id == offerID {
					order = o
				}
			}
			if order == nil {
				t.Fatal("authorized maker offer was not acknowledged by private relay")
			}
			requireWholeReviewedParent(t, order, h.status("maker").Pubkey, 1000000, 2000000)
			tq := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: order.Maker, Id: order.Id, Sell: order.Sell, Quantity: 1000000, ParentRevision: order.Revision, FundingFee: 6500, OwnerFeeCap: 20000})
			swapID, trequest := nativeConfirm(t, h, "taker", tq)
			find := func(name string) *pb.Swap {
				for _, s := range h.status(name).Swaps {
					if s.Id == swapID {
						return s
					}
				}
				return nil
			}
			funded := false
			for i := 0; i < 20; i++ {
				h.tick()
				if s := find("maker"); s != nil && s.Short.Txid != "" {
					if _, err := h.nodes[scenario.sell].Transaction(h.ctx, s.Short.Txid); err == nil {
						funded = true
						break
					}
				}
				h.minePending()
			}
			if !funded {
				t.Fatal("native-authorized trade never funded", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			accepted := proto.Clone(find("maker")).(*pb.Swap)
			reads := map[string]int64{}
			for name, owner := range owners {
				reads[name] = owner.reads.Load()
				owner.locked.Store(true)
				close(owner.done)
				if _, err := h.clients[name].GetRecovery(h.contexts[name], &emptypb.Empty{}); err == nil {
					t.Fatal("revoked owner disclosed recovery")
				}
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
				for _, name := range []string{"maker", "taker"} {
					s := find(name)
					if s == nil || s.Stage != want || s.LongConfirmations < 2 || s.ShortConfirmations < 2 {
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
				t.Fatal("revoked new consent blocked funded settlement", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			for _, name := range []string{"maker", "taker"} {
				s := find(name)
				if s.Long.Txid != accepted.Long.Txid || s.Short.Txid != accepted.Short.Txid || s.Long.Amount != 2000000 || s.Short.Amount != 1000000 || s.FundingFee != 6500 || s.OwnerFeeCap != 20000 {
					t.Fatal("revocation changed accepted terms", s)
				}
				if scenario.refund && s.SecretRevealed {
					t.Fatal("refund path unexpectedly revealed secret")
				}
				incoming, own := s.Long, s.Short
				if name == "taker" {
					incoming, own = s.Short, s.Long
				}
				spend, principal, id := s.ClaimTxid, incoming.Amount, chain.ID(incoming.Chain)
				if scenario.refund {
					spend, principal, id = s.RefundTxid, own.Amount, chain.ID(own.Chain)
				}
				if fee := actualStrategyFee(t, h, chain.ID(own.Chain), own.Txid, own.Amount); fee != 6500 {
					t.Fatal("actual funding fee changed", fee)
				}
				fee := actualStrategyFee(t, h, id, spend, 0)
				record, err := h.nodes[id].Transaction(h.ctx, spend)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := contract.Parse(record.Hex)
				if err != nil {
					t.Fatal(err)
				}
				if len(tx.TxOut) != 1 || tx.TxOut[0].Value != principal-fee || fee > 20000 {
					t.Fatal("actual settlement differs from accepted principal/cap")
				}
				if owners[name].reads.Load() != reads[name] {
					t.Fatal("settlement reacquired a locked credential")
				}
				request := mrequest
				if name == "taker" {
					request = trequest
				}
				got, err := h.clients[name].ConfirmTrade(h.contexts[name], request)
				if err != nil || got.GetState() != "accepted" {
					t.Fatal("accepted receipt identity lost under revocation", got, err)
				}
				owners[name].locked.Store(false) // A later explicit initial unlock permits reopening.
				h.restart(name)
				got, err = h.clients[name].ConfirmTrade(h.contexts[name], request)
				if err != nil || got.GetState() != "accepted" {
					t.Fatal("restart lost accepted receipt", got, err)
				}
				t.Logf("%s %s native authorization revoked before settlement; principal=%d actual fee=%d, exact funding and accepted receipt preserved", scenario.name, name, principal, fee)
			}
		})
	}
}
