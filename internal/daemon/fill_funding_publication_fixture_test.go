package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/relay"
)

// The fixture supplies retained funding bytes. The production recordFunding and
// asynchronous encrypted relay path supply the notification, not chain proof.
func TestPartialMatrixFundingRequiresRelayPublication(t *testing.T) {
	for _, role := range []string{"taker", "maker"} {
		t.Run(role, func(t *testing.T) { runPartialFundingPublication(t, role) })
	}
}

func runPartialFundingPublication(t *testing.T, role string) {
	t.Helper()
	e, child, _, _ := isolatedFixture(t, role)
	kind := "long-funded"
	if role == "maker" {
		kind = "short-funded"
	}
	if err := e.recordFunding(child); err != nil {
		t.Fatal(err)
	}
	expected, err := partialPublications(e, []string{child.ID}, kind)
	if err != nil || len(expected) != 1 {
		t.Fatal("exact funding notification unavailable", err)
	}
	var key string
	for key = range expected {
	}
	immutable := protocol.Digest(child)
	localRelay, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}, 2), make(chan struct{})
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
	ctx, cancel := context.WithCancel(context.Background())
	e.Config.Relays = []string{"ws" + strings.TrimPrefix(server.URL, "http")}
	e.relayCancel = cancel
	e.startPublicationWorkers(ctx)
	defer func() {
		e.nodes = nil
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := e.dispatchPublications(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("funding notification worker did not reach the gated relay")
	}
	if ready, err := partialPublicationsPublished(e, expected); err != nil || ready {
		t.Fatal("scheduled funding notification justified sender shutdown", err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := e.dispatchPublications(); err != nil {
			t.Fatal(err)
		}
		ready, err := partialPublicationsPublished(e, expected)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exact funding notification did not reach the relay")
		}
		time.Sleep(time.Millisecond)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil || saved.Outbox[key] == nil || !saved.Outbox[key].Published || saved.Outbox[key].Acknowledged || protocol.Digest(saved.Swaps[child.ID]) != immutable {
		t.Fatal("relay publication changed funding custody or invented a recipient ACK", err)
	}
	for _, failure := range []string{"pending", "event", "recipient", "terms", "funding", "changed-transaction", "retired", "recipient-ack"} {
		t.Run(failure, func(t *testing.T) {
			before := protocol.Digest(e.s)
			original := e.s.Outbox[key]
			delivery, core, want := *original, *child, expected[key]
			e.s.Outbox[key], e.s.Swaps[child.ID] = &delivery, &core
			switch failure {
			case "pending":
				delivery.Published = false
			case "event":
				want.event = "changed"
			case "recipient":
				want.recipient = "changed"
			case "terms":
				want.terms = "changed"
			case "funding":
				want.funding = "changed"
			case "changed-transaction":
				if role == "maker" {
					core.ShortFunding += "00"
				} else {
					core.LongFunding += "00"
				}
			case "retired":
				delivery.Retired = true
			case "recipient-ack":
				delivery.Acknowledged = true
			}
			ready, err := partialPublicationsPublished(e, map[string]partialPublication{key: want})
			e.s.Outbox[key], e.s.Swaps[child.ID] = original, child
			if ready || (failure == "pending" && err != nil) || (failure != "pending" && err == nil) || protocol.Digest(e.s) != before {
				t.Fatal("wrong funding publication evidence accepted or changed custody", err)
			}
		})
	}
}
