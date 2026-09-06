package daemon

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// A successful spend observation can be the only chance to learn a preimage.
// Persist it before any unrelated lookup or refund eligibility check can return;
// a later reorg must not erase a fact that this process already witnessed.
func (e *Engine) rememberSwapWitnesses(s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	incoming := s.Short
	if s.Role == "maker" {
		incoming = s.Long
	}
	changed := false
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		obs, spent := observation(all, c)
		if !spent || obs.Tx == nil {
			continue
		}
		if secret, claimed := contract.ExtractSecret(c, obs.Tx); claimed {
			encoded := hex.EncodeToString(secret)
			changed = changed || s.Secret != encoded || !s.SecretObserved || !s.SecretExposed || (c.Chain == incoming.Chain && !s.IncomingClaimSeen)
			s.Secret, s.SecretObserved, s.SecretExposed = encoded, true, true
			if c.Chain == incoming.Chain {
				s.IncomingClaimSeen = true
			}
		}
	}
	if changed {
		return e.save()
	}
	return nil
}

// These maps contain successful, validated scan results. Source switching may
// make their canonicality stale, but cannot make an already public preimage
// private again. Target readiness is checked separately before any broadcast.
func (e *Engine) rememberTowerWitnesses(all map[chain.ID]map[string]chain.Observation) error {
	changed := false
	for _, state := range e.s.TowerJobs {
		if state.Job.Observe == nil {
			continue
		}
		c := *state.Job.Observe
		obs, spent := observation(all, c)
		if !spent || obs.Tx == nil {
			continue
		}
		if secret, claimed := contract.ExtractSecret(c, obs.Tx); claimed {
			encoded := hex.EncodeToString(secret)
			changed = changed || state.Secret != encoded
			state.Secret = encoded
		}
	}
	if changed {
		return e.save()
	}
	return nil
}

// During a peer outage, absence is never evidence. Only a witnessed preimage
// permits an isolated claim. A locally generated secret or a signed SelfClaim
// saved before its first broadcast is insufficient. Refunds and funding wait for
// fresh observations of BOTH chains, including the incoming-spend scan.
func (e *Engine) advanceIsolatedSwap(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	if err := e.rememberSwapWitnesses(s, all); err != nil {
		return err
	}
	incoming := s.Short
	if s.Role == "maker" {
		incoming = s.Long
	}
	terminalStable := terminalSwapStage(s.Stage)
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		if !e.fresh(c.Chain) {
			continue
		}
		if _, scanned := all[c.Chain]; !scanned {
			continue
		}
		o, ok := observation(all, c)
		previous := s.ShortSpend
		if c.Chain == s.Long.Chain {
			previous = s.LongSpend
		}
		if (previous != "" && (!ok || o.TxID != previous || o.Confirmations < e.Config.Network.Confirmations())) || (previous == "" && ok) {
			terminalStable = false
		}
		if c.Chain == s.Long.Chain {
			s.LongSpend = ""
			s.LongConfirmations = 0
			if ok {
				s.LongSpend = o.TxID
				s.LongConfirmations = o.Confirmations
			}
		} else {
			s.ShortSpend = ""
			s.ShortConfirmations = 0
			if ok {
				s.ShortSpend = o.TxID
				s.ShortConfirmations = o.Confirmations
			}
		}
	}
	if terminalStable {
		// An unrelated outage is not a reorg. Keep completed history terminal;
		// readiness separately identifies the peer observation as stale.
		return nil
	}
	s.Stage = "chain unavailable; funding, revelation and refunds held"
	if !s.SecretObserved || !e.fresh(incoming.Chain) {
		return errors.New(s.Stage)
	}
	if _, ok := all[incoming.Chain]; !ok {
		return errors.New("target-chain spend scan unavailable")
	}
	targetObs, targetSpent := observation(all, incoming)
	if targetSpent && targetObs.Confirmations >= e.Config.Network.Confirmations() {
		return nil
	}
	// Verify the exact agreed unspent target, without asking the unavailable peer
	// to repeat replay proofs. Its signed funding/terms remain unchanged; this
	// claim only publishes a preimage already witnessed in a valid contract spend.
	out, err := e.nodes[incoming.Chain].Output(ctx, incoming.TxID, incoming.Vout)
	if err != nil {
		return err
	}
	mempoolClaim := false
	if targetSpent && targetObs.Tx != nil && targetObs.Confirmations == 0 {
		_, mempoolClaim = contract.ExtractSecret(incoming, targetObs.Tx)
	}
	if out == nil && !mempoolClaim {
		return errors.New("claim target unavailable or already spent")
	}
	script, err := incoming.PkScript()
	if err != nil {
		return err
	}
	if out != nil && (int64(out.Value) != incoming.Amount || out.Script.Hex != hex.EncodeToString(script) || out.Confirmations < e.Config.Network.Confirmations()) {
		return errors.New("claim target differs from confirmed agreed contract")
	}
	if !e.fresh(incoming.Chain) {
		return errors.New("claim source changed after scan; refreshing target evidence")
	}
	if s.SelfClaim == "" {
		secret, err := hex.DecodeString(s.Secret)
		if err != nil {
			return err
		}
		key, err := e.swapKey(incoming.Chain, s.ID)
		if err != nil {
			return err
		}
		tx, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
		if err != nil {
			return err
		}
		s.SelfClaim = contract.Hex(tx)
	}
	s.Stage = "claiming with previously observed secret"
	if err := e.save(); err != nil {
		return err
	}
	return e.broadcastOwner(ctx, s, incoming.Chain, false)
}
