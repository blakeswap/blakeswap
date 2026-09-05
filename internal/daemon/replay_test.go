package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"strings"
	"testing"
)

func TestRealMailboxIdempotencyAndAcknowledgmentAuthority(t *testing.T) {
	h := newHarness(t, 50)
	o := h.command("maker", "offer.create", map[string]any{"sell": "btc", "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": 50}).(protocol.Offer)
	h.tick("maker", "taker")
	id := h.command("taker", "swap.take", map[string]string{"maker": o.Maker, "id": o.ID}).(map[string]string)["id"]
	taker, maker := h.engines["taker"], h.engines["maker"]
	var pending *Delivery
	for _, d := range taker.s.Outbox {
		if !d.IsAck {
			pending = d
			break
		}
	}
	if pending == nil {
		t.Fatal("missing durable request")
	}
	if err := maker.receive(pending.Event); err != nil {
		t.Fatal(err)
	}
	if err := maker.receive(pending.Event); err != nil {
		t.Fatal(err)
	}
	if len(maker.s.Swaps) != 1 {
		t.Fatal("duplicate event created another reservation")
	}
	_, message, err := transport.Unwrap(maker.identity, pending.Event)
	if err != nil {
		t.Fatal(err)
	}
	var changed protocol.Request
	if err = json.Unmarshal(message.Body, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Hash = transport.RandomID()
	message.Body, _ = json.Marshal(changed)
	altered, err := transport.Wrap(taker.identity, maker.identity.Public(), message)
	if err != nil {
		t.Fatal(err)
	}
	if maker.receive(altered) == nil {
		t.Fatal("same message ID changed its signed contents")
	}
	ackBody, _ := json.Marshal(map[string]string{"id": pending.MessageID, "digest": pending.Digest})
	ack := transport.Message{Version: 1, ID: transport.RandomID(), Type: "ack", SwapID: id, Body: ackBody}
	forged, err := transport.Wrap(nostr.Generate(), taker.identity.Public(), ack)
	if err != nil {
		t.Fatal(err)
	}
	if err = taker.receive(forged); err != nil {
		t.Fatal(err)
	}
	if taker.s.Outbox[pending.MessageID] == nil {
		t.Fatal("third party acknowledged another party's message")
	}
	ack.ID = transport.RandomID()
	authentic, err := transport.Wrap(maker.identity, taker.identity.Public(), ack)
	if err != nil {
		t.Fatal(err)
	}
	if err = taker.receive(authentic); err != nil {
		t.Fatal(err)
	}
	if taker.s.Outbox[pending.MessageID] != nil {
		t.Fatal("recipient acknowledgment did not clear pending delivery")
	}
	status, _ := json.Marshal(taker.Status())
	for _, secret := range []string{taker.s.Mnemonic, taker.identity.Hex(), taker.s.Swaps[id].Secret} {
		if strings.Contains(string(status), secret) {
			t.Fatal("public status leaks private state")
		}
	}
}
