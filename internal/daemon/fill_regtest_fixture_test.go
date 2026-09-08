package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Execute the real command/encrypted-message/archive boundary with generated
// wallet keys and injected chain inputs. This is not an actual-chain test.
func TestPartialMatrixRejectionSurvivesColdPlacement(t *testing.T) {
	maker, taker := actualInputEngine(t), actualInputEngine(t)
	ctx := context.Background()
	raw, _ := json.Marshal(walletWholeParams(chain.BTC, 1000000, 2000000, 2000, 0, 0))
	result, err := maker.Command(ctx, Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	o := result.(protocol.Offer)
	taker.s.Book[o.Maker+":"+o.ID] = maker.s.Offers[o.ID]
	raw, _ = json.Marshal(map[string]any{"maker": o.Maker, "id": o.ID, "quantity": o.SellAmount, "parent_revision": o.Revision})
	result, err = taker.Command(ctx, Request{Method: "swap.take", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]string)["id"]
	expected := taker.s.Swaps[id].Request
	if rejected, err := partialRejectedRequest(taker, expected); err != nil || rejected {
		t.Fatal("pending request counted as rejected", err)
	}
	raw, _ = json.Marshal(map[string]string{"id": o.ID})
	if _, err := maker.Command(ctx, Request{Method: "offer.cancel", Params: raw}); err != nil {
		t.Fatal(err)
	}
	for _, d := range taker.s.Outbox {
		if d.Type == "request" {
			if err := maker.receive(d.Event); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range maker.s.Outbox {
		if d.Type == "rejected" {
			if err := taker.receive(d.Event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rejected, err := partialRejectedRequest(taker, expected); err != nil || !rejected {
		t.Fatal("authenticated hot rejection unavailable", err)
	}
	if err := taker.compactArchive(ctx, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := taker.save(); err != nil {
		t.Fatal(err)
	}
	if taker.s.Swaps[id] != nil {
		t.Fatal("unfunded rejected request did not archive")
	}
	var cold Swap
	if found, err := taker.archivedValue("swaps", id, &cold); err != nil || !found || protocol.Digest(cold.Request) != protocol.Digest(expected) || cold.Stage != "rejected" {
		t.Fatal("exact authenticated rejection was not retained", err)
	}
	before := protocol.Digest(taker.s)
	if rejected, err := partialRejectedRequest(taker, expected); err != nil || !rejected {
		t.Fatal("matrix predicate lost the same retained rejection after archival", err)
	}
	if protocol.Digest(taker.s) != before || taker.s.Swaps[id] != nil {
		t.Fatal("rejection lookup reactivated or changed wallet authority")
	}
	for _, failure := range []string{"missing", "unreadable", "malformed", "wrong-id", "wrong-quantity", "wrong-role", "accepted", "funded"} {
		t.Run(failure, func(t *testing.T) {
			reads := 0
			taker.archiveRead = func(kind, key string) (storage.ArchiveRecord, bool, error) {
				reads++
				if kind != "swaps" || key != expected.ID {
					t.Fatal("lookup escaped exact request identity")
				}
				if failure == "missing" {
					return storage.ArchiveRecord{}, false, nil
				}
				if failure == "unreadable" {
					return storage.ArchiveRecord{}, false, errors.New("cold source unavailable")
				}
				value := cold
				switch failure {
				case "wrong-id":
					value.ID = "another-request"
				case "wrong-quantity":
					value.Request.Quantity++
				case "wrong-role":
					value.Role = "maker"
				case "accepted":
					value.Terms = &protocol.Terms{}
				case "funded":
					value.LongSent = true
				}
				data, _ := json.Marshal(value)
				if failure == "malformed" {
					data = []byte(`{"broken"`)
				}
				return storage.ArchiveRecord{Kind: kind, ID: key, Data: data}, true, nil
			}
			defer func() { taker.archiveRead = nil }()
			if rejected, err := partialRejectedRequest(taker, expected); err == nil || rejected {
				t.Fatal("invalid cold evidence became successful rejection", err)
			}
			if reads != 1 || protocol.Digest(taker.s) != before || taker.s.Swaps[id] != nil {
				t.Fatal("failed exact read changed custody or traversed history")
			}
		})
	}
}
