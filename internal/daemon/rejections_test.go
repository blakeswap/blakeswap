package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"testing"
)

func TestRealCancellationReservationAndReceiptBinding(t *testing.T) {
	for _, scenario := range []string{"cancel-before-request", "two-takers-one-reservation"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, 50)
			o := h.command("maker", "offer.create", walletWholeParams(chain.BTC, 1000000, 2000000, 2000, 0, 50)).(protocol.Offer)
			h.tick("maker", "taker")
			if scenario == "cancel-before-request" {
				h.command("maker", "offer.cancel", map[string]string{"id": o.ID})
			}
			first := h.command("taker", "swap.take", map[string]any{"maker": o.Maker, "id": o.ID, "quantity": o.SellAmount, "parent_revision": o.Revision, "tower_bps": 50}).(map[string]string)["id"]
			request := h.swap("taker", first).Request

			if scenario == "two-takers-one-reservation" {
				raw, _ := json.Marshal(map[string]any{"maker": o.Maker, "id": o.ID, "quantity": o.SellAmount, "parent_revision": o.Revision})
				if _, err := h.engines["taker"].Command(h.ctx, Request{Method: "swap.take", Params: raw}); err == nil {
					t.Fatal("duplicate local reservation request accepted")
				}
			}
			if scenario == "cancel-before-request" {
				partialWaitMailbox(h, "cancelled request retained as rejected", func() bool {
					rejected, err := partialRejectedRequest(h.engines["taker"], request)
					if err != nil {
						t.Fatal(err)
					}
					return rejected
				}, func() { h.tick("taker", "maker") })
				if len(h.engines["maker"].s.Swaps) != 0 {
					t.Fatal("cancelled order executed")
				}
				return
			}
			partialWaitMailbox(h, "accepted request has refund protection", func() bool {
				winner := h.swap("taker", first)
				return winner.Terms != nil && len(winner.Jobs) == 1
			}, func() { h.tick("taker", "maker") })
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
			validReceipt := protocol.Receipt{Version: protocol.Version, JobID: job.ID, Digest: protocol.Digest(job)}
			if err := validReceipt.Validate(); err != nil {
				t.Fatal("invalid receipt fixture", err)
			}
			receipt := validReceipt
			raw, _ := json.Marshal(receipt)
			message := transport.Message{Version: protocol.Version, ID: transport.RandomID(), Type: "tower-receipt", SwapID: winner.ID, Body: raw}
			if err := h.engines["taker"].handle(nostr.Generate().Public().Hex(), message); err == nil || err.Error() != "receipt from unselected tower" {
				t.Fatal("wrong-tower receipt did not reach sender binding", err)
			}
			receipt.Digest = transport.RandomID()
			message.Body, _ = json.Marshal(receipt)
			if err := h.engines["taker"].handle(winner.protection().PubKey, message); err == nil || err.Error() != "receipt does not commit to a requested job" {
				t.Fatal("altered-template receipt did not reach digest binding", err)
			}
			if len(winner.Receipts) != 0 {
				t.Fatal("rejected receipt granted funding authority")
			}
			message.Body, _ = json.Marshal(validReceipt)
			if err := h.engines["taker"].handle(winner.protection().PubKey, message); err != nil || winner.Receipts[job.ID] != validReceipt {
				t.Fatal("matching selected-tower receipt was not retained", err)
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
