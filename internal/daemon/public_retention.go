package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
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
func (e *Engine) retainPublicOffer(event nostr.Event, offer protocol.Offer) {
	if e.s.PublicVersions == nil {
		e.s.PublicVersions = map[string]PublicVersion{}
	}
	if e.s.Book == nil {
		e.s.Book = map[string]nostr.Event{}
	}
	key := offer.Maker + ":" + offer.ID
	previous, known := e.s.PublicVersions[key]
	if current, ok := e.s.Book[key]; ok && (!known || newerPublic(current, previous)) {
		previous = PublicVersion{CreatedAt: current.CreatedAt, ID: current.ID.Hex()}
		known = true
	}
	if known && !newerPublic(event, previous) {
		return
	}
	own := offer.Maker == e.identity.Public().Hex()
	if !known && !own && (len(e.s.PublicVersions) >= publicIdentityLimit || len(e.s.Book) >= publicOrderLimit) {
		e.s.PublicLimited = true
		return
	}
	e.s.PublicVersions[key] = PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}
	e.s.Book[key] = event
}
func (e *Engine) prunePublicOffers() {
	if e.s.PublicVersions == nil {
		e.s.PublicVersions = map[string]PublicVersion{}
	}
	for key, event := range e.s.Book {
		previous, known := e.s.PublicVersions[key]
		if !known || newerPublic(event, previous) {
			e.s.PublicVersions[key] = PublicVersion{CreatedAt: event.CreatedAt, ID: event.ID.Hex()}
		}
		var offer protocol.Offer
		if json.Unmarshal([]byte(event.Content), &offer) == nil && (offer.Expires <= time.Now().Unix() || offer.Status == "cancelled" || offer.Status == "filled") && offer.Maker != e.identity.Public().Hex() {
			delete(e.s.Book, key)
		}
	}
}
