package daemon

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/btcsuite/btcd/wire"
)

type activityReaderKey struct{}

// Called with e.mu held when starting a tracked advisory read. Only watched
// contract inputs need a durable callback; ordinary receipts stay read-only.
func (e *Engine) activityWitnessContext(ctx context.Context, id chain.ID) context.Context {
	sink := e.activityWitnessSink(ctx, id)
	if sink == nil {
		return ctx
	}
	return chain.WithSpendWitnessSink(ctx, sink)
}
func (e *Engine) activityWitnessSink(ctx context.Context, id chain.ID) func(chain.SpendWitness) error {
	wallet, network := e.Config.Name, e.Config.Network
	registered := ctx.Value(activityReaderKey{}) == e
	points := map[string]bool{}
	add := func(c contract.HTLC) {
		if c.Chain == id && c.TxID != "" {
			points[chain.OutpointKey(c.TxID, c.Vout)] = true
		}
	}
	for _, s := range e.s.Swaps {
		add(s.Long)
		add(s.Short)
	}
	for _, job := range e.s.TowerJobs {
		if job.Job.Observe != nil {
			add(*job.Job.Observe)
		}
	}
	if len(points) == 0 {
		return nil
	}
	// A decoded transaction can spend several watched contracts. Consume all
	// its facts on the first delivery, before lease reacquisition can cancel.
	// Native readers invoke the sink synchronously with this same tx pointer.
	decoded := map[*wire.MsgTx]map[string]chain.Observation{}
	delivered := map[*wire.MsgTx]bool{}
	return func(w chain.SpendWitness) error {
		if w.Tx == nil {
			return errors.New("history witness has no decoded transaction")
		}
		facts, known := decoded[w.Tx]
		if !known {
			facts = map[string]chain.Observation{}
			for _, in := range w.Tx.TxIn {
				point := chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)
				if points[point] {
					facts[point] = chain.Observation{Tx: w.Tx}
				}
			}
			decoded[w.Tx] = facts
		}
		if len(facts) == 0 {
			return nil
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if wallet != e.Config.Name || network != e.Config.Network || e.activityDrained {
			return errors.New("history witness belongs to a closed or changed wallet")
		}
		persist := e.save
		if e.activityClosed {
			if !registered || e.fatal != errEngineClosed {
				return errors.New("history witness is not a registered closing reader")
			}
			// Close has stopped protocol execution but joins this reader before
			// closing the vault. Preserve facts decoded before/during cancellation.
			persist = e.persistState
		} else if e.fatal != nil {
			return e.fatal
		}
		// Deadline/source changes cannot make a validated public preimage private.
		// Save before the backend performs any subsequent canonicality lookup.
		if delivered[w.Tx] {
			return nil
		}
		if err := e.rememberActivityFacts(map[chain.ID]map[string]chain.Observation{id: facts}, persist); err != nil {
			return err
		}
		delivered[w.Tx] = true
		return nil
	}
}

// e.mu is held. Immutable facts never grant scan readiness or confirmations.
func (e *Engine) rememberActivityFacts(facts map[chain.ID]map[string]chain.Observation, persist func() error) error {
	for _, s := range e.s.Swaps {
		if err := e.rememberSwapWitnessesWithSave(s, facts, persist); err != nil {
			return err
		}
	}
	return e.rememberTowerWitnessesWithSave(facts, persist)
}

// Also consume complete returned bytes for optional historical backends that do
// not emit incremental witnesses. Native backends emit before later I/O can fail.
func (e *Engine) rememberActivityTransaction(id chain.ID, record chain.Transaction) error {
	if record.Hex == "" {
		return nil
	}
	tx, err := contract.Parse(record.Hex)
	if err != nil || tx.TxHash().String() != record.TxID {
		return errors.New("invalid history witness transaction")
	}
	facts := map[chain.ID]map[string]chain.Observation{id: {}}
	for _, in := range tx.TxIn {
		facts[id][chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)] = chain.Observation{Tx: tx}
	}
	return e.rememberActivityFacts(facts, e.save)
}
