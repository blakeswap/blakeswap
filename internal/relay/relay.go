// Package relay is a small durable NIP-01 loopback relay for local development.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	bolt "go.etcd.io/bbolt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type subscriber struct {
	filters []nostr.Filter
	send    func(any) bool
}
type Relay struct {
	mu            sync.Mutex
	db            *bolt.DB
	events        map[string]nostr.Event
	subscriptions map[string]subscriber
	clients       chan struct{}
}

func Open(path string) (*Relay, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	r := &Relay{db: db, events: map[string]nostr.Event{}, subscriptions: map[string]subscriber{}, clients: make(chan struct{}, 64)}
	e = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists([]byte("events"))
		if e != nil {
			return e
		}
		return b.ForEach(func(k, v []byte) error {
			var event nostr.Event
			if e := json.Unmarshal(v, &event); e != nil {
				return e
			}
			if e := transport.Valid(event); e != nil {
				return e
			}
			r.events[string(k)] = event
			return nil
		})
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	return r, nil
}
func (r *Relay) Close() error { return r.db.Close() }
func replaceKey(e nostr.Event) string {
	if e.Kind >= 30000 && e.Kind < 40000 {
		return fmt.Sprintf("%d:%s:%s", e.Kind, e.PubKey.Hex(), transport.Tag(e, "d"))
	}
	if e.Kind == 10050 {
		return "10050:" + e.PubKey.Hex()
	}
	return e.ID.Hex()
}
func newer(a, b nostr.Event) bool {
	return a.CreatedAt > b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID.Hex() < b.ID.Hex())
}
func expired(e nostr.Event) bool {
	s := transport.Tag(e, "expiration")
	if s == "" {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err != nil || n <= time.Now().Unix()
}
func (r *Relay) put(event nostr.Event) error {
	if e := transport.Valid(event); e != nil {
		return e
	}
	if event.Kind != transport.OfferKind && event.Kind != 1059 && event.Kind != 10050 {
		return errors.New("unsupported event kind")
	}
	if len(event.String()) > transport.MaxEventSize || event.CreatedAt > nostr.Now()+600 || expired(event) {
		return errors.New("event expired, future-dated or too large")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := replaceKey(event)
	old, exists := r.events[key]
	if exists && (old.ID == event.ID || !newer(event, old)) {
		return nil
	}
	if !exists && len(r.events) >= 100000 {
		return errors.New("relay storage quota reached")
	}
	raw, e := json.Marshal(event)
	if e != nil {
		return e
	}
	if e = r.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("events")).Put([]byte(key), raw) }); e != nil {
		return e
	}
	r.events[key] = event
	for id, s := range r.subscriptions {
		for _, f := range s.filters {
			if f.Matches(event) {
				s.send([]any{"EVENT", id, event})
				break
			}
		}
	}
	return nil
}
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	select {
	case r.clients <- struct{}{}:
		defer func() { <-r.clients }()
	default:
		http.Error(w, "capacity", 503)
		return
	}
	c, e := websocket.Accept(w, req, nil)
	if e != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(transport.MaxEventSize + 4096)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	out := make(chan any, 12000)
	send := func(v any) bool {
		select {
		case out <- v:
			return true
		default:
			cancel()
			return false
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-out:
				writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				e := wsjson.Write(writeCtx, c, v)
				stop()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	connection := transport.RandomID()
	subs := map[string]string{}
	defer func() {
		r.mu.Lock()
		for _, id := range subs {
			delete(r.subscriptions, id)
		}
		r.mu.Unlock()
		cancel()
		c.CloseNow()
		<-done
	}()
	for {
		var raw []json.RawMessage
		if e = wsjson.Read(ctx, c, &raw); e != nil {
			return
		}
		if len(raw) < 2 {
			send([]any{"NOTICE", "invalid request"})
			continue
		}
		var kind string
		_ = json.Unmarshal(raw[0], &kind)
		switch kind {
		case "EVENT":
			var event nostr.Event
			if e = json.Unmarshal(raw[1], &event); e == nil {
				e = r.put(event)
			}
			reason := ""
			if e != nil {
				reason = e.Error()
			}
			send([]any{"OK", event.ID.Hex(), e == nil, reason})
		case "REQ":
			var id string
			if json.Unmarshal(raw[1], &id) != nil || id == "" || len(id) > 64 || len(raw) < 3 || len(raw) > 6 {
				send([]any{"NOTICE", "invalid subscription"})
				continue
			}
			if len(subs) >= 32 && subs[id] == "" {
				send([]any{"CLOSED", id, "subscription limit"})
				continue
			}
			var filters []nostr.Filter
			valid := true
			for _, v := range raw[2:] {
				var f nostr.Filter
				if json.Unmarshal(v, &f) != nil || f.Search != "" {
					valid = false
					break
				}
				filters = append(filters, f)
			}
			if !valid {
				send([]any{"CLOSED", id, "invalid filter"})
				continue
			}
			r.mu.Lock()
			key := connection + ":" + id
			subs[id] = key
			events := []nostr.Event{}
			for _, event := range r.events {
				if expired(event) {
					continue
				}
				for _, f := range filters {
					if f.Matches(event) {
						events = append(events, event)
						break
					}
				}
			}
			sort.Slice(events, func(i, j int) bool { return newer(events[i], events[j]) })
			if len(events) > 10000 {
				send([]any{"CLOSED", id, "history exceeds safe bound; narrow filter"})
				delete(r.subscriptions, key)
				r.mu.Unlock()
				continue
			}
			for _, event := range events {
				send([]any{"EVENT", id, event})
			}
			send([]any{"EOSE", id})
			r.subscriptions[key] = subscriber{filters, func(v any) bool { msg := v.([]any); msg[1] = id; return send(msg) }}
			r.mu.Unlock()
		case "CLOSE":
			var id string
			_ = json.Unmarshal(raw[1], &id)
			r.mu.Lock()
			delete(r.subscriptions, subs[id])
			delete(subs, id)
			r.mu.Unlock()
		default:
			send([]any{"NOTICE", "unsupported command"})
		}
	}
}
