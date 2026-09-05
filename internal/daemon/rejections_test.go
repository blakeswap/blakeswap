package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"testing"
)

func TestRealCancellationReservationAndReceiptBinding(t *testing.T) {
	for _, scenario := range []string{"cancel-before-request", "two-takers-one-reservation"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, 50)
			o := h.command("maker", "offer.create", map[string]any{"sell": "btc", "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": 50}).(protocol.Offer)
			h.tick("maker", "taker")
			if scenario == "cancel-before-request" {
				h.command("maker", "offer.cancel", map[string]string{"id": o.ID})
			}
			first := h.command("taker", "swap.take", map[string]string{"maker": o.Maker, "id": o.ID}).(map[string]string)["id"]
			second := ""
			if scenario == "two-takers-one-reservation" {
				second = h.command("taker", "swap.take", map[string]string{"maker": o.Maker, "id": o.ID}).(map[string]string)["id"]
			}
			h.tick("taker", "maker", "taker")
			if scenario == "cancel-before-request" {
				if len(h.engines["maker"].s.Swaps) != 0 || h.swap("taker", first).Stage != "rejected" {
					t.Fatal("cancelled order executed")
				}
				return
			}
			if len(h.engines["maker"].s.Swaps) != 1 {
				t.Fatal("offer reserved more than once")
			}
			one, two := h.swap("taker", first), h.swap("taker", second)
			if (one.Stage == "rejected") == (two.Stage == "rejected") {
				t.Fatal("expected exactly one rejected request")
			}
			winner := one
			if winner.Stage == "rejected" {
				winner = two
			}
			if winner.LongSent {
				t.Fatal("funded without durable tower receipt")
			}
			if len(winner.Jobs) != 1 {
				t.Fatal("missing refund protection")
			}
			job := winner.Jobs[0]
			receipt := protocol.Receipt{JobID: job.ID, Digest: protocol.Digest(job)}
			raw, _ := json.Marshal(receipt)
			message := transport.Message{Version: 1, ID: transport.RandomID(), Type: "tower-receipt", SwapID: winner.ID, Body: raw}
			if err := h.engines["taker"].handle(nostr.Generate().Public().Hex(), message); err == nil {
				t.Fatal("receipt accepted from wrong tower")
			}
			receipt.Digest = transport.RandomID()
			message.Body, _ = json.Marshal(receipt)
			if err := h.engines["taker"].handle(winner.Terms.Tower, message); err == nil {
				t.Fatal("receipt for altered template accepted")
			}
			// Mutating a bounty or locktime cannot be acknowledged as the original job.
			bad := job
			bad.Lock--
			if bad.Validate(winner.Terms.TowerScripts, 50) == nil {
				t.Fatal("early tower job accepted")
			}
			bad = job
			bad.Payout = job.TowerScript
			if bad.Validate(winner.Terms.TowerScripts, 50) == nil {
				t.Fatal("redirected payout accepted")
			}
		})
	}
}
