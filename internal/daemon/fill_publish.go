package daemon

import (
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
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
	*p = next
	e.stageOffer(parentPublicOffer(next), *event)
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
