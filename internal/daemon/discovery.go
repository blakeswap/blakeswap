package daemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func (e *Engine) ownTower() protocol.Tower {
	pub := e.identity.Public()
	tower := protocol.Tower{PubKey: pub.Hex(), Npub: nip19.EncodeNpub(pub), Name: e.Config.Name, Network: e.Config.Network, Public: e.Config.PublicWatchtower, BPS: protocol.DefaultTowerBPS, Scripts: map[chain.ID]string{}}
	if e.Config.Mode == "tower" {
		tower.BPS = e.Config.Tower.BPS
	} else if e.Config.RescueFeeBPS != 0 {
		tower.BPS = e.Config.RescueFeeBPS
	}
	for id, script := range e.scripts {
		tower.Scripts[id] = hex.EncodeToString(script)
	}
	return tower
}

func (e *Engine) advertiseTower() error {
	if e.s.Towers == nil {
		e.s.Towers = map[string]nostr.Event{}
	}
	now := time.Now().Unix()
	for pub, event := range e.s.Towers {
		if _, err := protocol.DecodeTower(event, e.Config.Network, now); err != nil {
			delete(e.s.Towers, pub)
		}
	}
	tower := e.ownTower()
	if event, ok := e.s.Towers[e.identity.Public().Hex()]; ok && now-int64(event.CreatedAt) < 900 && e.s.TowerPublic == e.Config.PublicWatchtower {
		if previous, err := protocol.DecodeTower(event, e.Config.Network, now); err == nil && previous.BPS == tower.BPS {
			return nil
		}
	}
	if old, ok := e.s.Towers[e.identity.Public().Hex()]; ok && int64(old.CreatedAt) >= now {
		now = int64(old.CreatedAt) + 1
	}
	tower.Expires = now + protocol.TowerLifetime
	raw, err := json.Marshal(tower)
	if err != nil {
		return err
	}
	event := nostr.Event{Kind: transport.TowerKind, CreatedAt: nostr.Timestamp(now), Tags: nostr.Tags{{"d", e.Config.Network.Namespace()}, {"t", e.Config.Network.Namespace()}, {"expiration", strconv.FormatInt(tower.Expires, 10)}}, Content: string(raw)}
	if err := transport.Sign(&event, e.identity); err != nil {
		return err
	}
	// Replace undelivered announcements instead of accumulating expired retries.
	for id, delivery := range e.s.Outbox {
		if delivery.Event.Kind == transport.TowerKind {
			delete(e.s.Outbox, id)
		}
	}
	e.s.Towers[tower.PubKey] = event
	if e.Config.PublicWatchtower || e.s.TowerPublic {
		e.queueEvent(event)
	}
	e.s.TowerPublic = e.Config.PublicWatchtower
	return nil
}

func (e *Engine) ingestTower(event nostr.Event) {
	tower, err := protocol.DecodeTower(event, e.Config.Network, time.Now().Unix())
	if err != nil {
		return
	}
	if e.s.Towers == nil {
		e.s.Towers = map[string]nostr.Event{}
	}
	old, exists := e.s.Towers[tower.PubKey]
	if !exists && len(e.s.Towers) >= 1000 {
		return
	}
	if !exists || event.CreatedAt > old.CreatedAt || (event.CreatedAt == old.CreatedAt && event.ID.Hex() < old.ID.Hex()) {
		e.s.Towers[tower.PubKey] = event
	}
}

func (e *Engine) selectedTower(o protocol.Offer) (TowerConfig, error) {
	if o.TowerBPS == 0 {
		return TowerConfig{}, nil
	}
	if o.Tower != nil {
		if err := o.Tower.Verify(); err != nil {
			return TowerConfig{}, err
		}
		if o.Tower.PubKey == e.identity.Public().Hex() {
			return TowerConfig{}, errors.New("select an independent watchtower")
		}
		return *o.Tower, nil
	}
	// Preserve already published legacy CLI offers that used a configured quote.
	if o.TowerBPS != e.Config.Tower.BPS || !protocol.Hex32(e.Config.Tower.PubKey) {
		return TowerConfig{}, errors.New("watchtower quote unavailable")
	}
	return e.Config.Tower, nil
}

// Private lookup uses the existing authenticated encrypted mailbox. No provider
// advertisement is published merely because someone knows its npub.
func (e *Engine) resolveTower(value string) error {
	pub, err := protocol.PublicKey(value)
	if err != nil {
		return err
	}
	if pub == e.identity.Public() {
		return errors.New("select another wallet as your watchtower")
	}
	for _, delivery := range e.s.Outbox {
		if delivery.To == pub.Hex() && delivery.Type == "tower-query" {
			return nil
		}
	}
	if len(e.s.Outbox) >= 10000 {
		return errors.New("mailbox capacity reached")
	}
	return e.queue(pub.Hex(), "tower-query", "", map[string]int64{"period": time.Now().Unix() / 900})
}
func (e *Engine) refreshFavoriteTowers() error {
	for _, value := range e.Config.FavoriteWatchtowers {
		pub, err := protocol.PublicKey(value)
		if err != nil {
			return err
		}
		if pub == e.identity.Public() {
			continue
		}
		if event, exists := e.s.Towers[pub.Hex()]; exists {
			if tower, err := protocol.DecodeTower(event, e.Config.Network, time.Now().Unix()); err == nil && tower.Expires > time.Now().Unix()+900 {
				continue
			}
		}
		if err := e.resolveTower(value); err != nil {
			return err
		}
	}
	return nil
}

func discoveryMessage(typ string) bool { return typ == "tower-query" || typ == "tower-quote" }
func (e *Engine) pruneDiscovery() {
	if e.s.DiscoverySeen == nil {
		e.s.DiscoverySeen = map[string]int64{}
	}
	now := time.Now().Unix()
	for key, expires := range e.s.DiscoverySeen {
		if expires <= now {
			delete(e.s.DiscoverySeen, key)
		}
	}
	for key, delivery := range e.s.Outbox {
		if delivery.Expires > 0 && delivery.Expires <= now {
			delete(e.s.Outbox, key)
		}
	}
}
