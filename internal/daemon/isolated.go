package daemon

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// During a peer outage, absence is never evidence. Only a witnessed preimage
// permits an isolated claim. A locally generated secret or a signed SelfClaim
// saved before its first broadcast is insufficient. Refunds and funding wait for
// fresh observations of BOTH chains, including the incoming-spend scan.
func (e *Engine) advanceIsolatedSwap(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	incoming := s.Short
	if s.Role == "maker" {
		incoming = s.Long
	}
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		if !e.fresh(c.Chain) {
			continue
		}
		o, ok := observation(all, c)
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
		if ok {
			if secret, known := contract.ExtractSecret(c, o.Tx); known {
				s.Secret = hex.EncodeToString(secret)
				s.SecretObserved = true
				s.SecretExposed = true
				if c.Chain == incoming.Chain {
					s.IncomingClaimSeen = true
				}
			}
		}
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
