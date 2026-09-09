package api

import (
	"errors"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// These whole-trade scenarios deliberately retain their original principals.
// Refuse a changed current parent instead of silently rewriting chosen q or
// treating an incompatible whole amount as a current take authorization.
func wholeReviewedParent(o *pb.Offer, maker string, quantity, buy int64) error {
	if o == nil || o.Version != int32(protocol.Version) || o.Network != "regtest" || o.Maker != maker || !protocol.Hex32(o.Maker) || !protocol.Hex32(o.Id) || o.Revision == 0 || o.Status != "open" || o.FillMode != "whole" || o.MinFill != quantity || o.MaxFill != quantity || o.SellAmount != quantity || o.Available != quantity || o.BuyAmount != buy || (o.Sell != "btc" && o.Sell != "blake") {
		return errors.New("current parent differs from explicit whole-trade fixture authorization")
	}
	return nil
}
func requireWholeReviewedParent(t *testing.T, o *pb.Offer, maker string, quantity, buy int64) {
	t.Helper()
	if err := wholeReviewedParent(o, maker, quantity, buy); err != nil {
		t.Fatal(err, o)
	}
}
func TestWholeReviewedFixtureRejectsChangedOrLegacyParent(t *testing.T) {
	maker := "1111111111111111111111111111111111111111111111111111111111111111"
	o := &pb.Offer{Version: int32(protocol.Version), Id: "2222222222222222222222222222222222222222222222222222222222222222", Maker: maker, Network: "regtest", Sell: "btc", SellAmount: 1000000, BuyAmount: 2000000, FillMode: "whole", MinFill: 1000000, MaxFill: 1000000, Available: 1000000, Revision: ^uint64(0), Status: "open"}
	if err := wholeReviewedParent(o, maker, 1000000, 2000000); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*pb.Offer){
		"legacy":    func(p *pb.Offer) { p.Version = 2 },
		"maker":     func(p *pb.Offer) { p.Maker = p.Id },
		"network":   func(p *pb.Offer) { p.Network = "mainnet" },
		"mode":      func(p *pb.Offer) { p.FillMode = "partial" },
		"minimum":   func(p *pb.Offer) { p.MinFill-- },
		"maximum":   func(p *pb.Offer) { p.MaxFill++ },
		"available": func(p *pb.Offer) { p.Available-- },
		"quantity":  func(p *pb.Offer) { p.SellAmount-- },
		"buy":       func(p *pb.Offer) { p.BuyAmount++ },
		"revision":  func(p *pb.Offer) { p.Revision = 0 },
		"closed":    func(p *pb.Offer) { p.Status = "cancelled" },
	} {
		t.Run(name, func(t *testing.T) {
			p := proto.Clone(o).(*pb.Offer)
			change(p)
			if wholeReviewedParent(p, maker, 1000000, 2000000) == nil {
				t.Fatal("changed fixture authorized")
			}
		})
	}
}
