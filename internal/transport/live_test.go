package transport

import (
	"context"
	"fiatjaf.com/nostr"
	"os"
	"testing"
)

// Exercises the daemon's actual orderbook and mailbox filters, using a fresh
// disposable identity for NIP-42 AUTH. No EVENT is published.
func TestPublishedRelaysReadOnly(t *testing.T) {
	if os.Getenv("BLAKESWAP_LIVE_READS") != "1" {
		t.Skip("opt-in external read")
	}
	for _, url := range []string{"wss://nos.lol", "wss://relay.primal.net", "wss://relay.ditto.pub"} {
		t.Run(url, func(t *testing.T) {
			t.Parallel()
			key := nostr.Generate()
			_, err := PullAs(context.Background(), url, key,
				nostr.Filter{Kinds: []nostr.Kind{OfferKind}, Tags: nostr.TagMap{"t": {"blakeswap-mainnet-v1"}}},
				nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {key.Public().Hex()}}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
