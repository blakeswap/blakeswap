package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

var errFundingAncestry = errors.New("funding ancestry requires current canonical confirmation; keep monitoring this child")

type fundingAncestryProof struct {
	txid  string
	point recoveryCheckpoint
}

func (e *Engine) fundingAncestryReady(s *Swap) bool {
	if s == nil || len(s.FundingParents) == 0 {
		return true
	}
	own, _, _ := localFunding(s)
	p, ok := e.fundingAncestryProofs[s.ID]
	return ok && !s.FundingAncestryHeld && p.txid == own.TxID && e.fresh(own.Chain) && e.activitySourceCurrent(own.Chain, p.point.Generation) && p.point.Height == e.heights[own.Chain]
}

func (e *Engine) setFundingAncestryHold(s *Swap, held bool) error {
	if held {
		delete(e.fundingAncestryProofs, s.ID)
	}
	if s.FundingAncestryHeld == held {
		return nil
	}
	s.FundingAncestryHeld = held
	return e.save()
}

// Unknown sources retain the old outcome with an explicit hold. A positive
// contradiction of this child's retained funding block demotes only that child
// to Committed. No outcome can return inventory or replenish permanent charges.
func (e *Engine) contradictFundingAncestry(s *Swap) error {
	s.FundingAncestryHeld = true
	delete(e.fundingAncestryProofs, s.ID)
	if s.Role == "maker" {
		f := e.s.FillRecords[s.ID]
		if f == nil {
			return errors.New("funding contradiction lacks child allocation")
		}
		if f.Allocation.EverCommitted && (f.Allocation.Disposition == FillFilled || f.Allocation.Disposition == FillReleased) {
			if _, err := e.fillSummary(s, false); err != nil {
				return err
			}
			p := e.s.ParentOrders[f.ParentID]
			if p == nil {
				return errors.New("funding contradiction lacks parent custody")
			}
			next, child, err := p.transitionFill(*f, FillCommitted, false)
			if err != nil {
				return err
			}
			*p, *f = next, child
		}
	}
	s.Stage = "funding ancestry contradicted; awaiting current child evidence"
	return e.save()
}

// One child's own confirmed funding proves the existence of every transaction
// ancestor under the selected backend's consensus view. This constant-depth
// check avoids a lifetime closure walk and any shared failed-prefix read queue.
// Each child gets its own bounded turn, including after an unrelated failure.
func (e *Engine) refreshFundingAncestry(ctx context.Context, s *Swap) (result error) {
	if len(s.FundingParents) == 0 {
		return nil
	}
	delete(e.fundingAncestryProofs, s.ID)
	defer func() {
		if result != nil {
			if err := e.setFundingAncestryHold(s, true); err != nil {
				result = err
			}
		}
	}()
	if err := validateSwapFundingParents(&e.s, engineFillReader{e}, s); err != nil {
		return err
	}
	own, raw, _ := localFunding(s)
	node := e.nodes[own.Chain]
	hasher, ok := node.(recoveryBlockHasher)
	generation := e.chainGeneration[own.Chain]
	if !ok || !e.fresh(own.Chain) || !e.activitySourceCurrent(own.Chain, generation) || e.heights[own.Chain] == 0 {
		return errFundingAncestry
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	height := e.heights[own.Chain]
	actualHeight, err := node.Height(ctx)
	if err != nil || actualHeight != height {
		return errors.Join(errFundingAncestry, err)
	}
	tip, err := hasher.BlockHash(ctx, height)
	if err != nil || tip == "" {
		return errors.Join(errFundingAncestry, err)
	}
	// Persist a known contradiction before the transaction/proof lookup can fail.
	if old := s.FundingAncestryAnchor; old != nil {
		contradicted := height < old.Height
		if !contradicted {
			hash, err := hasher.BlockHash(ctx, old.Height)
			if err != nil || hash == "" {
				return errors.Join(errFundingAncestry, err)
			}
			contradicted = hash != old.Hash
		}
		if !e.fresh(own.Chain) || !e.activitySourceCurrent(own.Chain, generation) {
			return errFundingAncestry
		}
		if contradicted {
			stable, err := hasher.BlockHash(ctx, height)
			if err != nil || stable != tip {
				return errors.Join(errFundingAncestry, err)
			}
			if err := e.contradictFundingAncestry(s); err != nil {
				return err
			}
		}
	}
	observed, err := node.Transaction(ctx, own.TxID)
	if err != nil {
		return errors.Join(errFundingAncestry, err)
	}
	tx, err := contract.Parse(observed.Hex)
	exact, parseErr := contract.Parse(raw)
	minimum := e.Config.Network.Confirmations()
	if err != nil || parseErr != nil || observed.TxID != own.TxID || tx.TxHash() != exact.TxHash() || observed.Height == 0 || observed.Height > height || observed.Confirmations < minimum || uint64(height)-uint64(observed.Height)+1 < uint64(minimum) || observed.BlockHash == "" {
		return errFundingAncestry
	}
	block, err := hasher.BlockHash(ctx, observed.Height)
	if err != nil || block != observed.BlockHash {
		return errors.Join(errFundingAncestry, err)
	}
	after, err := hasher.BlockHash(ctx, height)
	actualHeight, heightErr := node.Height(ctx)
	if heightErr != nil || actualHeight != height {
		return errors.Join(errFundingAncestry, heightErr)
	}
	if err != nil || after != tip || ctx.Err() != nil || !e.fresh(own.Chain) || e.heights[own.Chain] != height || !e.activitySourceCurrent(own.Chain, generation) {
		return errors.Join(errFundingAncestry, err, ctx.Err())
	}
	anchor := FundingAnchor{Height: observed.Height, Hash: observed.BlockHash}
	changed := s.FundingAncestryHeld || s.FundingAncestryAnchor == nil || *s.FundingAncestryAnchor != anchor
	s.FundingAncestryHeld = false
	s.FundingAncestryAnchor = &anchor
	if changed {
		if err := e.save(); err != nil {
			return err
		}
	}
	if e.fundingAncestryProofs == nil {
		e.fundingAncestryProofs = map[string]fundingAncestryProof{}
	}
	e.fundingAncestryProofs[s.ID] = fundingAncestryProof{txid: own.TxID, point: recoveryCheckpoint{Height: height, Hash: tip, Generation: generation}}
	return nil
}

// Exact already-authorized funding may resume under its existing publication
// guard. An ancestry hold never signs a new funding transaction, changes a
// template, first reveals a secret, or permits a refund. Public witness rescue
// is independent of this unavailable ancestry proof.
func (e *Engine) holdFundingAncestry(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation, cause error) error {
	if !errors.Is(cause, errFundingAncestry) {
		return cause
	}
	own, raw, sent := localFunding(s)
	if sent && raw != "" && !e.restoredSwap(s.ID) && e.fatal == nil && e.publicationReady(own.Chain, true) == nil {
		_, err := e.nodes[own.Chain].Transaction(ctx, own.TxID)
		if chain.TransactionNotFound(err) {
			if saveErr := e.save(); saveErr != nil {
				return saveErr
			}
			cause = errors.Join(cause, e.broadcast(ctx, own.Chain, raw, true))
		}
	}
	return e.holdObservedSpend(ctx, s, all, observationEvidenceError{fmt.Errorf("%w: %v", errFundingAncestry, cause)})
}

func pendingAncestryPublication(s *Swap, all map[chain.ID]map[string]chain.Observation) bool {
	_, raw, sent := localFunding(s)
	_, longSpent := observation(all, s.Long)
	_, shortSpent := observation(all, s.Short)
	return raw != "" && !sent && !longSpent && !shortSpent && !s.SecretObserved && !s.SecretExposed && s.SelfClaim == ""
}
