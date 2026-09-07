package relay

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func mailboxEvent(t *testing.T, key nostr.SecretKey, recipient nostr.PubKey, at nostr.Timestamp, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1059, CreatedAt: at, Tags: nostr.Tags{{"p", recipient.Hex()}}, Content: content}
	if err := transport.Sign(&event, key); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestBoundedRelayHistoryBeyondTenThousandAndLateLiveDelivery(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	identity, sender := nostr.Generate(), nostr.Generate()
	base := nostr.Now() - 20000
	// Seed the deterministic relay's retained history without 10,017 separate
	// filesystem fsyncs. All reads below go through its real websocket handler
	// and the transport's signature verification and inclusive page cursors.
	for i := range 10017 {
		event := mailboxEvent(t, sender, identity.Public(), base+nostr.Timestamp(i/3), fmt.Sprint(i))
		r.events[event.ID.Hex()] = event
	}
	server := httptest.NewServer(r)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	filter := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {identity.Public().Hex()}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ready := make(chan struct{}, 1)
	delivered := make(chan nostr.Event, 1)
	closed := make(chan error, 1)
	go func() {
		closed <- transport.ListenAs(ctx, url, identity, filter, func(event nostr.Event) error {
			delivered <- event
			return nil
		}, func() { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("live subscription did not become ready")
	}
	start := time.Now()
	seen := map[string]bool{}
	cursor := transport.RelayCursor{}
	pages := 0
	for cursor.Sweeps == 0 {
		page, err := transport.ReadPageAs(ctx, url, identity, filter, cursor)
		if err != nil || len(page.Events) > transport.RelayTieLimit {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			seen[event.ID.Hex()] = true
		}
		cursor = page.Next
		pages++
		if pages > 200 || cursor.Blocked {
			t.Fatal("bounded history made no progress", cursor)
		}
	}
	if len(seen) != 10017 || cursor.Incomplete == "" {
		t.Fatal("history was skipped or EOSE claimed exhaustive coverage", len(seen), cursor)
	}
	t.Logf("read %d distinct retained mailbox events in %d pages over %s", len(seen), pages, time.Since(start))
	late := mailboxEvent(t, sender, identity.Public(), base-10000, "late randomized timestamp")
	if err = transport.Publish(ctx, url, late); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-delivered:
		if event.ID != late.ID {
			t.Fatal("limit zero replayed old history or live timestamp filter lost late event")
		}
	case <-ctx.Done():
		t.Fatal("late old-timestamp event was not delivered live")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("live subscription ignored cancellation")
	}
}

func TestRelaySaturatedTimestampReportsIncompleteWithoutSkipping(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	identity := nostr.Generate()
	at := nostr.Now() - 10
	for i := range transport.RelayTieLimit + 1 {
		event := mailboxEvent(t, identity, identity.Public(), at, fmt.Sprint(i))
		r.events[event.ID.Hex()] = event
	}
	older := mailboxEvent(t, identity, identity.Public(), at-1, "older obligation")
	r.events[older.ID.Hex()] = older
	server := httptest.NewServer(r)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	filter := nostr.Filter{Kinds: []nostr.Kind{1059}}
	cursor := transport.RelayCursor{}
	for range 10 {
		page, err := transport.ReadPageAs(context.Background(), url, identity, filter, cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursor = page.Next
		if cursor.Blocked {
			break
		}
	}
	if !cursor.Blocked || cursor.Until != at || cursor.Sweeps != 0 || !strings.Contains(cursor.Incomplete, "timestamp") {
		t.Fatal("unexposable timestamp was silently skipped", cursor)
	}
}

func TestRelayIndependentFilterLimitsAndAddressableTies(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	key := nostr.Generate()
	at := nostr.Now() - 10
	for i := range 12 {
		event := signed(t, key, at+nostr.Timestamp(i/2), fmt.Sprint(i))
		event.Tags[0][1] = fmt.Sprint(i)
		if err = transport.Sign(&event, key); err != nil {
			t.Fatal(err)
		}
		r.events[replaceKey(event)] = event
	}
	server := httptest.NewServer(r)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	events, err := transport.Pull(context.Background(), url, nostr.Filter{Kinds: []nostr.Kind{transport.OfferKind}, Limit: 3}, nostr.Filter{Kinds: []nostr.Kind{transport.OfferKind}, Until: at + 1, Limit: 2})
	if err != nil || len(events) != 5 {
		t.Fatal("filters did not apply initial limits independently", len(events), err)
	}
	for i := 1; i < len(events); i++ {
		if !newer(events[i-1], events[i]) {
			t.Fatal("relay violated descending time and lowest-ID ordering")
		}
	}
}
