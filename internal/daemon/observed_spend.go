package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Proof uncertainty may hold accounting without vetoing a claim with an already
// public secret. Persistence and identity errors deliberately use other types.
type observationEvidenceError struct{ error }

type observedSpendProof struct {
	witness    chainhash.Hash
	generation uint64
	err        error
}

type observedSpendTurn struct {
	contract   contract.HTLC
	witness    chainhash.Hash
	generation uint64
}

var errObservedWorkDeferred = errors.New("observed spend proof deferred by this scan's work budget")

const observedPrevoutReads = 128
const observedPrevoutBytes = 4 << 20

// Keep only compact verification results for active contracts. Raw previous
// transactions and complete prevout vectors live for a single bounded check.
// Successful signatures remain immutable across scans; canonicality is always
// checked against the current observation separately. Failed reads retry on the
// next scan. A deferred witness gets the first work turn on the next scan, so
// a repeatedly unavailable early transaction cannot starve a later child.
func (e *Engine) resetObservedSpendWork() {
	live := make(map[contract.HTLC]bool, 2*len(e.s.Swaps))
	for _, s := range e.s.Swaps {
		live[s.Long], live[s.Short] = true, true
	}
	for c, proof := range e.observedSpendProofs {
		if !live[c] || proof.err != nil || proof.generation != e.chainGeneration[c.Chain] {
			delete(e.observedSpendProofs, c)
		}
	}
	e.observedSpendPriority = e.observedSpendNext
	if e.observedSpendPriority == nil {
		e.observedSpendPriority = e.observedSpendBefore
	}
	e.observedSpendNext, e.observedSpendBefore = nil, nil
	if p := e.observedSpendPriority; p != nil && !live[p.contract] {
		e.observedSpendPriority = nil
	}
	e.observedSpendReads, e.observedSpendBytes = 0, 0
}

func (e *Engine) deferObservedSpend(c contract.HTLC, o chain.Observation) {
	next := &e.observedSpendNext
	if e.observedSpendPriority != nil {
		// Candidates skipped before this pass's reserved turn wait until the
		// suffix has had its turns; otherwise two failed prefixes can alternate
		// forever while starving every candidate after them.
		next = &e.observedSpendBefore
	}
	if *next == nil {
		*next = &observedSpendTurn{contract: c, witness: o.Tx.WitnessHash(), generation: e.chainGeneration[c.Chain]}
	}
}

func (e *Engine) observedSpendTurn(c contract.HTLC, all map[chain.ID]map[string]chain.Observation) error {
	p := e.observedSpendPriority
	if p == nil {
		return nil
	}
	o, found := observation(all, p.contract)
	proof, proven := e.observedSpendProofs[p.contract]
	if !found || o.Tx == nil || !e.fresh(p.contract.Chain) || p.generation != e.chainGeneration[p.contract.Chain] || o.Tx.TxHash().String() != o.TxID || o.Tx.WitnessHash() != p.witness || !needsObservedPrevouts(p.contract, o.Tx) || (proven && proof.err == nil && proof.witness == p.witness && proof.generation == p.generation) || p.contract == c {
		// A disappeared, changed, stale or already proven candidate cannot hold
		// the queue. Only the exact current witness consumes its priority turn.
		e.observedSpendPriority = nil
		return nil
	}
	return errObservedWorkDeferred
}

func (e *Engine) validateContractObservation(c contract.HTLC, o chain.Observation) error {
	if o.Tx == nil || o.Tx.TxHash().String() != o.TxID {
		return observationEvidenceError{errors.New("contract observation lacks matching transaction evidence")}
	}
	if proof, ok := e.observedSpendProofs[c]; ok && proof.witness == o.Tx.WitnessHash() && proof.generation == e.chainGeneration[c.Chain] {
		if proof.err != nil {
			return observationEvidenceError{proof.err}
		}
		return nil
	}
	if err := contract.VerifyObservedSpend(c, o.Tx, nil); err != nil {
		return observationEvidenceError{err}
	}
	return nil
}

func needsObservedPrevouts(c contract.HTLC, tx *wire.MsgTx) bool {
	if c.Chain != chain.Blake || tx == nil || len(tx.TxIn) < 2 {
		return false
	}
	op, err := contract.Outpoint(c.TxID, c.Vout)
	if err != nil {
		return false
	}
	for _, in := range tx.TxIn {
		if in != nil && in.PreviousOutPoint == op && len(in.Witness) > 0 && len(in.Witness[0]) > 0 {
			sig := in.Witness[0]
			return sig[len(sig)-1] == byte(contract.UnifiedAll)
		}
	}
	return false
}

// Enrich the two legs independently. A failure on one must not suppress a
// durably recorded contradiction on the other. All nested callers reuse these
// exact witness-bound results, leaving most of the swap's budget for rescue.
func (e *Engine) prepareObservedSpends(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) {
	if e.fatal != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		o, found := observation(all, c)
		if !found || !e.fresh(c.Chain) || !needsObservedPrevouts(c, o.Tx) || o.Tx.TxHash().String() != o.TxID {
			continue
		}
		witness := o.Tx.WitnessHash()
		if proof, ok := e.observedSpendProofs[c]; ok && proof.witness == witness && proof.generation == e.chainGeneration[c.Chain] {
			continue
		}
		generation := e.chainGeneration[c.Chain]
		var spent []*wire.TxOut
		usedReads, usedBytes := e.observedSpendReads, e.observedSpendBytes
		err := e.observedSpendTurn(c, all)
		attempted := err == nil
		if err == nil {
			spent, err = e.observedPrevouts(ctx, c, o.Tx)
		}
		if errors.Is(err, errObservedWorkDeferred) && (!attempted || usedReads > 0 || usedBytes > 0) {
			// An observation that already failed with the entire fresh quota
			// must not reserve first place again ahead of waiting children.
			e.deferObservedSpend(c, o)
		}
		if err == nil {
			err = contract.VerifyObservedSpend(c, o.Tx, spent)
		}
		if !e.fresh(c.Chain) || e.chainGeneration[c.Chain] != generation {
			err = errors.New("contract proof source changed; refreshing observations")
		}
		if e.observedSpendProofs == nil {
			e.observedSpendProofs = map[contract.HTLC]observedSpendProof{}
		}
		e.observedSpendProofs[c] = observedSpendProof{witness: witness, generation: generation, err: err}
	}
}

func (e *Engine) observedPrevouts(ctx context.Context, c contract.HTLC, tx *wire.MsgTx) ([]*wire.TxOut, error) {
	if len(tx.TxIn) > observedPrevoutReads || e.nodes[c.Chain] == nil {
		return nil, errors.New("observed spend exceeds available prevout evidence")
	}
	if len(tx.TxIn)-1 > observedPrevoutReads-e.observedSpendReads {
		return nil, errObservedWorkDeferred
	}
	op, err := contract.Outpoint(c.TxID, c.Vout)
	if err != nil {
		return nil, err
	}
	pk, err := c.PkScript()
	if err != nil {
		return nil, err
	}
	spent := make([]*wire.TxOut, len(tx.TxIn))
	seen := map[wire.OutPoint]bool{}
	for i, in := range tx.TxIn {
		if in == nil || seen[in.PreviousOutPoint] {
			return nil, errors.New("invalid observed input set")
		}
		seen[in.PreviousOutPoint] = true
		if in.PreviousOutPoint == op {
			// This exact signed funding contract already authenticates its output.
			spent[i] = wire.NewTxOut(c.Amount, pk)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.fresh(c.Chain) {
			return nil, errors.New("observed prevout evidence unavailable in this pass")
		}
		if e.observedSpendReads >= observedPrevoutReads || e.observedSpendBytes >= observedPrevoutBytes {
			return nil, errObservedWorkDeferred
		}
		e.observedSpendReads++
		previous, err := e.nodes[c.Chain].Transaction(ctx, in.PreviousOutPoint.Hash.String())
		if err != nil {
			return nil, fmt.Errorf("observed previous transaction: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(previous.Hex)/2 > observedPrevoutBytes-e.observedSpendBytes {
			// Charge a rejected oversized response too. Further inputs must
			// wait; a provider cannot cause repeated oversized reads this pass.
			e.observedSpendBytes = observedPrevoutBytes
			return nil, errObservedWorkDeferred
		}
		e.observedSpendBytes += len(previous.Hex) / 2
		raw, err := contract.Parse(previous.Hex)
		if err != nil || raw.TxHash() != in.PreviousOutPoint.Hash || uint64(in.PreviousOutPoint.Index) >= uint64(len(raw.TxOut)) {
			return nil, errors.New("observed previous transaction does not authenticate input")
		}
		spent[i] = raw.TxOut[in.PreviousOutPoint.Index]
	}
	return spent, nil
}

func (e *Engine) swapObservationError(s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	var result error
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		if o, found := observation(all, c); found && e.fresh(c.Chain) {
			result = errors.Join(result, e.validateContractObservation(c, o))
		}
	}
	if result != nil {
		return observationEvidenceError{result}
	}
	return nil
}

// Save a contradiction supported by already available evidence before doing
// any prevout IO. Then allow newly verified evidence to reconcile the other leg.
func (e *Engine) reconcileObservedSwap(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	if err := e.reconcileFillContradiction(s, all); err != nil {
		if _, proofOnly := err.(observationEvidenceError); !proofOnly {
			return err
		}
	}
	e.prepareObservedSpends(ctx, s, all)
	if err := e.reconcileFillContradiction(s, all); err != nil {
		return err
	}
	return e.swapObservationError(s, all)
}

func (e *Engine) holdObservedSpend(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation, err error) error {
	if child := e.s.FillRecords[s.ID]; s.Role == "maker" && child != nil && child.FundingDisabled && !child.Allocation.EverCommitted {
		return err
	}
	if _, proofOnly := err.(observationEvidenceError); proofOnly && s.SecretObserved && e.fatal == nil {
		if s.Role == "maker" {
			if _, identityErr := e.fillSummary(s, false); identityErr != nil {
				return errors.Join(err, identityErr)
			}
		}
		return errors.Join(err, e.claimObservedSecret(ctx, s, all))
	}
	return err
}
