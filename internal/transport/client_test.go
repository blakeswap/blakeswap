package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestPullRelayAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name        string
		challenge   bool
		requireAuth bool
		anonymous   bool
		ack         string
		rejectRetry bool
		partial     bool
		wantError   string
	}{
		{name: "open relay"},
		{name: "unsolicited challenge does not expose identity", challenge: true},
		{name: "authenticated mailbox", challenge: true, requireAuth: true, ack: "ok"},
		{name: "discard partial history before retry", challenge: true, requireAuth: true, ack: "ok", partial: true},
		{name: "anonymous caller never signs", challenge: true, requireAuth: true, anonymous: true, wantError: "auth-required:"},
		{name: "missing challenge", requireAuth: true, wantError: "sent no challenge"},
		{name: "denied identity", challenge: true, requireAuth: true, ack: "denied", wantError: "relay authentication rejected"},
		{name: "malformed acknowledgment", challenge: true, requireAuth: true, ack: "malformed", wantError: "malformed relay authentication acknowledgment"},
		{name: "unrelated acknowledgment", challenge: true, requireAuth: true, ack: "unrelated", wantError: "context deadline exceeded"},
		{name: "retry is bounded", challenge: true, requireAuth: true, ack: "ok", rejectRetry: true, wantError: "auth-required:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := nostr.Generate()
			filter := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {key.Public().Hex()}}}
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				write := func(msg ...any) {
					t.Helper()
					if err := wsjson.Write(ctx, c, msg); err != nil {
						t.Error(err)
					}
				}
				read := func() []json.RawMessage {
					t.Helper()
					var msg []json.RawMessage
					if err := wsjson.Read(ctx, c, &msg); err != nil {
						t.Error(err)
					}
					return msg
				}
				if tc.challenge {
					write("AUTH", "challenge-for-this-connection")
				}
				request := read()
				if len(request) != 3 || string(request[0]) != `"REQ"` {
					t.Error("missing subscription")
					return
				}
				if tc.partial {
					event, err := Wrap(nostr.Generate(), key.Public(), Message{Version: 1, ID: RandomID(), Type: "test"})
					if err != nil {
						t.Error(err)
						return
					}
					write("EVENT", "sync", event)
				}
				if !tc.requireAuth {
					write("EOSE", "sync")
					var unexpected []json.RawMessage
					if wsjson.Read(ctx, c, &unexpected) == nil {
						t.Error("client authenticated without needing it")
					}
					return
				}
				write("CLOSED", "sync", "auth-required: authenticate the recipient")
				if tc.anonymous || !tc.challenge {
					return
				}
				auth := read()
				if len(auth) != 2 || string(auth[0]) != `"AUTH"` {
					t.Error("missing AUTH")
					return
				}
				var event nostr.Event
				if err := json.Unmarshal(auth[1], &event); err != nil {
					t.Error(err)
					return
				}
				if err := Valid(event); err != nil {
					t.Error("invalid authentication signature", err)
					return
				}
				if event.PubKey != key.Public() || event.Kind != 22242 || event.Content != "" || len(event.Tags) != 2 || Tag(event, "challenge") != "challenge-for-this-connection" || Tag(event, "relay") != "ws://"+r.Host || event.CreatedAt < nostr.Now()-10 || event.CreatedAt > nostr.Now()+10 {
					t.Error("authentication did not bind the identity, challenge and destination")
					return
				}
				switch tc.ack {
				case "denied":
					write("OK", event.ID.Hex(), false, "restricted: not allowed")
					return
				case "malformed":
					write("OK", event.ID.Hex(), "true", "")
					return
				case "unrelated":
					write("OK", strings.Repeat("0", 64), true, "")
					write("EOSE", "sync") // A closed subscription cannot complete AUTH.
					var unexpected []json.RawMessage
					if wsjson.Read(ctx, c, &unexpected) == nil {
						t.Error("client retried before a matching acknowledgment")
					}
					return
				}
				write("OK", event.ID.Hex(), true, "")
				retry := read()
				if len(retry) != len(request) {
					t.Error("missing retry")
					return
				}
				for i := range request {
					if string(retry[i]) != string(request[i]) {
						t.Error("retry changed filter or subscription")
					}
				}
				if tc.rejectRetry {
					write("AUTH", "replacement-challenge")
					write("CLOSED", "sync", "auth-required: still denied")
					var unexpected []json.RawMessage
					if wsjson.Read(ctx, c, &unexpected) == nil {
						t.Error("authentication retried indefinitely")
					}
					return
				}
				write("EOSE", "sync")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			url := "ws" + strings.TrimPrefix(server.URL, "http")
			var events []nostr.Event
			var err error
			if tc.anonymous {
				events, err = Pull(ctx, url, filter)
			} else {
				events, err = PullAs(ctx, url, key, filter)
			}
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("want %q, got %v", tc.wantError, err)
			}
			if len(events) != 0 {
				t.Fatal("partial pre-auth history escaped")
			}
			<-done
		})
	}
}
