package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/transport"
)

const relayTickEvents = 64
const relayLiveQueue = 32

// Progress is independent for each exact relay/filter. EOSE is never treated as
// exhaustive historical coverage. These advisory cursors are not backup facts.
type RelaySyncRecord struct {
	Relay      string                `json:"relay"`
	Filter     string                `json:"filter"`
	Cursor     transport.RelayCursor `json:"cursor"`
	ObservedAt int64                 `json:"observed_at"`
	Live       bool                  `json:"live"`
	Error      string                `json:"error,omitempty"`
}
type relayHistoryBatch struct {
	page     transport.RelayPage
	err      error
	ack      chan struct{}
	offset   int
	rejected string
}
type relayLiveState struct {
	ready bool
	err   string
}
type relayWorker struct {
	key, url, name string
	pages          chan *relayHistoryBatch
	live           chan nostr.Event
	states         chan relayLiveState
	current        *relayHistoryBatch
}

func relayDelay(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *Engine) startRelaySync() {
	if e.relayCancel != nil || e.activityClosed || len(e.Config.Relays) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.relayCancel = cancel
	e.startPublicationWorkers(ctx)
	if e.s.RelaySync == nil {
		e.s.RelaySync = map[string]RelaySyncRecord{}
	}
	filters := []struct {
		name   string
		filter nostr.Filter
	}{
		{"mailbox", nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {e.identity.Public().Hex()}}}},
		{"offers", nostr.Filter{Kinds: []nostr.Kind{transport.OfferKind}, Tags: nostr.TagMap{"t": {e.Config.Network.Namespace()}}}},
		{"towers", nostr.Filter{Kinds: []nostr.Kind{transport.TowerKind}, Tags: nostr.TagMap{"t": {e.Config.Network.Namespace()}}}},
	}
	for _, spec := range filters {
		for _, url := range e.Config.Relays {
			key := url + "\n" + spec.name
			record := e.s.RelaySync[key]
			record.Relay, record.Filter, record.Live = url, spec.name, false
			e.s.RelaySync[key] = record
			w := &relayWorker{key: key, url: url, name: spec.name, pages: make(chan *relayHistoryBatch, 1), live: make(chan nostr.Event, relayLiveQueue), states: make(chan relayLiveState, 4)}
			e.relayWorkers = append(e.relayWorkers, w)
			e.relayReaders.Add(2)
			go func(filter nostr.Filter, cursor transport.RelayCursor) {
				defer e.relayReaders.Done()
				for ctx.Err() == nil {
					if cursor.Blocked {
						if !relayDelay(ctx, 30*time.Second) {
							return
						}
						cursor.Blocked = false
						cursor.Until, cursor.Limit = 0, transport.RelayPageSize
					}
					page, err := transport.ReadPageAs(ctx, w.url, e.identity, filter, cursor)
					batch := &relayHistoryBatch{page: page, err: err, ack: make(chan struct{})}
					select {
					case w.pages <- batch:
					case <-ctx.Done():
						return
					}
					select {
					case <-batch.ack:
					case <-ctx.Done():
						return
					}
					previous := cursor
					cursor = page.Next
					if err != nil || cursor.Blocked || cursor.Sweeps > previous.Sweeps {
						if !relayDelay(ctx, 30*time.Second) {
							return
						}
					}
				}
			}(spec.filter, record.Cursor)
			go func(filter nostr.Filter) {
				defer e.relayReaders.Done()
				report := func(state relayLiveState) {
					select {
					case w.states <- state:
					case <-ctx.Done():
					}
				}
				for ctx.Err() == nil {
					err := transport.ListenAs(ctx, w.url, e.identity, filter, func(event nostr.Event) error {
						select {
						case w.live <- event:
							return nil
						default:
							return errors.New("live relay queue is full; reconnecting and history sweeps will retry retained events")
						}
					}, func() { report(relayLiveState{ready: true}) })
					if ctx.Err() != nil {
						return
					}
					report(relayLiveState{err: fmt.Sprint(err)})
					if !relayDelay(ctx, time.Second) {
						return
					}
				}
			}(spec.filter)
		}
	}
}

// All network reads and signature verification run outside Engine.mu. A tick
// consumes a bounded number of events, prioritizing encrypted mailbox traffic.
// A page's next cursor is saved with all effects before its producer can advance.
func (e *Engine) drainRelaySync() {
	e.startRelaySync()
	remaining := relayTickEvents
	for index, w := range e.relayWorkers {
		budget := remaining / (len(e.relayWorkers) - index)
		record := e.s.RelaySync[w.key]
		for len(w.states) > 0 {
			state := <-w.states
			record.Live = state.ready
			record.Error = state.err
		}
		if w.current == nil {
			select {
			case w.current = <-w.pages:
			default:
			}
		}
		consumed := 0
		// Live traffic includes older randomized gift-wrap timestamps and receives
		// half each worker's turn even during a large historical replay.
		for remaining > 0 && consumed < min(8, budget/2) {
			select {
			case event := <-w.live:
				if err := e.ingestRelayEvent(event); err != nil {
					record.Error = err.Error()
				}
				remaining--
				consumed++
			default:
				consumed = min(8, budget/2)
			}
		}
		batch := w.current
		if batch != nil {
			limit := min(len(batch.page.Events), batch.offset+min(budget-consumed, remaining))
			for batch.offset < limit {
				if err := e.ingestRelayEvent(batch.page.Events[batch.offset]); err != nil {
					batch.rejected = err.Error()
				}
				batch.offset++
				remaining--
			}
			if batch.offset == len(batch.page.Events) {
				record.Cursor = batch.page.Next
				record.Error = ""
				record.ObservedAt = time.Now().Unix()
				if batch.err != nil {
					record.Error = batch.err.Error()
				}
				if batch.rejected != "" {
					record.Error = "Some relay events could not be applied and remain subject to repeated history sweeps: " + batch.rejected
				}
				e.relayAcks = append(e.relayAcks, batch.ack)
				w.current = nil
			}
		}
		e.s.RelaySync[w.key] = record
	}
	e.marketAllRelays = len(e.Config.Relays) > 0
	for _, url := range e.Config.Relays {
		record := e.s.RelaySync[url+"\noffers"]
		if !record.Live || record.Error != "" || record.ObservedAt == 0 {
			e.marketAllRelays = false
		}
		if record.ObservedAt > e.marketObservedAt {
			e.marketObservedAt = record.ObservedAt
		}
	}
}
func (e *Engine) ingestRelayEvent(event nostr.Event) error {
	switch event.Kind {
	case transport.TowerKind:
		e.ingestTower(event)
	case transport.OfferKind:
		return e.ingestOffer(event)
	default:
		return e.receive(event)
	}
	return nil
}
func (e *Engine) acknowledgeRelayPages() {
	for _, ack := range e.relayAcks {
		close(ack)
	}
	e.relayAcks = nil
}

func (e *Engine) relayHealth() []RelaySyncRecord {
	records := make([]RelaySyncRecord, 0, len(e.s.RelaySync))
	for _, record := range e.s.RelaySync {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Relay != records[j].Relay {
			return records[i].Relay < records[j].Relay
		}
		return records[i].Filter < records[j].Filter
	})
	return records
}
