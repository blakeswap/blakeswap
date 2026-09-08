package daemon

import (
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

// A complete, current scan can contradict this child's saved positive outcome.
// An outage, missing map, or another child's reorg cannot. Persist the demotion
// before a funding lookup or other later IO can fail; neither this transition
// nor its eventual repair returns consumed money or makes inventory Available.
func (e *Engine) reconcileFillContradiction(s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	if s == nil || s.Role != "maker" || s.Terms == nil {
		return nil
	}
	f := e.s.FillRecords[s.ID]
	if f == nil {
		return errors.New("maker outcome lacks active child allocation")
	}
	if !f.Allocation.EverCommitted || (f.Allocation.Disposition != FillFilled && f.Allocation.Disposition != FillReleased) {
		return nil
	}
	if _, err := e.fillSummary(s, false); err != nil {
		return err
	}
	contradicted := map[chain.ID]bool{}
	var proofError error
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		if !e.fresh(c.Chain) || all[c.Chain] == nil {
			continue
		}
		o, found := observation(all, c)
		if found {
			if err := e.validateContractObservation(c, o); err != nil {
				proofError = errors.Join(proofError, err)
				continue
			}
		}
		previous := s.ShortSpend
		if c.Chain == s.Long.Chain {
			previous = s.LongSpend
		}
		if previous != "" && (!found || previous != o.TxID || o.Confirmations < e.Config.Network.Confirmations()) || previous == "" && found {
			contradicted[c.Chain] = true
		}
	}
	for id := range contradicted {
		if !e.fresh(id) {
			delete(contradicted, id)
		}
	}
	if len(contradicted) == 0 {
		if proofError != nil {
			return observationEvidenceError{proofError}
		}
		return nil
	}
	p := e.s.ParentOrders[f.ParentID]
	if p == nil {
		return errors.New("contradicted child lacks active parent accounting")
	}
	next, child, err := p.transitionFill(*f, FillCommitted, false)
	if err != nil {
		return err
	}
	*p, *f = next, child
	s.Stage = "settlement contradicted; awaiting current child evidence"
	if err := e.save(); err != nil {
		return err
	}
	if proofError != nil {
		return observationEvidenceError{proofError}
	}
	return nil
}

// A restored accepted child may have been funded after the exported checkpoint.
// Exact current own-output settlement proves that its reserved authorization
// was exercised. Account for it once without signing, publishing or clearing
// the independent parent restore hold. Unknown observations cannot take this path.
func (e *Engine) settleRestoredMakerFill(s *Swap, to FillDisposition, all map[chain.ID]map[string]chain.Observation) error {
	if s.Role != "maker" {
		return nil
	}
	if !e.restoredSwap(s.ID) || !e.recoverySwapResolved(s, all) {
		return errors.New("restored accounting requires current exact settlement evidence")
	}
	own, found := observation(all, s.Short)
	if !found || own.Confirmations < e.Config.Network.Confirmations() || e.validateContractObservation(s.Short, own) != nil {
		return errors.New("restored accounting lacks positive own-output proof")
	}
	f := e.s.FillRecords[s.ID]
	if f == nil {
		return errors.New("restored accounting lacks child allocation")
	}
	if _, err := e.fillSummary(s, false); err != nil {
		return err
	}
	p := e.s.ParentOrders[f.ParentID]
	if p == nil {
		return errors.New("restored accounting lacks parent custody")
	}
	next, child := p.clone(), *f
	var err error
	if child.Allocation.Disposition == FillReserved {
		if !child.ImportedUncertain || child.FundingDisabled {
			return errors.New("restored commitment contradicts original child custody")
		}
		next, child, err = next.transitionFill(child, FillCommitted, false)
		if err != nil {
			return err
		}
	}
	next, child, err = next.transitionFill(child, to, false)
	if err != nil {
		return err
	}
	child.ImportedUncertain = false
	*p, *f = next, child
	return nil
}
