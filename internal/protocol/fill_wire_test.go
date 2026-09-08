package protocol

import (
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func partialTerms(t *testing.T) Terms {
	t.Helper()
	original := sample(t)
	maker := nostr.Generate()
	o := original.Offer()
	o.Maker, o.BuyAmount = maker.Public().Hex(), 333333
	o.FillPolicy = FillPolicy{Mode: FillPartial, Min: 100000, Max: 700000}
	o.Revision = 7
	raw, _ := o.PublicJSON()
	event := original.Request.OfferEvent
	event.Content = string(raw)
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	r := original.Request
	r.OfferEvent, r.Quantity, r.Revision = event, 300000, o.Revision
	terms, err := NewTerms(r, original.MakerKeys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return terms
}

func TestPartialTermsBindExactQuantityRevisionAndRoundedAmounts(t *testing.T) {
	terms := partialTerms(t)
	if terms.Long.Amount != 100000 || terms.Short.Amount != 300000 || terms.Offer().SellAmount != 1000000 {
		t.Fatal("child contract is not the exact slice of an unchanged parent")
	}
	for name, mutate := range map[string]func(*Terms){
		"quantity":          func(x *Terms) { x.Request.Quantity++ },
		"revision":          func(x *Terms) { x.Request.Revision++ },
		"request version":   func(x *Terms) { x.Request.Version = 1 },
		"terms version":     func(x *Terms) { x.Version = 1 },
		"parent buy":        func(x *Terms) { x.Long.Amount = x.Offer().BuyAmount },
		"parent sell":       func(x *Terms) { x.Short.Amount = x.Offer().SellAmount },
		"sibling hash":      func(x *Terms) { x.Request.Hash = transport.RandomID() },
		"sibling taker key": func(x *Terms) { x.Request.Keys[chain.BTC] = x.MakerKeys[chain.BTC] },
	} {
		t.Run(name, func(t *testing.T) {
			encoded, _ := json.Marshal(terms)
			var changed Terms
			_ = json.Unmarshal(encoded, &changed)
			mutate(&changed)
			if changed.Validate() == nil {
				t.Fatal("changed child terms accepted")
			}
		})
	}
}

func TestSignedFillOfferRejectsLegacyAndInconsistentAvailability(t *testing.T) {
	terms := partialTerms(t)
	maker := nostr.Generate()
	o := terms.Offer()
	o.Maker = maker.Public().Hex()
	for name, mutate := range map[string]func(map[string]any){
		"missing version":    func(m map[string]any) { delete(m, "version") },
		"old version":        func(m map[string]any) { m["version"] = 1 },
		"missing fill mode":  func(m map[string]any) { delete(m, "fill_mode") },
		"missing revision":   func(m map[string]any) { delete(m, "revision") },
		"legacy reservation": func(m map[string]any) { m["reservation"] = transport.RandomID() },
		"invalid tail":       func(m map[string]any) { m["available"] = 200000 },
		"oversold":           func(m map[string]any) { m["available"] = -1 },
		"closed available":   func(m map[string]any) { m["status"] = "cancelled" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := o.PublicJSON()
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			mutate(fields)
			raw, _ = json.Marshal(fields)
			event := terms.Request.OfferEvent
			event.Content = string(raw)
			if err := transport.Sign(&event, maker); err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeOffer(event, time.Now().Unix()); err == nil {
				t.Fatal("incompatible signed order accepted")
			}
		})
	}
	before := o.EconomicsDigest()
	o.Available, o.Revision = 700000, 8
	if o.EconomicsDigest() != before {
		t.Fatal("availability changed immutable economic identity")
	}
	o.BuyAmount++
	if o.EconomicsDigest() == before {
		t.Fatal("price change preserved economic identity")
	}
}

func TestTowerReceiptRequiresCurrentProtocolMarker(t *testing.T) {
	r := Receipt{Version: Version, JobID: transport.RandomID(), Digest: transport.RandomID()}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 1, 3} {
		r.Version = version
		if r.Validate() == nil {
			t.Fatal("incompatible receipt accepted")
		}
	}
}
