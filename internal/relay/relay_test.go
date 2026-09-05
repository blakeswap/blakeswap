package relay

import (
	"context"
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/transport"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func signed(t *testing.T, key nostr.SecretKey, at nostr.Timestamp, content string) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: transport.OfferKind, CreatedAt: at, Tags: nostr.Tags{{"d", "one"}, {"t", transport.Namespace}}, Content: content}
	if err := transport.Sign(&e, key); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestRelayDurabilityReplacementAndValidation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	r, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(r)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	key := nostr.Generate()
	old := signed(t, key, nostr.Now()-10, "open")
	cancel := signed(t, key, nostr.Now(), "cancelled")
	for _, event := range []nostr.Event{old, cancel, old, cancel} {
		if e = transport.Publish(ctx, url, event); e != nil {
			t.Fatal(e)
		}
	}
	f := nostr.Filter{Kinds: []nostr.Kind{transport.OfferKind}}
	events, e := transport.Pull(ctx, url, f)
	if e != nil || len(events) != 1 || events[0].ID != cancel.ID {
		t.Fatal("stale order resurrected", events, e)
	}
	forged := cancel
	forged.Content = "open"
	if e = transport.Publish(ctx, url, forged); e == nil {
		t.Fatal("forged order accepted")
	}
	future := signed(t, key, nostr.Now()+10000, "future")
	if e = transport.Publish(ctx, url, future); e == nil {
		t.Fatal("future order accepted")
	}
	bob := nostr.Generate()
	mail, e := transport.Wrap(key, bob.Public(), transport.Message{Version: 1, ID: transport.RandomID(), Type: "test", Body: json.RawMessage(`{}`)})
	if e != nil {
		t.Fatal(e)
	}
	if e = transport.Publish(ctx, url, mail); e != nil {
		t.Fatal(e)
	}
	server.Close()
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	server = httptest.NewServer(r)
	defer server.Close()
	url = "ws" + strings.TrimPrefix(server.URL, "http")
	events, e = transport.Pull(ctx, url, nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {bob.Public().Hex()}}})
	if e != nil || len(events) != 1 || events[0].ID != mail.ID {
		t.Fatal("offline mailbox lost after restart", e)
	}
	if _, _, e = transport.Unwrap(bob, events[0]); e != nil {
		t.Fatal(e)
	}
}
func TestAddressableTieUsesLowestEventID(t *testing.T) {
	r, e := Open(filepath.Join(t.TempDir(), "relay.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	key := nostr.Generate()
	a := signed(t, key, nostr.Now(), "a")
	b := signed(t, key, a.CreatedAt, "b")
	want := a
	if b.ID.Hex() < a.ID.Hex() {
		want = b
	}
	if e = r.put(a); e != nil {
		t.Fatal(e)
	}
	if e = r.put(b); e != nil {
		t.Fatal(e)
	}
	if r.events[replaceKey(a)].ID != want.ID {
		t.Fatal("wrong tie break")
	}
}
