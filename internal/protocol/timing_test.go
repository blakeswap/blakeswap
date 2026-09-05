package protocol

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
	"testing"
	"time"
)

func publicSample(t *testing.T, n chain.Network, sell chain.ID) Terms {
	old := sample(t)
	maker := nostr.Generate()
	offer := old.Offer()
	offer.Network = n
	offer.Maker = maker.Public().Hex()
	offer.Sell = sell
	raw, _ := json.Marshal(offer)
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", n.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	request := old.Request
	request.OfferEvent = event
	now := uint32(time.Now().Unix())
	terms, err := NewTermsWithClocks(request, old.MakerKeys, map[chain.ID]uint32{chain.BTC: n.ForkHeight() + 1000, chain.Blake: n.ForkHeight() + 20000}, map[chain.ID]uint32{chain.BTC: now - 3600, chain.Blake: now - 1200}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return terms
}
func TestPublicTimeLocksAndAsymmetricHeights(t *testing.T) {
	for _, network := range []chain.Network{chain.Mainnet, chain.Testnet} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(string(network)+"/"+string(sell), func(t *testing.T) {
				terms := publicSample(t, network, sell)
				start := terms.Long.RefundHeight - LongSeconds
				if terms.Long.RefundHeight-terms.Short.RefundHeight != 2*24*3600 {
					t.Fatal("refund gap depends on block rate")
				}
				for _, delta := range []int64{-1, 0, 1, 3599, 3600, 7199, 7200, 7201, 43199, 43200, 43201, 86400, 172800, 345600} {
					clock := uint32(int64(start) + delta)
					clocks := map[chain.ID]uint32{chain.BTC: clock, chain.Blake: clock}
					for _, phase := range []string{"fund-long", "fund-short", "reveal"} {
						err := terms.timeGateAt(phase, clocks, int64(clock))
						if err == nil && (clock >= terms.Short.RefundHeight || clock >= terms.Long.RefundHeight || (phase == "reveal" && clock >= terms.RevealBefore)) {
							t.Fatalf("unsafe %s at %d", phase, delta)
						}
					}
				}
				clocks := map[chain.ID]uint32{chain.BTC: start, chain.Blake: start + MaxClockSkew + 1}
				if terms.Gate("fund-long", clocks) == nil {
					t.Fatal("clock skew accepted")
				}
				bad := terms
				bad.Short.RefundHeight = bad.Long.RefundHeight
				if bad.Validate() == nil {
					t.Fatal("equal refund deadlines accepted")
				}
				bad = terms
				bad.RevealBefore++
				if bad.Validate() == nil {
					t.Fatal("changed reveal cutoff accepted")
				}

			})
		}
	}
}
func TestPublicTimingBoundaries(t *testing.T) {
	terms := publicSample(t, chain.Mainnet, chain.BTC)
	start := terms.Long.RefundHeight - LongSeconds
	for _, tt := range []struct {
		phase   string
		delta   uint32
		allowed bool
	}{
		{"fund-long", 7200, true}, {"fund-long", 7201, false},
		{"fund-short", RevealSeconds - 7200, true}, {"fund-short", RevealSeconds - 7199, false},
		{"reveal", RevealSeconds - 1, true}, {"reveal", RevealSeconds, false},
	} {
		clock := start + tt.delta
		err := terms.timeGateAt(tt.phase, map[chain.ID]uint32{chain.BTC: clock, chain.Blake: clock}, int64(clock))
		if (err == nil) != tt.allowed {
			t.Fatalf("%s at +%d: %v", tt.phase, tt.delta, err)
		}
	}
	clocks := map[chain.ID]uint32{chain.BTC: start, chain.Blake: start}
	if terms.timeGateAt("fund-long", clocks, int64(start)+6*3600+1) == nil {
		t.Fatal("stale chain accepted")
	}
	if terms.timeGateAt("fund-long", clocks, int64(start)-2*3600-1) == nil {
		t.Fatal("future chain accepted")
	}
	future := terms
	future.Long.RefundHeight += 10 * 24 * 3600
	future.Short.RefundHeight += 10 * 24 * 3600
	future.Takeover += 10 * 24 * 3600
	future.RevealBefore += 10 * 24 * 3600
	if future.Validate() != nil {
		t.Fatal("fixture is not structurally valid")
	}
	if future.timeGateAt("fund-long", clocks, int64(start)) == nil {
		t.Fatal("maker locked taker for arbitrarily long period")
	}
}
func TestSignedOfferCannotCrossNetworkNamespace(t *testing.T) {
	terms := publicSample(t, chain.Mainnet, chain.Blake)
	key := nostr.Generate()
	offer := terms.Offer()
	offer.Maker = key.Public().Hex()
	raw, _ := json.Marshal(offer)
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", chain.Testnet.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, key); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOffer(event, time.Now().Unix()); err == nil {
		t.Fatal("valid signature bypassed network binding")
	}
}
