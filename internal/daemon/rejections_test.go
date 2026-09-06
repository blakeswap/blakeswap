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
			first := h.command("taker", "swap.take", map[string]any{"maker": o.Maker, "id": o.ID, "tower_bps": 50}).(map[string]string)["id"]

			if scenario == "two-takers-one-reservation" {
				raw, _ := json.Marshal(map[string]string{"maker": o.Maker, "id": o.ID})
				if _, err := h.engines["taker"].Command(h.ctx, Request{Method: "swap.take", Params: raw}); err == nil {
					t.Fatal("duplicate local reservation request accepted")
				}
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
			winner := h.swap("taker", first)
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
			if err := h.engines["taker"].handle(winner.protection().PubKey, message); err == nil {
				t.Fatal("receipt for altered template accepted")
			}
			// Mutating a bounty or locktime cannot be acknowledged as the original job.
			bad := job
			bad.Lock--
			if bad.Validate(winner.protection().Scripts, 50) == nil {
				t.Fatal("early tower job accepted")
			}
			bad = job
			bad.Payout = job.TowerScript
			if bad.Validate(winner.protection().Scripts, 50) == nil {
				t.Fatal("redirected payout accepted")
			}
		})
	}
}
