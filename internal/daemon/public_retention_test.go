package daemon

import (
	"context"
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

func TestOwnPublicOrderingArchivesWithHistoryAndSurvivesRestart(t *testing.T) {
	e, _ := receiveEngine(t)
	id := transport.RandomID()
	key := e.identity.Public().Hex() + ":" + id
	at := nostr.Now()
	open := publicRetentionEvent(t, e.identity, id, "open", at-1)
	cancelled := publicRetentionEvent(t, e.identity, id, "cancelled", at)
	e.s.Offers = map[string]nostr.Event{id: open}
	if err := e.ingestOffer(open); err != nil {
		t.Fatal(err)
	}
	e.s.Offers[id] = cancelled
	if err := e.ingestOffer(cancelled); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(e.s)
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Book) != 0 || len(e.s.OwnPublicVersions) != 0 || len(e.s.PublicVersions) != 0 {
		t.Fatal("completed own view remained active")
	}
	if BackupSemanticToken(e.s) != token {
		t.Fatal("moving the ordering floor changed freshness")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.semanticParts = nil
	if err := e.ingestOffer(open); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Book) != 0 {
		t.Fatal("old resweep resurrected cold own offer")
	}
	// A newly observed signed cancellation advances the durable floor while the
	// original source remains cold and cannot grant new publication authority.
	newer := publicRetentionEvent(t, e.identity, id, "cancelled", at+1)
	if err := e.ingestOffer(newer); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Book) != 0 || len(e.s.Offers) != 0 || e.s.OwnPublicVersions[key].ID != newer.ID.Hex() {
		t.Fatal("new cold ordering lost its safe projection")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	changed := BackupSemanticToken(e.s)
	if changed == token {
		t.Fatal("new meaningful archived ordering did not stale backup")
	}
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != changed {
		t.Fatal("ordering compaction changed semantic token")
	}
	if err := e.ingestOffer(cancelled); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Book) != 0 {
		t.Fatal("older cancellation repopulated view")
	}
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	newest := publicRetentionEvent(t, e.identity, id, "open", at+2)
	if err := e.ingestRelayEvent(newest); err == nil {
		t.Fatal("cold ordering read failure masqueraded as successful relay application")
	}
	if len(e.s.Book) != 0 {
		t.Fatal("read failure admitted unverifiable public version")
	}
}

func TestOwnRemoteOpenExpiryLeavesNoActiveOrdering(t *testing.T) {
	e, _ := receiveEngine(t)
	id := transport.RandomID()
	key := e.identity.Public().Hex() + ":" + id
	old := publicRetentionEvent(t, e.identity, id, "cancelled", nostr.Now()-1)
	e.s.Offers = map[string]nostr.Event{id: old}
	if err := e.ingestOffer(old); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	offer := protocol.Offer{Network: chain.Regtest, ID: id, Maker: e.identity.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Status: "open", Expires: time.Now().Unix() + 2}
	raw, _ := offer.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", id}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, e.identity); err != nil {
		t.Fatal(err)
	}
	if err := e.ingestRelayEvent(event); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Book) != 1 || len(e.s.Offers) != 0 {
		t.Fatal("newer remote own order did not remain view-only")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Unix(offer.Expires, 0)))
	e.prunePublicOffers()
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Book) != 0 || len(saved.OwnPublicVersions) != 0 {
		t.Fatal("expired own remote view did not compact")
	}
	if _, ok := saved.PublicVersions[key]; ok {
		t.Fatal("expired remote own order permanently retained active public identity after own floor archived")
	}
}

func TestExpiredOfferResweepDoesNotInvalidateHealthyMarket(t *testing.T) {
	e, _ := receiveEngine(t)
	key := nostr.Generate()
	id := transport.RandomID()
	offer := protocol.Offer{Network: chain.Regtest, ID: id, Maker: key.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Status: "open", Expires: time.Now().Unix() - 1}
	raw, _ := offer.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now() - 3600, Tags: nostr.Tags{{"d", id}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, key); err != nil {
		t.Fatal(err)
	}
	e.Config.Relays = []string{"wss://healthy.invalid"}
	workerKey := e.Config.Relays[0] + "\noffers"
	e.relayCancel = func() {}
	e.s.RelaySync = map[string]RelaySyncRecord{workerKey: {Relay: e.Config.Relays[0], Filter: "offers", Live: true, ObservedAt: time.Now().Unix()}}
	w := &relayWorker{key: workerKey, url: e.Config.Relays[0], name: "offers", pages: make(chan *relayHistoryBatch, 1), live: make(chan nostr.Event, 32), states: make(chan relayLiveState, 4)}
	w.pages <- &relayHistoryBatch{page: transport.RelayPage{Events: []nostr.Event{event}, Next: transport.RelayCursor{Sweeps: 1}}, ack: make(chan struct{})}
	e.relayWorkers = []*relayWorker{w}
	e.drainRelaySync()
	if len(e.s.Book) != 0 {
		t.Fatal("expired order was retained")
	}
	if !e.marketAllRelays {
		t.Fatalf("healthy completed resweep became unavailable solely because of an expired signed order: %s", e.s.RelaySync[workerKey].Error)
	}
}

func TestLegacyOrphanOwnOrderingMigrationPreservesStrongestFloor(t *testing.T) {
	for _, newest := range []string{"legacy", "cold", "tie"} {
		t.Run(newest, func(t *testing.T) {
			e, _ := receiveEngine(t)
			id := transport.RandomID()
			key := e.identity.Public().Hex() + ":" + id
			floor := PublicVersion{CreatedAt: nostr.Now(), ID: "bbbb"}
			e.s.OwnPublicVersions = map[string]PublicVersion{key: floor}
			if err := e.stageArchive("own_public_versions", key); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			legacy := floor
			switch newest {
			case "legacy":
				legacy.CreatedAt++
			case "cold":
				legacy.CreatedAt--
			case "tie":
				legacy.ID = "aaaa"
			}
			e.s.PublicVersions = map[string]PublicVersion{key: legacy}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			budget := 64
			// A newer cold floor first promotes its old owner. The next Tick
			// may compact the replacement after that deletion is committed.
			for range 2 {
				budget = 64
				if err := e.compactOwnPublicVersions(&budget); err != nil {
					t.Fatal(err)
				}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.PublicVersions) != 0 || len(saved.OwnPublicVersions) != 0 {
				t.Fatal("orphan own floor remained active")
			}
			var retained PublicVersion
			found, err := e.archivedValue("own_public_versions", key, &retained)
			expected := legacy
			if newest == "cold" {
				expected = floor
			}
			if err != nil || !found || retained != expected {
				t.Fatal("ordering floor lost", retained, expected, err)
			}
			// Failed source reads retain the legacy owner and cannot authorize a guess.
			e.s.PublicVersions[key] = PublicVersion{CreatedAt: floor.CreatedAt + 2, ID: "newer"}
			if err := e.vault.Close(); err != nil {
				t.Fatal(err)
			}
			budget = 64
			if err := e.compactOwnPublicVersions(&budget); err == nil {
				t.Fatal("cold read failure hidden")
			}
			if _, ok := e.s.PublicVersions[key]; !ok {
				t.Fatal("read failure discarded legacy floor")
			}
		})
	}
}
