package transport

import (
	"context"
	"fiatjaf.com/nostr"
	"os"
	"testing"
)

// Only sends a REQ for an impossible all-zero event ID. No EVENT is published.
func TestPublishedRelaysReadOnly(t *testing.T) {
	if os.Getenv("BLAKESWAP_LIVE_READS") != "1" {
		t.Skip("opt-in external read")
	}
	for _, url := range []string{"wss://nos.lol", "wss://relay.primal.net", "wss://relay.ditto.pub"} {
		t.Run(url, func(t *testing.T) {
			_, err := Pull(context.Background(), url, nostr.Filter{IDs: []nostr.ID{{}}, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
