package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"strings"
	"time"
)

const publicOrderLimit = 1000
const publicIdentityLimit = 10000

type PublicVersion struct {
	CreatedAt nostr.Timestamp `json:"created_at"`
	ID        string          `json:"id"`
}

func newerPublic(event nostr.Event, previous PublicVersion) bool {
	return event.CreatedAt > previous.CreatedAt || (event.CreatedAt == previous.CreatedAt && (previous.ID == "" || event.ID.Hex() < previous.ID))
}

// The signed addressable ordering key survives removal of expired public rows.
// We never forget a cancellation and then accept an older open version during
// an overlapping sweep. Unknown public identities have a separate bounded
// admission budget; hitting it is reported as incomplete discovery.
func (e *Engine) retainPublicOffer(event nostr.Event, offer protocol.Offer) error {
	if e.s.PublicVersions == nil {
		e.s.PublicVersions = map[string]PublicVersion{}
	}
	if e.s.OwnPublicVersions == nil {
		e.s.OwnPublicVersions = map[string]PublicVersion{}
	}
	if e.s.Book == nil {
		e.s.Book = map[string]nostr.Event{}
	}
	key := offer.Maker + ":" + offer.ID
	own := offer.Maker == e.identity.Public().Hex()
	versions := e.s.PublicVersions
	if own {
		versions = e.s.OwnPublicVersions
	}
	previous, known := versions[key]
	cold := false
	if own && !known {
		var err error
		cold, err = e.archivedValue("own_public_versions", key, &previous)
		if err != nil {
			return err
		}
		known = cold
	}
	if own {
		if legacy, ok := e.s.PublicVersions[key]; ok && (!known || legacy.CreatedAt > previous.CreatedAt || legacy.CreatedAt == previous.CreatedAt && legacy.ID < previous.ID) {
			previous, known = legacy, true
		}
	}

	if current, ok := e.s.Book[key]; ok && (!known || newerPublic(current, previous)) {
		previous = PublicVersion{CreatedAt: current.CreatedAt, ID: current.ID.Hex()}
		known = true
	}
	if known && !newerPublic(event, previous) {
		return nil
	}
	if !known && !own && (len(e.s.PublicVersions) >= publicIdentityLimit || len(e.s.Book) >= publicOrderLimit) {
		e.s.PublicLimited = true
		return nil
	}
	if cold {
		if _, err := e.activateArchived("own_public_versions", key); err != nil {
			return err
		}
	}
	versions[key] = PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}
	if own {
		delete(e.s.PublicVersions, key)
	}
	// A later remote cancellation can advance the retained ordering floor without
	// repopulating a closed local order in the active UI or publisher maps.
	if own && e.s.Offers[offer.ID].ID == (nostr.ID{}) && (offer.Status == "filled" || offer.Status == "cancelled" || offer.Expires <= time.Now().Unix()) {
		delete(e.s.Book, key)
		return nil
	}
	e.s.Book[key] = event
	return nil
}

// Keep the strongest authenticated floor, including a cold owner. The old
// public map can be discarded only after this lookup or promotion succeeds.
func (e *Engine) retainOwnPublicVersion(key string, candidate PublicVersion) error {
	if e.s.OwnPublicVersions == nil {
		e.s.OwnPublicVersions = map[string]PublicVersion{}
	}
	previous, known := e.s.OwnPublicVersions[key]
	cold := false
	if !known {
		var err error
		cold, err = e.archivedValue("own_public_versions", key, &previous)
		if err != nil {
			return err
		}
		known = cold
	}
	if known && (previous.CreatedAt > candidate.CreatedAt || previous.CreatedAt == candidate.CreatedAt && previous.ID <= candidate.ID) {
		return nil
	}
	if cold {
		if _, err := e.activateArchived("own_public_versions", key); err != nil {
			return err
		}
	}
	e.s.OwnPublicVersions[key] = candidate
	return nil
}

func (e *Engine) prunePublicOffers() error {
	if e.s.PublicVersions == nil {
		e.s.PublicVersions = map[string]PublicVersion{}
	}
	prefix := e.identity.Public().Hex() + ":"
	for key, event := range e.s.Book {
		if strings.HasPrefix(key, prefix) {
			if legacy, ok := e.s.PublicVersions[key]; ok {
				if err := e.retainOwnPublicVersion(key, legacy); err != nil {
					return err
				}
			}
			if err := e.retainOwnPublicVersion(key, PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}); err != nil {
				return err
			}
			delete(e.s.PublicVersions, key)
		} else {
			previous, known := e.s.PublicVersions[key]
			if !known || newerPublic(event, previous) {
				e.s.PublicVersions[key] = PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}
			}
		}
		var offer protocol.Offer
		if json.Unmarshal([]byte(event.Content), &offer) == nil && (offer.Expires <= time.Now().Unix() || offer.Status == "cancelled" || offer.Status == "filled") && (offer.Maker != e.identity.Public().Hex() || e.s.Offers[offer.ID].ID == (nostr.ID{})) {
			delete(e.s.Book, key)
		}
	}
	return nil
}

// Called after owned publication has safely left the live offer map. The
// exact signed ordering floor moves with it, preventing old resweeps from
// rebuilding the active view after compaction or restart.
func (e *Engine) archiveOwnOfferView(id string, event nostr.Event) error {
	key := e.identity.Public().Hex() + ":" + id
	if legacy, ok := e.s.PublicVersions[key]; ok {
		if err := e.retainOwnPublicVersion(key, legacy); err != nil {
			return err
		}
	}
	if err := e.retainOwnPublicVersion(key, PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}); err != nil {
		return err
	}
	// Retiring an older local source cannot retire a newer independently signed
	// open view. The strongest ordering floor and visible row have distinct jobs.
	if current, ok := e.s.Book[key]; ok {
		var offer protocol.Offer
		invalid := json.Unmarshal([]byte(current.Content), &offer) != nil
		closed := invalid || offer.Expires <= time.Now().Unix() || offer.Status == "cancelled" || offer.Status == "filled"
		retired := PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}
		if closed || !newerPublic(current, retired) {
			delete(e.s.Book, key)
		}
	}

	delete(e.s.PublicVersions, key)
	if _, ok := e.s.Book[key]; ok {
		return nil
	}
	if _, active := e.s.OwnPublicVersions[key]; active {
		return e.stageArchive("own_public_versions", key)
	}
	return nil
}
func (e *Engine) compactOwnPublicVersions(remaining *int) error {
	prefix := e.identity.Public().Hex() + ":"
	// Older checkpoints may contain orphaned own floors with no Book/Offers row.
	// Migrate at most the normal batch budget, preserving newer cold evidence.
	for _, key := range sortedArchiveIDs(e.s.PublicVersions) {
		if *remaining <= 0 {
			break
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if err := e.retainOwnPublicVersion(key, e.s.PublicVersions[key]); err != nil {
			return err
		}
		delete(e.s.PublicVersions, key)
		*remaining--
	}
	for _, key := range sortedArchiveIDs(e.s.OwnPublicVersions) {
		if *remaining <= 0 {
			break
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		id := strings.TrimPrefix(key, prefix)
		if _, active := e.s.Offers[id]; active {
			continue
		}
		if _, visible := e.s.Book[key]; visible {
			continue
		}
		if err := e.stageArchive("own_public_versions", key); err != nil {
			return err
		}
		*remaining--
	}
	return nil
}
