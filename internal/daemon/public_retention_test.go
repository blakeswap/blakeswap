package daemon

import (
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"testing"
	"time"
)

func publicRetentionEvent(t *testing.T, key nostr.SecretKey, id, status string, at nostr.Timestamp) nostr.Event {
	t.Helper()
	offer := protocol.Offer{Network: chain.Regtest, ID: id, Maker: key.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Status: status, Expires: time.Now().Unix() + 3600}
	raw, _ := offer.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: at, Tags: nostr.Tags{{"d", id}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, key); err != nil {
		t.Fatal(err)
	}
	return event
}
func TestPublicCancellationSurvivesPruneRestartAndOverlappingTies(t *testing.T) {
	e, _ := receiveEngine(t)
	key := nostr.Generate()
	id := transport.RandomID()
	at := nostr.Now()
	open := publicRetentionEvent(t, key, id, "open", at-1)
	cancel := publicRetentionEvent(t, key, id, "cancelled", at)
	e.ingestOffer(open)
	e.ingestOffer(cancel)
	e.prunePublicOffers()
	if len(e.s.Book) != 0 {
		t.Fatal("closed public row stayed active")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.ingestOffer(open)
	if len(e.s.Book) != 0 {
		t.Fatal("overlapping history resurrected cancelled offer")
	}
	first := publicRetentionEvent(t, key, id, "open", at)
	second := publicRetentionEvent(t, key, id, "filled", at)
	newer, older := first, second
	if newer.ID.Hex() > older.ID.Hex() {
		newer, older = older, newer
	}
	// New identity establishes the lexicographic tie fixture independently of the
	// prior cancellation's own ID, then overlaps in reverse arrival order.
	delete(e.s.PublicVersions, key.Public().Hex()+":"+id)
	e.ingestOffer(older)
	e.ingestOffer(newer)
	e.ingestOffer(older)
	if e.s.PublicVersions[key.Public().Hex()+":"+id].ID != newer.ID.Hex() {
		t.Fatal("addressable same-time order changed across overlap")
	}
}
func TestPublicCapacityStillAcceptsKnownCancellation(t *testing.T) {
	e, _ := receiveEngine(t)
	key := nostr.Generate()
	id := transport.RandomID()
	at := nostr.Now()
	e.ingestOffer(publicRetentionEvent(t, key, id, "open", at-1))
	for i := len(e.s.PublicVersions); i < publicIdentityLimit; i++ {
		e.s.PublicVersions[string(rune(0x1000+i))] = PublicVersion{ID: "bounded retained identity"}
	}
	e.ingestOffer(publicRetentionEvent(t, key, transport.RandomID(), "open", at))
	if !e.s.PublicLimited || len(e.s.Book) != 1 {
		t.Fatal("unsolicited public limit was ignored")
	}
	e.ingestOffer(publicRetentionEvent(t, key, id, "cancelled", at))
	e.prunePublicOffers()
	if len(e.s.Book) != 0 {
		t.Fatal("capacity discarded a known cancellation")
	}
}
