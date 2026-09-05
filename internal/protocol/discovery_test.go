package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func signedTower(t *testing.T) (nostr.Event, Tower) {
	t.Helper()
	key := nostr.Generate()
	now := time.Now().Unix()
	tower := Tower{PubKey: key.Public().Hex(), Npub: nip19.EncodeNpub(key.Public()), Name: "Provider", Network: chain.Regtest, Expires: now + TowerLifetime, BPS: 50, Public: true, Scripts: map[chain.ID]string{chain.BTC: "0014" + strings.Repeat("11", 20), chain.Blake: "0014" + strings.Repeat("22", 20)}}
	raw, _ := json.Marshal(tower)
	event := nostr.Event{Kind: transport.TowerKind, CreatedAt: nostr.Timestamp(now), Tags: nostr.Tags{{"d", chain.Regtest.Namespace()}, {"t", chain.Regtest.Namespace()}, {"expiration", strconv.FormatInt(tower.Expires, 10)}}, Content: string(raw)}
	if err := transport.Sign(&event, key); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTower(event, chain.Regtest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event, decoded
}

func TestProviderSignedQuotesRejectTamperingAndStaleDiscovery(t *testing.T) {
	event, quote := signedTower(t)
	if err := quote.Verify(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{quote.PubKey, quote.Npub} {
		pub, err := PublicKey(value)
		if err != nil || pub.Hex() != quote.PubKey {
			t.Fatal("npub round trip", err)
		}
	}
	for _, value := range []string{"", "npub1bad", nip19.EncodeNpub(nostr.PubKey{}), nip19.EncodeNsec(nostr.Generate())} {
		if _, err := PublicKey(value); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	if _, err := DecodeTower(event, chain.Mainnet, time.Now().Unix()); err == nil {
		t.Fatal("wrong network accepted")
	}
	if _, err := DecodeTower(event, chain.Regtest, quote.Expires); err == nil {
		t.Fatal("expired discovery accepted")
	}
	if _, err := DecodeTower(event, chain.Regtest, int64(event.CreatedAt)-61); err == nil {
		t.Fatal("future discovery accepted")
	}
	for name, mutate := range map[string]func(*Tower){
		"rate": func(q *Tower) { q.BPS++ }, "identity": func(q *Tower) { q.PubKey = nostr.Generate().Public().Hex() },
		"payout": func(q *Tower) {
			q.Scripts = map[chain.ID]string{chain.BTC: q.Scripts[chain.Blake], chain.Blake: q.Scripts[chain.BTC]}
		},
		"visibility": func(q *Tower) { q.Public = false }, "expiry": func(q *Tower) { q.Expires++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := quote
			mutate(&changed)
			if changed.Verify() == nil {
				t.Fatal("unapproved provider quote accepted")
			}
		})
	}
	event.Content += " "
	if _, err := DecodeTower(event, chain.Regtest, time.Now().Unix()); err == nil {
		t.Fatal("forged event accepted")
	}
}
