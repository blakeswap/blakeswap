package protocol

import (
	"encoding/hex"
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
	"testing"
	"time"
)

func sample(t testing.TB) Terms {
	t.Helper()
	maker, taker := nostr.Generate(), nostr.Generate()
	keys := func() map[chain.ID]string {
		m := map[chain.ID]string{}
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			k, e := btcec.NewPrivateKey()
			if e != nil {
				t.Fatal(e)
			}
			m[id] = hex.EncodeToString(k.PubKey().SerializeCompressed())
		}
		return m
	}
	offer := Offer{ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 1000000, BuyAmount: 2000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	raw, _ := json.Marshal(offer)
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", transport.Namespace}}, Content: string(raw)}
	if e := transport.Sign(&event, maker); e != nil {
		t.Fatal(e)
	}
	r := Request{ID: transport.RandomID(), OfferEvent: event, Taker: taker.Public().Hex(), Hash: transport.RandomID(), Keys: keys()}
	terms, e := NewTerms(r, keys(), map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 1000}, "", nil)
	if e != nil {
		t.Fatal(e)
	}
	return terms
}
func TestTermsBindEveryContractField(t *testing.T) {
	terms := sample(t)
	changes := map[string]func(*Terms){"price": func(t *Terms) { t.Long.Amount++ }, "asset": func(t *Terms) { t.Short.Chain = chain.Blake }, "secret hash": func(t *Terms) { t.Long.Hash = transport.RandomID() }, "refund key": func(t *Terms) { t.Long.RefundKey = t.Long.ClaimKey }, "early bounty": func(t *Terms) { t.Takeover-- }, "late reveal": func(t *Terms) { t.RevealBefore++ }, "replay domain": func(t *Terms) { t.Domains[chain.Blake] = chain.BTC.Domain() }, "preset outpoint": func(t *Terms) { t.Short.TxID = transport.RandomID() }, "protocol version": func(t *Terms) { t.Version = 2 }}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(terms)
			var copy Terms
			_ = json.Unmarshal(raw, &copy)
			change(&copy)
			if copy.Validate() == nil {
				t.Fatal("unsafe mutation accepted")
			}
		})
	}
}
func TestDeadlineBoundaryGrid(t *testing.T) {
	terms := sample(t)
	for longAdvance := uint32(0); longAdvance <= 110; longAdvance++ {
		for shortAdvance := uint32(0); shortAdvance <= 60; shortAdvance++ {
			heights := map[chain.ID]uint32{chain.Blake: 1000 + longAdvance, chain.BTC: 100 + shortAdvance}
			for _, phase := range []string{"fund-long", "fund-short", "reveal"} {
				if terms.Gate(phase, heights) != nil {
					continue
				}
				if heights[chain.Blake] >= terms.Long.RefundHeight || heights[chain.BTC] >= terms.Short.RefundHeight {
					t.Fatal("accepted after refund deadline")
				}
				if phase == "reveal" && (heights[chain.Blake] >= terms.RevealBefore || heights[chain.BTC]+16 > terms.Short.RefundHeight || heights[chain.Blake]+48 > terms.Long.RefundHeight) {
					t.Fatal("secret revealed outside safety window")
				}
			}
		}
	}
	if terms.Gate("unknown", map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 1000}) == nil {
		t.Fatal("unknown phase accepted")
	}
}
func TestBountyRoundingAndEconomicBounds(t *testing.T) {
	if Bounty(120001, 50) != 601 {
		t.Fatal("bounty must round up to whole satoshis")
	}
	if Bounty(10000000000, 1000) != 1000000000 {
		t.Fatal("percentage overflow")
	}
	o := sample(t).Offer()
	o.TowerBPS = 50
	o.SellAmount = 100000
	if o.Validate(time.Now().Unix()) == nil {
		t.Fatal("dust bounty accepted")
	}
	o.SellAmount = 120000
	if e := o.Validate(time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
}
