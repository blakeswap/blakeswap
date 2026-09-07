package daemon

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
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
	if previous.Hash != "" {
		if previous.Generation != generation || height < previous.Height {
			e.clearRecoveryPayments(id)
		} else {
			hash, err := source.BlockHash(ctx, previous.Height)
			if err != nil {
				return fail(err)
			}
			if hash != previous.Hash {
				e.clearRecoveryPayments(id)
			}
		}
	}
	hash, err := source.BlockHash(ctx, height)
	if err != nil {
		return fail(err)
	}
	if height == previous.Height && previous.Hash != "" && hash != previous.Hash {
		e.clearRecoveryPayments(id)
	}
	if hash == "" {
		return fail(errors.New("empty recovery chain checkpoint"))
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
func recoverySwapOwnInactive(s *Swap) bool {
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
	switch s.Stage {
	case "rejected", "expired before acceptance":
		return s.Role == "taker" && s.Terms == nil
	case "expired before funding":
		return s.Role == "taker"
	case "expired before maker funding":
		return s.Role == "maker"
	}
	return false
}
func recoverySwapInactive(s *Swap) bool {
	if !recoverySwapOwnInactive(s) {
		return false
	}
	incoming, raw, sent := s.Short, s.ShortFunding, s.ShortSent
	if s.Role == "maker" {
		incoming, raw, sent = s.Long, s.LongFunding, s.LongSent
	}
	return incoming.TxID == "" && raw == "" && !sent
}
