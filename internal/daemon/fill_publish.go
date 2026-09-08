package daemon

import (
	"encoding/json"
	"errors"
	"maps"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func parentPublicOffer(p ParentOrder) protocol.Offer {
	o := p.Offer
	o.Revision, o.Available = p.Quantities.Revision, p.Quantities.Available
	switch {
	case p.Quantities.Closed:
		o.Status = "cancelled"
	case o.Available > 0:
		o.Status = "open"
	case p.Quantities.Reserved > 0 || p.Quantities.Committed > 0:
		o.Status = "reserved"
	case p.Quantities.Released > 0:
		o.Status = "cancelled"
	default:
		o.Status = "filled"
	}
	return o
}

// prepareParentPublication uses one wall-clock timestamp per parent address.
// Pending intermediate ledger revisions are coalesced; no future timestamps or
// content-revision override of Nostr's CreatedAt/ID ordering is introduced.
func (e *Engine) prepareParentPublication(p ParentOrder, now int64) (ParentOrder, *nostr.Event, error) {
	if p.RestoreHold || p.SignedRevision == p.Quantities.Revision || now <= p.LastSignedAt {
		return p, nil, nil
	}
	event, err := e.signOffer(parentPublicOffer(p), nostr.Timestamp(now))
	if err != nil {
		return p, nil, err
	}
	p.SignedRevision, p.LastSignedAt = p.Quantities.Revision, now
	return p, &event, nil
}

func (e *Engine) publishParent(id string) error {
	p := e.s.ParentOrders[id]
	if p == nil {
		return nil
	}
	next, event, err := e.prepareParentPublication(*p, time.Now().Unix())
	if err != nil || event == nil {
		return err
	}
	if err := e.stageParentOffer(next, *event); err != nil {
		return err
	}
	*p = next
	return nil
}

// The locked tick saves all staged events before dispatchPublications can send
// them. New acceptance requires SignedRevision==Quantities.Revision, so a
// deferred next-second revision cannot grant quantity from a stale relay view.
func (e *Engine) publishPendingParents() error {
	for id := range e.s.ParentOrders {
		if err := e.publishParent(id); err != nil {
			return err
		}
	}
	return nil
}

// A child transition can revise a cancelled parent after its previous public
// record went cold, including after a reorg reactivates the child. Transfer only
// the exact public/history owners into this publication transaction. Never
// expose their old event as new authority or advance SignedRevision on failure.
func (e *Engine) stageParentOffer(parent ParentOrder, event nostr.Event) error {
	if parent.RestoreHold || parent.Offer.Maker != e.identity.Public().Hex() || parent.Offer.Network.Normalized() != e.Config.Network {
		return errors.New("parent publication lacks current local authority")
	}
	var records []storage.ArchiveRecord
	for _, kind := range []string{"offers", "order_records"} {
		if _, err := e.collectArchiveActivation(kind, parent.Offer.ID, map[string]bool{}, &records); err != nil {
			return err
		}
	}
	for _, record := range records {
		var source protocol.Offer
		if record.Kind == "offers" {
			var previous nostr.Event
			if err := json.Unmarshal(record.Data, &previous); err != nil {
				return err
			}
			var err error
			source, err = historicalOffer(previous)
			if err != nil {
				return err
			}
		} else {
			var previous OrderRecord
			if err := json.Unmarshal(record.Data, &previous); err != nil {
				return err
			}
			source = previous.Offer
		}
		if source.ID != parent.Offer.ID || source.Maker != parent.Offer.Maker || source.Network.Normalized() != parent.Offer.Network.Normalized() || source.EconomicsDigest() != parent.Economics {
			return errors.New("archived publication belongs to another parent")
		}
	}
	// Plan decoding, semantic origins and ownership counters against private
	// copies. A bad later companion cannot partially promote an earlier one.
	staged := Engine{s: e.s, Config: e.Config, identity: e.identity, vault: e.vault, archiveRead: e.archiveRead}
	staged.s.Offers = maps.Clone(e.s.Offers)
	if staged.s.Offers == nil {
		staged.s.Offers = map[string]nostr.Event{}
	}
	staged.s.OrderRecords = maps.Clone(e.s.OrderRecords)
	staged.s.OwnPublicVersions = maps.Clone(e.s.OwnPublicVersions)
	staged.s.PublicVersions = maps.Clone(e.s.PublicVersions)
	staged.s.Book = maps.Clone(e.s.Book)
	if e.s.Capacity != nil {
		capacity := *e.s.Capacity
		capacity.Archived.Kinds = maps.Clone(capacity.Archived.Kinds)
		staged.s.Capacity = &capacity
	}
	staged.archivePuts = maps.Clone(e.archivePuts)
	staged.archiveDeletes = maps.Clone(e.archiveDeletes)
	staged.archiveOrigins = maps.Clone(e.archiveOrigins)
	for _, record := range records {
		if err := staged.promoteArchived(record); err != nil {
			return err
		}
	}
	// Retaining the authenticated ordering floor may also read/promote a cold
	// owner. Complete that work privately, including a stronger cold floor that
	// deliberately stays cold. Do not repeat ingestion after committing owners.
	staged.s.Offers[parent.Offer.ID] = event
	if err := staged.retainPublicOffer(event, parentPublicOffer(parent)); err != nil {
		return err
	}
	e.s.Offers, e.s.OrderRecords, e.s.Capacity = staged.s.Offers, staged.s.OrderRecords, staged.s.Capacity
	e.s.OwnPublicVersions, e.s.PublicVersions, e.s.Book = staged.s.OwnPublicVersions, staged.s.PublicVersions, staged.s.Book
	e.archivePuts, e.archiveDeletes, e.archiveOrigins = staged.archivePuts, staged.archiveDeletes, staged.archiveOrigins
	e.stageOfferRecords(parentPublicOffer(parent), event)
	e.queueEvent(event)
	return nil
}
