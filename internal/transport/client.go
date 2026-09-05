package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"time"
)

func Publish(ctx context.Context, url string, event nostr.Event) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, url, nil)
	if e != nil {
		return e
	}
	defer c.CloseNow()
	c.SetReadLimit(MaxEventSize)
	if e = wsjson.Write(ctx, c, []any{"EVENT", event}); e != nil {
		return e
	}
	for {
		var reply []json.RawMessage
		if e = wsjson.Read(ctx, c, &reply); e != nil {
			return e
		}
		if len(reply) < 4 {
			continue
		}
		var kind, id, reason string
		var ok bool
		_ = json.Unmarshal(reply[0], &kind)
		_ = json.Unmarshal(reply[1], &id)
		if kind != "OK" || id != event.ID.Hex() {
			continue
		}
		_ = json.Unmarshal(reply[2], &ok)
		_ = json.Unmarshal(reply[3], &reason)
		if !ok {
			return errors.New(reason)
		}
		return nil
	}
}
func Pull(ctx context.Context, url string, filters ...nostr.Filter) ([]nostr.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, url, nil)
	if e != nil {
		return nil, e
	}
	defer c.CloseNow()
	c.SetReadLimit(MaxEventSize + 4096)
	req := []any{"REQ", "sync"}
	for _, f := range filters {
		req = append(req, f)
	}
	if e = wsjson.Write(ctx, c, req); e != nil {
		return nil, e
	}
	var events []nostr.Event
	for {
		var reply []json.RawMessage
		if e = wsjson.Read(ctx, c, &reply); e != nil {
			return nil, e
		}
		if len(reply) < 2 {
			return nil, errors.New("malformed relay response")
		}
		var kind, sub string
		_ = json.Unmarshal(reply[0], &kind)
		_ = json.Unmarshal(reply[1], &sub)
		if sub != "sync" {
			continue
		}
		switch kind {
		case "EVENT":
			if len(reply) != 3 {
				return nil, errors.New("malformed event")
			}
			var event nostr.Event
			if e = json.Unmarshal(reply[2], &event); e != nil {
				return nil, e
			}
			if e = Valid(event); e != nil {
				continue
			}
			matched := false
			for _, f := range filters {
				if f.Matches(event) {
					matched = true
				}
			}
			if !matched {
				continue
			}
			events = append(events, event)
			if len(events) > 10000 {
				return nil, errors.New("relay history exceeds safe sync bound")
			}
		case "EOSE":
			return events, nil
		case "CLOSED":
			if len(reply) < 3 {
				return nil, errors.New("relay closed subscription")
			}
			return nil, fmt.Errorf("relay refused subscription: %s", string(reply[2:][0]))
		}
	}
}
