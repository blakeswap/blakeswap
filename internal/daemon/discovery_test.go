package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func discoveryEngine(t *testing.T) *Engine {
	t.Helper()
	vault, err := storage.Open(filepath.Join(t.TempDir(), "state.db"), []byte("test-discovery-password"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vault.Close() })
	btc, _ := hex.DecodeString("0014" + strings.Repeat("11", 20))
	blake, _ := hex.DecodeString("0014" + strings.Repeat("22", 20))
	return &Engine{Config: Config{Name: "Test", Mode: "trader", Network: chain.Regtest}, identity: nostr.Generate(), vault: vault, scripts: map[chain.ID][]byte{chain.BTC: btc, chain.Blake: blake}, s: State{Towers: map[string]nostr.Event{}, Outbox: map[string]*Delivery{}, Seen: map[string]string{}}}
}

func TestRescueFeeRefreshesSignedQuotesImmediately(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprint(public), func(t *testing.T) {
			e := discoveryEngine(t)
			e.Config.PublicWatchtower = public
			e.Config.Tower.BPS = 300 // A legacy selected provider must not set our own fee.
			if e.ownTower().BPS != protocol.DefaultTowerBPS {
				t.Fatal("legacy default changed")
			}
			if err := e.advertiseTower(); err != nil {
				t.Fatal(err)
			}
			previous := e.s.Towers[e.identity.Public().Hex()]
			for _, bps := range []int64{125, 1, 1000, 0} {
				e.Config.RescueFeeBPS = bps
				if err := e.advertiseTower(); err != nil {
					t.Fatal(err)
				}
				event := e.s.Towers[e.identity.Public().Hex()]
				quote, err := protocol.DecodeTower(event, chain.Regtest, time.Now().Unix())
				want := bps
				if want == 0 {
					want = protocol.DefaultTowerBPS
				}
				if err != nil || quote.BPS != want || quote.Public != public || event.CreatedAt <= previous.CreatedAt {
					t.Fatal("stale or invalid signed fee", err)
				}
				if !public && len(e.s.Outbox) != 0 {
					t.Fatal("private fee update published")
				}
				if public && (len(e.s.Outbox) != 1 || e.s.Outbox[event.ID.Hex()] == nil) {
					t.Fatal("outdated announcement retained")
				}
				previous = event
			}
		})
	}
}

func TestAcceptedRescueJobsKeepTheirRateAfterRestart(t *testing.T) {
	provider, owner := discoveryEngine(t), discoveryEngine(t)
	job := claimJob(t, provider)
	job.Owner = owner.identity.Public().Hex()
	provider.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job}}
	if err := provider.save(); err != nil {
		t.Fatal(err)
	}
	provider.s = State{}
	if _, err := provider.vault.Load(&provider.s); err != nil {
		t.Fatal(err)
	}
	provider.Config.RescueFeeBPS = 125
	provider.heights = map[chain.ID]uint32{chain.BTC: 120, chain.Blake: 120}
	provider.clocks = provider.heights
	scanner := &recordingScanner{}
	provider.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: scanner, chain.Blake: &recordingScanner{}}
	if _, err := provider.scanTower(context.Background()); err != nil || len(scanner.points) != 1 {
		t.Fatal("accepted job no longer scanned", err)
	}
	if err := provider.advanceTower(context.Background(), nil); err != nil || provider.s.TowerJobs[job.ID].Error != "" {
		t.Fatal("accepted job no longer serviced", err)
	}
	deliver := func(j protocol.Job) error {
		owner.s.Outbox = map[string]*Delivery{}
		if err := owner.queue(provider.identity.Public().Hex(), "tower-job", j.SwapID, j); err != nil {
			t.Fatal(err)
		}
		for _, d := range owner.s.Outbox {
			return provider.receive(d.Event)
		}
		t.Fatal("missing registration")
		return nil
	}
	if err := deliver(job); err != nil {
		t.Fatal("accepted registration retry rejected", err)
	}
	job.ID = transport.RandomID()
	if err := deliver(job); err == nil {
		t.Fatal("new registration used old rate")
	}
}
func TestPrivateByDefaultLookupAndPublicOptOut(t *testing.T) {
	provider, client := discoveryEngine(t), discoveryEngine(t)
	if err := provider.advertiseTower(); err != nil {
		t.Fatal(err)
	}
	if len(provider.s.Outbox) != 0 {
		t.Fatal("default watchtower leaked public announcement")
	}
	if err := client.resolveTower(provider.ownTower().Npub); err != nil {
		t.Fatal(err)
	}
	for _, d := range client.s.Outbox {
		if err := provider.receive(d.Event); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range provider.s.Outbox {
		if d.Event.Kind != 1059 {
			t.Fatal("private lookup published a public event")
		}
		if err := client.receive(d.Event); err != nil {
			t.Fatal(err)
		}
	}
	quote, err := protocol.DecodeTower(client.s.Towers[provider.identity.Public().Hex()], chain.Regtest, time.Now().Unix())
	if err != nil || quote.Public || protocol.Digest(quote.Scripts) != protocol.Digest(provider.ownTower().Scripts) {
		t.Fatal("private provider scripts not authenticated", err)
	}
	provider.Config.PublicWatchtower = true
	if err := provider.advertiseTower(); err != nil {
		t.Fatal(err)
	}
	visible := provider.s.Towers[provider.identity.Public().Hex()]
	client.ingestTower(visible)
	if q, err := protocol.DecodeTower(visible, chain.Regtest, time.Now().Unix()); err != nil || !q.Public {
		t.Fatal("public opt-in not announced", err)
	}
	provider.Config.PublicWatchtower = false
	if err := provider.advertiseTower(); err != nil {
		t.Fatal(err)
	}
	hidden := provider.s.Towers[provider.identity.Public().Hex()]
	if hidden.CreatedAt <= visible.CreatedAt {
		t.Fatal("withdrawal cannot replace public announcement")
	}
	client.ingestTower(hidden)
	client.ingestTower(visible)
	status := client.Status()
	if len(status.Watchtowers) != 1 || status.Watchtowers[0].Public {
		t.Fatal("old public announcement resurrected after opt-out")
	}
	publicEvents := 0
	for _, d := range provider.s.Outbox {
		if d.Event.Kind == transport.TowerKind {
			publicEvents++
			if d.Event.ID != hidden.ID {
				t.Fatal("stale announcement queued")
			}
		}
	}
	if publicEvents != 1 {
		t.Fatal("missing public withdrawal")
	}
	if _, err := provider.Command(context.Background(), Request{Method: "pause", Params: json.RawMessage(`{"paused":true}`)}); err == nil {
		t.Fatal("daemon can still pause")
	}
}

func TestRealDiscoveredTraderWatchtowerAndOfferBalance(t *testing.T) {
	h := newHarness(t, 50)
	tower := h.engines["tower"]
	// A trading wallet simultaneously serves rescue jobs, with no public listing.
	tower.Config.Mode = "trader"
	tower.Config.Tower = TowerConfig{}
	tower.Config.RescueFeeBPS = 125
	cfg := h.configs["tower"]
	cfg.Mode = "trader"
	cfg.Tower = TowerConfig{}
	cfg.RescueFeeBPS = 125
	h.configs["tower"] = cfg
	for _, name := range []string{"maker", "taker"} {
		h.engines[name].Config.Tower = TowerConfig{}
	}
	h.command("maker", "tower.resolve", map[string]string{"pubkey": tower.ownTower().Npub})
	h.tick("maker", "tower", "maker")
	maker := h.engines["maker"]
	for _, sell := range []string{"btc", "blake"} {
		empty, _ := json.Marshal(map[string]any{"sell": sell, "sell_amount": 1000000, "buy_amount": 2000000})
		if _, err := tower.Command(h.ctx, Request{Method: "offer.create", Params: empty}); err == nil || len(tower.s.Offers) != 0 {
			t.Fatal("empty trading wallet published offer", err)
		}
		raw, _ := json.Marshal(map[string]any{"sell": sell, "sell_amount": 10000000000, "buy_amount": 1000000})
		before := len(maker.s.Offers)
		if _, err := maker.Command(h.ctx, Request{Method: "offer.create", Params: raw}); err == nil || !strings.Contains(err.Error(), "funding fee") || len(maker.s.Offers) != before {
			t.Fatal("unfunded offer accepted", err)
		}
	}
	o := h.command("maker", "offer.create", map[string]any{"sell": "btc", "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": 125, "tower_pubkey": tower.ownTower().Npub}).(protocol.Offer)
	if o.Tower == nil || o.Tower.Verify() != nil || o.Tower.PubKey != tower.identity.Public().Hex() {
		t.Fatal("provider quote not pinned")
	}
	h.tick("maker", "taker")
	id := h.command("taker", "swap.take", map[string]string{"maker": o.Maker, "id": o.ID}).(map[string]string)["id"]
	h.until("protected long funding", func() bool { return h.swap("taker", id).LongSent }, func() { h.tick("taker", "maker", "tower") })
	if len(tower.s.TowerJobs) == 0 || !h.swap("taker", id).LongSent {
		t.Fatal("trading watchtower did not acknowledge and enable funding")
	}
	tower.s.Paused = true
	if err := tower.save(); err != nil {
		t.Fatal(err)
	}
	h.offline("tower")
	cfg.RescueFeeBPS = 250
	var ready []chain.ID
	cfg.ChainReady = func(id chain.ID, height uint32) {
		if height == 0 {
			t.Fatal("ready chain had no observed height")
		}
		ready = append(ready, id)
	}
	h.configs["tower"] = cfg
	h.online("tower")
	if len(ready) != 2 || ready[0] != chain.BTC || ready[1] != chain.Blake {
		t.Fatal("chains did not report readiness separately", ready)
	}
	if h.engines["tower"].ownTower().BPS != 250 {
		t.Fatal("new quote not applied on restart")
	}
	if h.engines["tower"].Status().Paused {
		t.Fatal("legacy pause survived reopen")
	}
	if len(h.engines["tower"].s.TowerJobs) == 0 {
		t.Fatal("watchtower jobs did not survive restart")
	}
	// Stop both traders before revelation and let the trading watchtower refund
	// the already funded long contract using its own generated payout script.
	long := h.swap("taker", id).Long
	h.offline("maker")
	h.offline("taker")
	h.mine(long.Chain, long.RefundHeight+protocol.RefundGrace-h.height(long.Chain))
	h.tick("tower")
	h.minePending()
	h.tick("tower")
	h.online("taker")
	h.tick("taker")
	if h.swap("taker", id).Stage != "refunded" {
		t.Fatal("trading watchtower did not execute delayed rescue", h.swap("taker", id).Stage)
	}
	for _, job := range h.engines["tower"].s.TowerJobs {
		if job.Job.BPS != 125 {
			t.Fatal("accepted rescue fee changed")
		}
		if job.Confirmed < 2 {
			t.Fatal("rescue did not confirm")
		}
	}
}

func TestDiscoveryMailboxIsSeparateBoundedAndExpires(t *testing.T) {
	provider, client := discoveryEngine(t), discoveryEngine(t)
	for i := 0; i < 10001; i++ {
		client.s.Seen[fmt.Sprint(i)] = "existing protocol message"
	}
	if err := client.resolveTower(provider.ownTower().Npub); err != nil {
		t.Fatal(err)
	}
	for _, d := range client.s.Outbox {
		if err := provider.receive(d.Event); err != nil {
			t.Fatal(err)
		}
		if err := provider.receive(d.Event); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range provider.s.Outbox {
		if d.Type != "tower-quote" || d.Expires == 0 {
			t.Fatal("discovery created durable ack")
		}
		if err := client.receive(d.Event); err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.s.Seen) != 0 || len(client.s.Seen) != 10001 || len(provider.s.DiscoverySeen) != 1 || len(client.s.DiscoverySeen) != 1 {
		t.Fatal("discovery consumes trading inbox")
	}
	if err := client.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.s.Outbox) != 1 || len(provider.s.Outbox) != 0 {
		t.Fatal("only the published query should remain until expiry")
	}
	for key := range client.s.DiscoverySeen {
		client.s.DiscoverySeen[key] = time.Now().Unix() - 1
	}
	for _, delivery := range client.s.Outbox {
		delivery.Expires = time.Now().Unix() - 1
	}
	client.pruneDiscovery()
	if len(client.s.DiscoverySeen) != 0 || len(client.s.Outbox) != 0 {
		t.Fatal("discovery state never expires")
	}
}

func TestOfflineFavoriteQueriesOnlyOncePerExpiryPeriod(t *testing.T) {
	provider, client := discoveryEngine(t), discoveryEngine(t)
	client.Config.FavoriteWatchtowers = []string{provider.ownTower().Npub}
	for i := 0; i < 5; i++ {
		if err := client.refreshFavoriteTowers(); err != nil {
			t.Fatal(err)
		}
		if err := client.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.s.Outbox) != 1 {
		t.Fatal("published query was recreated")
	}
	for _, d := range client.s.Outbox {
		if !d.Published {
			t.Fatal("query not published")
		}
		d.Expires = time.Now().Unix() - 1
	}
	client.pruneDiscovery()
	if err := client.refreshFavoriteTowers(); err != nil {
		t.Fatal(err)
	}
	if len(client.s.Outbox) != 1 {
		t.Fatal("expired lookup was not renewed")
	}
	for _, d := range client.s.Outbox {
		if d.Published {
			t.Fatal("new lookup inherited old delivery")
		}
	}
}
