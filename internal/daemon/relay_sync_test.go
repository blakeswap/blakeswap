package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestRelayCursorWaitsForWholeAppliedPageAndDurableCheckpoint(t *testing.T) {
	e, _ := receiveEngine(t)
	// Inject a verified page at the network/engine boundary. Invalid mailbox
	// envelopes still consume their bounded turn and produce visible incomplete
	// coverage, while a slow relay producer cannot affect this local phase.
	e.relayCancel = func() {}
	batch := &relayHistoryBatch{page: transport.RelayPage{Events: make([]nostr.Event, 128), Next: transport.RelayCursor{Until: 123, Incomplete: "coverage unverified"}}, ack: make(chan struct{})}
	w := &relayWorker{key: "fixture\nmailbox", name: "mailbox", pages: make(chan *relayHistoryBatch, 1), live: make(chan nostr.Event, 32), states: make(chan relayLiveState, 4)}
	w.pages <- batch
	e.relayWorkers = []*relayWorker{w}
	e.s.RelaySync = map[string]RelaySyncRecord{w.key: {}}
	for i := 0; i < 2; i++ {
		e.drainRelaySync()
		if e.s.RelaySync[w.key].Cursor.Until != 0 {
			t.Fatal("cursor skipped the unfinished page")
		}
		if err := e.save(); err != nil {
			t.Fatal(err)
		}
		e.acknowledgeRelayPages()
		select {
		case <-batch.ack:
			t.Fatal("producer advanced before all page effects")
		default:
		}
	}
	for w.current != nil {
		e.drainRelaySync()
	}
	if e.s.RelaySync[w.key].Cursor.Until != 123 || e.s.RelaySync[w.key].Error == "" {
		t.Fatal("missing bounded progress or failed-event coverage")
	}
	select {
	case <-batch.ack:
		t.Fatal("producer advanced before persistence")
	default:
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.RelaySync[w.key].Cursor.Until != 123 {
		t.Fatal("cursor checkpoint missing")
	}
	e.acknowledgeRelayPages()
	select {
	case <-batch.ack:
	default:
		t.Fatal("saved page did not release producer")
	}
}

type relayTestBackend struct{ *receiveBackend }

func (*relayTestBackend) Close() error { return nil }

func TestUnavailableRelaysDoNotBlockSettlementTickAndCloseJoinsReaders(t *testing.T) {
	entered := make(chan struct{}, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	e, _ := receiveEngine(t)
	e.Config.Relays = []string{"ws" + strings.TrimPrefix(server.URL, "http")}
	for id, b := range e.nodes {
		e.nodes[id] = &relayTestBackend{b.(*receiveBackend)}
	}
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	e.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	started := time.Now()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unresponsive relay blocked local tick for %s", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fixture did not exercise a pending network read")
	}
	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close failed to cancel/join relay producers")
	}
}

func TestRelayFiltersAllMakeBoundedProgressUnderMailboxLoad(t *testing.T) {
	e, _ := receiveEngine(t)
	e.relayCancel = func() {}
	e.s.RelaySync = map[string]RelaySyncRecord{}
	for i := range 9 {
		w := &relayWorker{key: string(rune('a' + i)), pages: make(chan *relayHistoryBatch, 1), live: make(chan nostr.Event, 32), states: make(chan relayLiveState, 4)}
		w.pages <- &relayHistoryBatch{page: transport.RelayPage{Events: make([]nostr.Event, 128)}, ack: make(chan struct{})}
		for range 32 {
			w.live <- nostr.Event{}
		}
		e.relayWorkers = append(e.relayWorkers, w)
	}
	e.drainRelaySync()
	total := 0
	for _, w := range e.relayWorkers {
		if w.current == nil || w.current.offset == 0 {
			t.Fatal("busy earlier filters starved another filter")
		}
		total += w.current.offset + 32 - len(w.live)
	}
	if total > relayTickEvents {
		t.Fatal("tick exceeded its event budget", total)
	}
}
