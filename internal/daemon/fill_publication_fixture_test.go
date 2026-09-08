package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/relay"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestPartialMatrixAcceptanceRequiresRelayPublication(t *testing.T) {
	maker, taker := actualInputEngine(t), actualInputEngine(t)
	ctx := context.Background()
	raw, _ := json.Marshal(walletWholeParams(chain.BTC, 1000000, 2000000, 2000, 0, 0))
	result, err := maker.Command(ctx, Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	taker.s.Book[offer.Maker+":"+offer.ID] = maker.s.Offers[offer.ID]
	raw, _ = json.Marshal(map[string]any{"maker": offer.Maker, "id": offer.ID, "quantity": offer.SellAmount, "parent_revision": offer.Revision})
	result, err = taker.Command(ctx, Request{Method: "swap.take", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]string)["id"]
	for _, delivery := range taker.s.Outbox {
		if delivery.Type == "request" {
			if err := maker.receive(delivery.Event); err != nil {
				t.Fatal(err)
			}
		}
	}
	var recordID string
	for key, delivery := range maker.s.Outbox {
		if delivery.Type == "accepted" && delivery.SwapID == id {
			recordID = key
		}
	}
	if recordID == "" || maker.s.Swaps[id] == nil || maker.s.Swaps[id].Terms == nil {
		t.Fatal("missing current durable acceptance")
	}
	expected, err := partialAcceptancePublications(maker, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	eventID := maker.s.Outbox[recordID].Event.ID.Hex()
	terms := protocol.Digest(maker.s.Swaps[id].Terms)
	bins := protocol.Digest(maker.s.ParentOrders[offer.ID].Quantities)
	localRelay, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-release:
			localRelay.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))
	defer localRelay.Close()
	defer server.Close()
	workerCtx, cancel := context.WithCancel(ctx)
	maker.Config.Relays = []string{"ws" + strings.TrimPrefix(server.URL, "http")}
	maker.relayCancel = cancel
	maker.startPublicationWorkers(workerCtx)
	maker.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	maker.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	defer func() {
		maker.nodes = nil
		if err := maker.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := maker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("publication worker did not reach gated relay")
	}
	var saved State
	if found, err := maker.vault.Load(&saved); err != nil || !found || saved.Outbox[recordID] == nil || saved.Outbox[recordID].Event.ID.Hex() != eventID || protocol.Digest(saved.Swaps[id].Terms) != terms || protocol.Digest(saved.ParentOrders[offer.ID].Quantities) != bins {
		t.Fatal("dispatch changed exact durable acceptance", err)
	}
	if ready, err := partialPublicationsPublished(maker, expected); err != nil || ready || saved.Outbox[recordID].Published {
		t.Fatal("scheduled publication counted as relay acknowledgement", err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := maker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		ready, err := partialPublicationsPublished(maker, expected)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real relay publication did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	if found, err := maker.vault.Load(&saved); err != nil || !found || saved.Outbox[recordID] == nil || !saved.Outbox[recordID].Published || saved.Outbox[recordID].Acknowledged || saved.Outbox[recordID].Event.ID.Hex() != eventID || protocol.Digest(saved.Swaps[id].Terms) != terms || protocol.Digest(saved.ParentOrders[offer.ID].Quantities) != bins || taker.s.Swaps[id].Terms != nil {
		t.Fatal("positive relay result changed offline recipient or durable acceptance", err)
	}
	for _, failure := range []string{"missing", "pending", "retired", "recipient-ack", "event", "child", "recipient", "terms"} {
		t.Run(failure, func(t *testing.T) {
			before := protocol.Digest(maker.s)
			original := maker.s.Outbox[recordID]
			altered := *original
			maker.s.Outbox[recordID] = &altered
			bad := make(map[string]partialPublication, len(expected))
			for key, value := range expected {
				bad[key] = value
			}
			want := bad[recordID]
			switch failure {
			case "missing":
				delete(maker.s.Outbox, recordID)
			case "pending":
				altered.Published = false
			case "retired":
				altered.Retired = true
			case "recipient-ack":
				altered.Acknowledged = true
			case "event":
				want.event = "changed"
			case "child":
				want.child = "another-child"
			case "recipient":
				want.recipient = "another-recipient"
			case "terms":
				want.terms = "changed"
			}
			bad[recordID] = want
			ready, err := partialPublicationsPublished(maker, bad)
			maker.s.Outbox[recordID] = original
			if ready || (failure == "pending" && err != nil) || (failure != "pending" && err == nil) || protocol.Digest(maker.s) != before {
				t.Fatal("invalid publication evidence accepted or mutated custody", err)
			}
		})
	}
	t.Run("resumed-history-before-funding", func(t *testing.T) {
		// The exact acceptance is already stored while the taker is offline.
		// Its persisted cursor predates that gift wrap, so live-only reads and
		// the resumed first sweep cannot deliver it. Do not reset the cursor.
		taker.Config.Relays = maker.Config.Relays
		key := maker.Config.Relays[0] + "\nmailbox"
		taker.s.RelaySync = map[string]RelaySyncRecord{key: {
			Relay: maker.Config.Relays[0], Filter: "mailbox",
			Cursor: transport.RelayCursor{Until: maker.s.Outbox[recordID].Event.CreatedAt - 1, Limit: transport.RelayPageSize},
		}}
		if err := taker.save(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			taker.nodes = nil
			if err := taker.Close(); err != nil {
				t.Error(err)
			}
		}()
		h := &harness{t: t, engines: map[string]*Engine{"taker": taker}}
		tick := func() {
			taker.drainRelaySync()
			if err := taker.save(); err != nil {
				t.Fatal(err)
			}
			taker.acknowledgeRelayPages()
		}
		partialWait(h, "resumed history reaches its old boundary", func() bool {
			return taker.s.RelaySync[key].Cursor.Sweeps == 1
		}, tick)
		if ready, err := partialPublicationsReceived(taker, expected); err != nil || ready || taker.s.Swaps[id].Terms != nil {
			t.Fatal("old cursor or live-only subscription falsely delivered stored acceptance", err)
		}
		partialWaitForPublications(h, "taker", expected, tick)
		if protocol.Digest(taker.s.Swaps[id].Terms) != terms || taker.s.Swaps[id].LongSent {
			t.Fatal("mailbox catch-up changed terms or performed funding")
		}
	})

}
