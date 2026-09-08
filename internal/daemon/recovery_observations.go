package daemon

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// These checkpoints and payment proofs are deliberately never serialized. A
// reopened engine must obtain positive evidence again. Between bounded payment
// slices, an unchanged checkpoint (or its verified descendant) preserves work.
type recoveryCheckpoint struct {
	Height     uint32
	Hash       string
	Generation uint64
}

type recoveryBlockHasher interface {
	BlockHash(context.Context, uint32) (string, error)
}

func (e *Engine) clearRecoveryPayments(id chain.ID) {
	for key := range e.recoverySends {
		if send := e.s.Sends[key]; send == nil || send.Chain == id {
			delete(e.recoverySends, key)
		}
	}
}

// A positively contradicted checkpoint withdraws authority to stop monitoring
// affected restored obligations. Keep their last displayed outcomes for audit;
// ordinary outages and unrelated chain failures do not create these holds.
func (e *Engine) invalidateRecoverySettlements(id chain.ID) error {
	e.clearRecoveryPayments(id)
	r := e.s.Recovery
	if r == nil {
		return nil
	}
	changed := false
	mark := func(kind, key string) {
		if r.InvalidatedSettlements == nil {
			r.InvalidatedSettlements = map[string]bool{}
		}
		key = kind + "/" + key
		if !r.InvalidatedSettlements[key] {
			r.InvalidatedSettlements[key] = true
			changed = true
		}
	}
	for key := range r.Swaps {
		s := e.s.Swaps[key]
		if s == nil || e.recoverySwapInactive(s) {
			continue
		}
		for _, c := range []contract.HTLC{s.Long, s.Short} {
			if c.Chain == id && c.TxID != "" {
				mark("swap", key)
			}
		}
	}
	for key := range r.Sends {
		if send := e.s.Sends[key]; send != nil && send.Chain == id {
			mark("send", key)
		}
	}
	for key := range r.TowerJobs {
		if job := e.s.TowerJobs[key]; job != nil && job.Job.Target.Chain == id && !job.Expired {
			mark("tower", key)
		}
	}
	if changed {
		// Persist before a later bounded read or unrelated Tick phase can fail.
		// The offline Settings guard must see the same withdrawn authority.
		return e.save()
	}
	return nil
}

func (e *Engine) refreshRecoveryCheckpoint(ctx context.Context, id chain.ID) error {
	if e.s.Recovery == nil {
		return nil
	}
	if e.recoveryCheckpoints == nil {
		e.recoveryCheckpoints = map[chain.ID]recoveryCheckpoint{}
	}
	previous := e.recoveryCheckpoints[id]
	generation := e.chainGeneration[id]
	if pool, ok := e.nodes[id].(*chain.Failover); ok {
		generation = pool.Generation()
	}
	source, ok := e.nodes[id].(recoveryBlockHasher)
	if !ok {
		e.clearRecoveryPayments(id)
		delete(e.recoveryCheckpoints, id)
		return errors.New("recovery requires canonical block checkpoint support")
	}
	height := e.heights[id]
	fail := func(err error) error { e.clearRecoveryPayments(id); delete(e.recoveryCheckpoints, id); return err }
	// Observe the new tip before checking the old checkpoint's ancestry. A
	// reorg between those calls must invalidate accumulated payment proofs,
	// rather than attach old proofs to a newly returned competing tip.
	hash, err := source.BlockHash(ctx, height)
	if err != nil {
		return fail(err)
	}
	if hash == "" {
		return fail(errors.New("empty recovery chain checkpoint"))
	}
	if previous.Hash != "" {
		if height < previous.Height {
			if err := e.invalidateRecoverySettlements(id); err != nil {
				return fail(err)
			}
		} else if previous.Generation != generation {
			e.clearRecoveryPayments(id)
		} else {
			hash, err := source.BlockHash(ctx, previous.Height)
			if err != nil {
				return fail(err)
			}
			if hash == "" {
				return fail(errors.New("empty recovery ancestor checkpoint"))
			}
			if hash != previous.Hash {
				if err := e.invalidateRecoverySettlements(id); err != nil {
					return fail(err)
				}
			}
		}
	}
	// The inverse transition matters too: the candidate tip may have been
	// read on a fork that disappeared before the old-height lookup. Reject
	// that mixed snapshot instead of assigning its hash to retained proofs.
	if previous.Hash != "" {
		current, err := source.BlockHash(ctx, height)
		if err != nil {
			return fail(err)
		}
		if current == "" {
			return fail(errors.New("empty recovery chain checkpoint"))
		}
		if current != hash {
			if err := e.invalidateRecoverySettlements(id); err != nil {
				return fail(err)
			}
			return fail(errors.New("recovery tip changed during checkpoint validation"))
		}
	}
	if height == previous.Height && previous.Hash != "" && hash != previous.Hash {
		if err := e.invalidateRecoverySettlements(id); err != nil {
			return fail(err)
		}
	}
	if pool, ok := e.nodes[id].(*chain.Failover); ok && pool.Generation() != generation {
		return fail(errors.New("recovery source changed during checkpoint validation"))
	}
	e.recoveryCheckpoints[id] = recoveryCheckpoint{height, hash, generation}
	return nil
}

func (e *Engine) recoveryPaymentConfirmed(send *WalletSend, expected string, tx chain.Transaction) bool {
	if e.s.Recovery == nil || !e.s.Recovery.Sends[send.ID] {
		return false
	}
	raw, err := contract.Parse(tx.Hex)
	if err != nil || raw.TxHash().String() != expected || (tx.TxID != "" && tx.TxID != expected) {
		return false
	}
	checkpoint := e.recoveryCheckpoints[send.Chain]
	// Never infer the funding height from a cached confirmation count. The
	// backend must return a validated confirmed transaction at/before this tip.
	if checkpoint.Hash == "" || tx.Height == 0 || tx.Height > checkpoint.Height || tx.Confirmations < e.Config.Network.Confirmations() {
		return false
	}
	return checkpoint.Height-tx.Height+1 >= uint32(e.Config.Network.Confirmations())
}

func (e *Engine) recoveryContextCurrent() bool {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if e.recoveryCheckpoints[id].Hash == "" || e.recoveryCheckpoints[id] != e.recoveryReconciled[id] {
			return false
		}
	}
	for id := range e.s.Recovery.Sends {
		if !e.recoverySends[id] {
			return false
		}
	}
	return true
}

// These are irreversible local decisions made before any own funding exists.
// They remain final across restart/reorg in the normal state machine as well.
// A pending request or any prepared funding is intentionally not equivalent.
func (e *Engine) recoverySwapOwnInactive(s *Swap) bool {
	if s == nil || s.SecretObserved || s.IncomingClaimSeen || len(s.SelfRefunds) > 0 {
		return false
	}
	own, raw, sent := s.Long, s.LongFunding, s.LongSent
	if s.Role == "maker" {
		own, raw, sent = s.Short, s.ShortFunding, s.ShortSent
	}
	if own.TxID != "" || raw != "" || sent {
		return false
	}
	if s.Role == "maker" {
		child, err := e.retainedFillRecord(s.ID)
		if err != nil || !child.FundingDisabled || child.Allocation.EverCommitted || (child.Allocation.Disposition != FillRetired && child.Allocation.Disposition != FillReleased) || child.RequestDigest != protocol.Digest(s.Request) {
			return false
		}
		// ImportedUncertain cannot erase an already durable irreversible
		// refusal. Exact identity and absence of any contradictory own signed
		// authority are required; a display stage never supplies this proof.
		_, err = e.fillSummary(s, e.s.Swaps[s.ID] == nil)
		return err == nil
	}
	switch s.Stage {
	case "rejected", "expired before acceptance":
		return s.Role == "taker" && s.Terms == nil
	case "expired before funding":
		return s.Role == "taker"
	}
	return false
}
func (e *Engine) recoverySwapInactive(s *Swap) bool {
	if !e.recoverySwapOwnInactive(s) {
		return false
	}
	incoming, raw, sent := s.Short, s.ShortFunding, s.ShortSent
	if s.Role == "maker" {
		incoming, raw, sent = s.Long, s.LongFunding, s.LongSent
	}
	return incoming.TxID == "" && raw == "" && !sent
}
