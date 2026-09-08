package daemon

import (
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// Only Available belongs to a parent cancellation. Existing children retain
// their exact inputs, quantities and consumed or reserved monetary authority.
func (e *Engine) withdrawParentAvailable(id string, now int64) error {
	p := e.s.ParentOrders[id]
	if p == nil {
		return errors.New("parent authorization unavailable")
	}
	if p.Quantities.Closed {
		return nil
	}
	next := p.clone()
	var err error
	next.Quantities, err = next.Quantities.withdrawAvailable()
	if err != nil {
		return err
	}
	next, event, err := e.prepareParentPublication(next, now)
	if err != nil {
		return err
	}
	*p = next
	delete(e.s.CoinReservations, "offer/"+id)
	if event != nil {
		e.stageOffer(parentPublicOffer(next), *event)
	}
	return nil
}

func (e *Engine) expireParents(now int64) error {
	for id, p := range e.s.ParentOrders {
		if p != nil && !p.RestoreHold && !p.Quantities.Closed && p.Offer.Expires <= now {
			if err := e.withdrawParentAvailable(id, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// The clock closes a funding window; it does not prove that a transaction was
// never signed elsewhere. Require uninterrupted local origin, no own signed
// evidence, and an irreversible funding refusal saved with the returned bins.
// A reorg that later reopens the clock cannot remove this refusal.
func (e *Engine) retireUnfundedMaker(s *Swap, gateErr error) error {
	if s == nil || s.Role != "maker" || s.Terms == nil || !protocol.FundingWindowClosed(gateErr) || !e.fresh(chain.BTC) || !e.fresh(chain.Blake) || e.restoredSwap(s.ID) {
		return errors.New("child retirement lacks a positive local funding-window closure")
	}
	child := e.s.FillRecords[s.ID]
	if child == nil || child.RequestDigest != protocol.Digest(s.Request) || child.ImportedUncertain || child.Allocation.EverCommitted || s.ShortFunding != "" || s.Short.TxID != "" || s.ShortSent || len(s.SelfRefunds) != 0 || len(s.Jobs) != 0 {
		return errors.New("child retirement cannot prove that own funding was never signed")
	}
	if child.FundingDisabled {
		return nil
	}
	p := e.s.ParentOrders[child.ParentID]
	if p == nil || p.RestoreHold {
		return errors.New("child retirement lacks current parent custody")
	}
	next := p.clone()
	now := time.Now().Unix()
	if next.Offer.Expires <= now {
		var err error
		next.Quantities, err = next.Quantities.withdrawAvailable()
		if err != nil {
			return err
		}
	}
	to := FillRetired
	if next.Quantities.Closed {
		to = FillReleased
	}
	next, value, err := next.transitionFill(*child, to, true)
	if err != nil {
		return err
	}
	next, event, err := e.prepareParentPublication(next, now)
	if err != nil {
		return err
	}
	*p, *child = next, value
	s.Stage = "expired before maker funding"
	for _, delivery := range e.s.Outbox {
		if delivery.SwapID == s.ID && delivery.Type == "accepted" {
			delivery.Retired = true
		}
	}
	delete(e.s.CoinReservations, "swap/"+s.ID)
	if event != nil {
		e.stageOffer(parentPublicOffer(next), *event)
	}
	e.reconcileReservations()
	return e.save()
}

// Called only from a current positive contract-spend branch. It does not infer
// completion from a cached stage or release the nonrefundable money charge.
func (e *Engine) settleMakerFill(s *Swap, to FillDisposition) error {
	if s.Role != "maker" {
		return nil
	}
	f := e.s.FillRecords[s.ID]
	if f == nil || f.RequestDigest != protocol.Digest(s.Request) {
		return errors.New("settlement child allocation unavailable")
	}
	if f.FundingDisabled && !f.Allocation.EverCommitted {
		return nil // A late peer refund never recreates returned own allocation.
	}
	p := e.s.ParentOrders[f.ParentID]
	if p == nil {
		return errors.New("settlement parent accounting unavailable")
	}
	next, child, err := p.transitionFill(*f, to, false)
	if err != nil {
		return err
	}
	*p, *f = next, child
	return nil
}
