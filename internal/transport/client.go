package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"strings"
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
	return pull(ctx, url, nil, filters...)
}

// PullAs authenticates only when a relay requires it to read this identity's
// mailbox. The key signs a relay-bound NIP-42 challenge, never relay-supplied
// event contents. Publishing gift wraps remains anonymous.
func PullAs(ctx context.Context, url string, identity nostr.SecretKey, filters ...nostr.Filter) ([]nostr.Event, error) {
	return pull(ctx, url, &identity, filters...)
}

func pull(ctx context.Context, url string, identity *nostr.SecretKey, filters ...nostr.Filter) ([]nostr.Event, error) {
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
	var challenge, authID string
	authAttempted := false
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
		if kind == "AUTH" {
			if len(reply) != 2 || sub == "" || len(sub) > 4096 {
				return nil, errors.New("invalid relay authentication challenge")
			}
			challenge = sub
			continue
		}
		if kind == "OK" && authID != "" && sub == authID {
			var ok bool
			var reason string
			if len(reply) != 4 || json.Unmarshal(reply[2], &ok) != nil || json.Unmarshal(reply[3], &reason) != nil {
				return nil, errors.New("malformed relay authentication acknowledgment")
			}
			if !ok {
				return nil, fmt.Errorf("relay authentication rejected: %s", reason)
			}
			authID = ""
			// CLOSED ended the original subscription. Retry only after the
			// matching positive AUTH acknowledgment, with no partial history.
			events = nil
			if e = wsjson.Write(ctx, c, req); e != nil {
				return nil, e
			}
			continue
		}
		if authID != "" {
			continue
		}
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
			var reason string
			if len(reply) != 3 || json.Unmarshal(reply[2], &reason) != nil {
				return nil, errors.New("relay closed subscription")
			}
			if strings.HasPrefix(reason, "auth-required:") && identity != nil && !authAttempted {
				if challenge == "" {
					return nil, errors.New("relay requires authentication but sent no challenge")
				}
				auth := nostr.Event{Kind: 22242, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"relay", url}, {"challenge", challenge}}}
				if e = Sign(&auth, *identity); e != nil {
					return nil, e
				}
				authAttempted = true
				authID = auth.ID.Hex()
				if e = wsjson.Write(ctx, c, []any{"AUTH", auth}); e != nil {
					return nil, e
				}
				continue
			}
			return nil, fmt.Errorf("relay refused subscription: %s", reason)
		}
	}
}
