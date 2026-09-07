package daemon

import (
	"context"
	"sort"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/transport"
)

type publicationAttempt struct {
	id     string
	event  nostr.Event
	relays []string
}
type publicationResult struct {
	id, eventID string
	all         bool
	failure     string
}

func (e *Engine) startPublicationWorkers(ctx context.Context) {
	e.publicationQueue = make(chan publicationAttempt, 16)
	e.publicationResults = make(chan publicationResult, 16)
	e.publicationBusy = map[string]bool{}
	for range 2 {
		e.relayReaders.Add(1)
		go func() {
			defer e.relayReaders.Done()
			for {
				var attempt publicationAttempt
				select {
				case <-ctx.Done():
					return
				case attempt = <-e.publicationQueue:
				}
				result := publicationResult{id: attempt.id, eventID: attempt.event.ID.Hex(), all: len(attempt.relays) > 0}
				for _, url := range attempt.relays {
					if err := transport.Publish(ctx, url, attempt.event); err != nil {
						result.all = false
						result.failure = err.Error()
					}
				}
				select {
				case <-ctx.Done():
					return
				case e.publicationResults <- result:
				}
			}
		}()
	}
}

// Tick schedules a bounded number of existing durable deliveries. Relay IO is
// independent of wallet settlement, and publication results apply only to the
// exact event that was attempted (a late success cannot acknowledge a successor).
func (e *Engine) dispatchPublications() error {
	if e.publicationQueue == nil {
		return nil
	}
	now := time.Now().Unix()
	for len(e.publicationResults) > 0 {
		result := <-e.publicationResults
		delete(e.publicationBusy, result.id)
		d := e.s.Outbox[result.id]
		if d == nil || d.Event.ID.Hex() != result.eventID {
			continue
		}
		d.Published = result.all
		if !result.all {
			e.lastError = "relay publication: " + result.failure
			continue
		}
		if d.Event.Kind == transport.OfferKind {
			id := transport.Tag(d.Event, "d")
			if record, ok := e.s.OrderRecords[id]; ok && record.EventID == result.eventID {
				record.Publication, record.AcknowledgedAt = "relay_acknowledged", now
				e.s.OrderRecords[id] = record
			}
		}
		if d.To == "" || (d.Expires > 0 && d.Type != "tower-query") {
			delete(e.s.Outbox, result.id)
		}
	}
	ids := make([]string, 0, len(e.s.Outbox))
	for id, d := range e.s.Outbox {
		if e.publicationBusy[id] || (d.Type == "tower-query" && d.Published) {
			continue
		}
		interval := int64(5)
		if d.IsAck && d.Published {
			interval = 60
		}
		if now-d.LastAttempt >= interval {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := e.s.Outbox[ids[i]], e.s.Outbox[ids[j]]
		if a.LastAttempt != b.LastAttempt {
			return a.LastAttempt < b.LastAttempt
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		if len(e.publicationBusy) >= 16 {
			break
		}
		d := e.s.Outbox[id]
		attempt := publicationAttempt{id: id, event: d.Event, relays: append([]string(nil), e.Config.Relays...)}
		select {
		case e.publicationQueue <- attempt:
			e.publicationBusy[id] = true
			d.LastAttempt = now
		default:
			return e.save()
		}
	}
	return e.save()
}
