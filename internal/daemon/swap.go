package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"time"
)

func (e *Engine) makeJob(s *Swap, c contract.HTLC, kind string, observe *contract.HTLC, lock uint32) (protocol.Job, error) {
	key, err := e.swapKey(c.Chain, s.ID)
	if err != nil {
		return protocol.Job{}, err
	}
	towerScript, err := hex.DecodeString(s.protection().Scripts[c.Chain])
	if err != nil {
		return protocol.Job{}, err
	}
	job := protocol.Job{Network: e.Config.Network, SwapID: s.ID, Owner: e.identity.Public().Hex(), TermsHash: protocol.Digest(s.Terms), Kind: kind, Target: c, Observe: observe, ScanFrom: 1, Lock: lock, BPS: s.protection().BPS, Payout: hex.EncodeToString(e.scripts[c.Chain]), TowerScript: hex.EncodeToString(towerScript)}
	if e.Config.Network != chain.Regtest {
		job.ScanFrom = s.Terms.StartHeights[c.Chain]
		if observe != nil {
			job.ObserveScanFrom = s.Terms.StartHeights[observe.Chain]
		}
	}
	job.ID = protocol.Digest([]string{s.ID, job.Owner, kind, string(c.Chain), c.TxID})
	for _, fee := range protocol.RescueFees {
		tx, err := contract.Spend(c, key, e.scripts[c.Chain], fee, kind == "refund", lock, towerScript, protocol.Bounty(c.Amount, job.BPS), nil)
		if err != nil {
			return job, err
		}
		job.Templates = append(job.Templates, contract.Hex(tx))
	}
	return job, job.Validate(s.protection().Scripts, job.BPS)
}
func (e *Engine) prepare(s *Swap, own contract.HTLC) error {
	key, err := e.swapKey(own.Chain, s.ID)
	if err != nil {
		return err
	}
	for _, fee := range protocol.RescueFees {
		tx, err := contract.Spend(own, key, e.scripts[own.Chain], fee, true, own.RefundHeight, nil, 0, nil)
		if err != nil {
			return err
		}
		s.SelfRefunds = append(s.SelfRefunds, contract.Hex(tx))
	}
	if s.protection().BPS > 0 {
		refund, err := e.makeJob(s, own, "refund", nil, own.RefundHeight+protocol.RefundDelay(e.Config.Network))
		if err != nil {
			return err
		}
		s.Jobs = append(s.Jobs, refund)
		if s.Role == "maker" {
			short := s.Short
			claim, err := e.makeJob(s, s.Long, "claim", &short, s.Terms.Takeover)
			if err != nil {
				return err
			}
			s.Jobs = append(s.Jobs, claim)
		}
		for _, job := range s.Jobs {
			if err = e.queue(s.protection().PubKey, "tower-job", s.ID, job); err != nil {
				return err
			}
		}
	}
	return e.save()
}
func towerReady(s *Swap) bool {
	for _, job := range s.Jobs {
		if receipt, ok := s.Receipts[job.ID]; !ok || receipt.Digest != protocol.Digest(job) {
			return false
		}
	}
	return true
}
func observation(all map[chain.ID]map[string]chain.Observation, c contract.HTLC) (chain.Observation, bool) {
	o, ok := all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)]
	return o, ok
}
func (e *Engine) advanceSwap(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	var revealError error
	if s.Terms == nil {
		e.expirePendingRequest(s, time.Now().Unix())
		return nil
	}
	if err := e.rememberSwapWitnesses(s, all); err != nil {
		return err
	}
	if err := s.Terms.Validate(); err != nil {
		return err
	}
	if (s.Stage == "expired before maker funding" && s.ShortFunding == "") || (s.Stage == "expired before funding" && s.LongFunding == "") {
		return nil // Safe expiry is final even if a reorg moves the clock back.
	}
	if !e.fresh(chain.BTC) || !e.fresh(chain.Blake) {
		return e.advanceIsolatedSwap(ctx, s, all)
	}
	// Reconcile prepared transactions even in older snapshots whose broadcast
	// succeeded before the sent flag was saved. Lookup errors are not absence.
	published, raw, fundingChain := s.LongSent, s.LongFunding, s.Long.Chain
	if s.Role == "maker" {
		published, raw, fundingChain = s.ShortSent, s.ShortFunding, s.Short.Chain
	}
	if raw != "" {
		tx, err := contract.Parse(raw)
		if err != nil {
			return err
		}
		_, err = e.nodes[fundingChain].Transaction(ctx, tx.TxHash().String())
		if err != nil {
			if !chain.TransactionNotFound(err) {
				return err
			}
			if published {
				// Publication was durably authorized before network IO. Resume
				// that exact transaction even after the new-funding deadline.
				if err = e.save(); err != nil {
					return err
				}
				if err = e.broadcast(ctx, fundingChain, raw); err != nil {
					return err
				}
			}
		} else if !published {
			if err = e.recordFunding(s); err != nil {
				return err
			}
		}
	}
	longObs, longSpent := observation(all, s.Long)
	shortObs, shortSpent := observation(all, s.Short)
	s.LongSpend = ""
	s.ShortSpend = ""
	s.LongConfirmations = 0
	s.ShortConfirmations = 0
	s.TowerPaid = 0
	s.TowerPayments = map[chain.ID]int64{}
	if longSpent {
		s.LongSpend = longObs.TxID
		s.LongConfirmations = longObs.Confirmations
	}
	if shortSpent {
		s.ShortSpend = shortObs.TxID
		s.ShortConfirmations = shortObs.Confirmations
	}
	for _, job := range s.Jobs {
		obs, ok := observation(all, job.Target)
		if ok && obs.Confirmations >= e.Config.Network.Confirmations() {
			for _, raw := range job.Templates {
				tx, err := contract.Parse(raw)
				if err != nil {
					return err
				}
				if tx.TxHash().String() == obs.TxID {
					s.TowerPaid += protocol.Bounty(job.Target.Amount, job.BPS)
					s.TowerPayments[job.Target.Chain] += protocol.Bounty(job.Target.Amount, job.BPS)
					break
				}
			}
		}
	}
	if longSpent && shortSpent && longObs.Confirmations >= e.Config.Network.Confirmations() && shortObs.Confirmations >= e.Config.Network.Confirmations() {
		_, lc := contract.ExtractSecret(s.Long, longObs.Tx)
		_, sc := contract.ExtractSecret(s.Short, shortObs.Tx)
		if lc && sc {
			s.Stage = "completed"
			if s.Role == "maker" {
				event := e.s.Offers[s.Terms.Offer().ID]
				o := s.Terms.Offer()
				if event.Content != "" {
					current, err := protocol.DecodeOffer(event, int64(event.CreatedAt))
					if err == nil && current.Status != "filled" {
						o.Status = "filled"
						o.Reservation = s.ID
						if err = e.publishOffer(o); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}
		if !lc && !sc {
			s.Stage = "refunded"
			return nil
		}
		s.Stage = "contested outcome"
		return errors.New("claim/refund split: inspect chain outcomes and deadline assumptions")
	}
	own, incoming := s.Long, s.Short
	ownSpent, incomingSpent := longSpent, shortSpent
	ownObs, incomingObs := longObs, shortObs
	if s.Role == "maker" {
		own, incoming = s.Short, s.Long
		ownSpent, incomingSpent = shortSpent, longSpent
		ownObs, incomingObs = shortObs, longObs
	}
	if ownSpent && ownObs.Confirmations >= e.Config.Network.Confirmations() {
		if _, claimed := contract.ExtractSecret(own, ownObs.Tx); !claimed && (incoming.TxID == "" || !incomingSpent) {
			s.Stage = "refunded"
			return nil
		}
	}
	if own.TxID == "" && incomingSpent && incomingObs.Confirmations >= e.Config.Network.Confirmations() {
		if _, claimed := contract.ExtractSecret(incoming, incomingObs.Tx); !claimed {
			s.Stage = "aborted; counterparty refunded"
			return nil
		}
	}
	s.Stage = "awaiting chain confirmations"
	// Construct and durably store funding/refunds before publishing any spend.
	if s.Role == "taker" && s.LongFunding == "" {
		if err := e.gate(s.Terms, "fund-long"); err != nil {
			s.Stage = "expired before funding"
			return err
		}
		tx, err := e.fundReserved(ctx, s.Long, "swap/"+s.ID)
		if err != nil {
			return err
		}
		s.LongFunding = contract.Hex(tx)
		s.Long.TxID = tx.TxHash().String()
		own = s.Long
		if err = e.prepare(s, own); err != nil {
			return err
		}
	}
	if s.Role == "maker" && s.ShortFunding == "" {
		if err := e.gate(s.Terms, "fund-short"); err != nil {
			s.Stage = "expired before maker funding"
			offer := s.Terms.Offer()
			delete(e.s.CoinReservations, "offer/"+offer.ID)
			if _, ok := e.s.Offers[offer.ID]; ok {
				offer.Status, offer.Reservation = "cancelled", s.ID
				if err := e.publishOffer(offer); err != nil {
					return err
				}
			}
			return err
		}
		ready, err := e.funded(ctx, s.Long)
		if err != nil {
			return err
		}
		if !ready {
			s.Stage = "awaiting taker funding"
			return nil
		}
		tx, err := e.fundReserved(ctx, s.Short, "offer/"+s.Terms.Offer().ID)
		if err != nil {
			return err
		}
		s.ShortFunding = contract.Hex(tx)
		s.Short.TxID = tx.TxHash().String()
		own = s.Short
		if err = e.prepare(s, own); err != nil {
			return err
		}
	}
	if (s.Role == "taker" && !s.LongSent) || (s.Role == "maker" && !s.ShortSent) {
		if len(s.SelfRefunds) != len(protocol.RescueFees) {
			return errors.New("funding blocked: refund bundle is incomplete")
		}
		if s.protection().BPS > 0 {
			expected := 1
			if s.Role == "maker" {
				expected = 2
			}
			if len(s.Jobs) != expected {
				return errors.New("funding blocked: rescue bundle is incomplete")
			}
		}
		if !towerReady(s) {
			s.Stage = "awaiting durable tower receipt"
			return nil
		}
		phase := "fund-long"
		raw := s.LongFunding
		id := s.Long.Chain
		if s.Role == "maker" {
			phase = "fund-short"
			raw = s.ShortFunding
			id = s.Short.Chain
		}
		if err := e.gate(s.Terms, phase); err != nil {
			return err
		}
		if s.Role == "maker" {
			ready, err := e.funded(ctx, s.Long)
			if err != nil {
				return err
			}
			if !ready {
				return errors.New("taker funding no longer confirmed")
			}
		}
		// Commit publication and its peer notification together before sending
		// anything. A crash or ambiguous broadcast response must remain resumable.
		if err := e.recordFunding(s); err != nil {
			return err
		}
		if err := e.broadcast(ctx, id, raw); err != nil {
			return err
		}
		s.Stage = "funding broadcast"
		return nil
	}
	// Only the taker can perform the first revelation, after checking both UTXOs.
	if s.Role == "taker" && !s.SecretExposed && s.SelfClaim == "" {
		longReady, err := e.funded(ctx, s.Long)
		if err != nil {
			return err
		}
		shortReady, err := e.funded(ctx, s.Short)
		if err != nil {
			return err
		}
		if longReady && shortReady {
			if err = e.gate(s.Terms, "reveal"); err != nil {
				revealError = err
				s.Stage = "awaiting refund deadline"
			} else {
				secret, err := hex.DecodeString(s.Secret)
				if err != nil {
					return err
				}
				key, err := e.swapKey(s.Short.Chain, s.ID)
				if err != nil {
					return err
				}
				tx, err := contract.Spend(s.Short, key, e.scripts[s.Short.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
				if err != nil {
					return err
				}
				s.SelfClaim = contract.Hex(tx)
				s.SecretExposed = true
				if err = e.save(); err != nil {
					return err
				}
			}
		}
	}
	if s.Role == "maker" && s.SecretExposed && s.SelfClaim == "" && !incomingSpent {
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
		if err = e.save(); err != nil {
			return err
		}
	}
	if s.SelfClaim != "" && (!incomingSpent || incomingObs.Confirmations < e.Config.Network.Confirmations()) {
		s.Stage = "claiming"
		if err := e.save(); err != nil {
			return err
		}
		if err := e.broadcastOwner(ctx, s, incoming.Chain, false); err != nil {
			return err
		}
	}
	if incomingSpent && incomingObs.Confirmations >= e.Config.Network.Confirmations() {
		s.Stage = "awaiting counterparty claim"
	}
	if e.fresh(chain.BTC) && e.fresh(chain.Blake) && !s.IncomingClaimSeen && refundReplaceable(own, ownSpent, ownObs) && own.TxID != "" && e.eligible(own.Chain, own.RefundHeight) && len(s.SelfRefunds) > 0 {
		if incomingSpent {
			if _, claimed := contract.ExtractSecret(incoming, incomingObs.Tx); claimed {
				s.Stage = "awaiting counterparty claim"
				return nil
			}
		}
		s.Stage = "refunding"
		if err := e.save(); err != nil {
			return err
		}
		return e.broadcastOwner(ctx, s, own.Chain, true)
	}
	return revealError
}

// The sent flag records an irrevocable decision to publish, not an RPC receipt.
// The signed transaction and notification can both be retried after restart.
func (e *Engine) recordFunding(s *Swap) error {
	raw, kind, peer := s.LongFunding, "long-funded", s.Terms.Offer().Maker
	if s.Role == "maker" {
		raw, kind, peer = s.ShortFunding, "short-funded", s.Request.Taker
	}
	if err := e.queue(peer, kind, s.ID, fundingMessage{protocol.Digest(s.Terms), raw}); err != nil {
		return err
	}
	if s.Role == "taker" {
		s.LongSent = true
	} else {
		s.ShortSent = true
	}
	return e.save()
}
func (e *Engine) advanceTower(ctx context.Context, all map[chain.ID]map[string]chain.Observation) error {
	if err := e.rememberTowerWitnesses(all); err != nil {
		return err
	}
	estimates := map[chain.ID]chain.FeeEstimate{}
	for _, state := range e.s.TowerJobs {
		job := state.Job
		if state.Expired {
			continue
		}
		state.Error = ""
		if !e.fresh(job.Target.Chain) || (job.Kind == "refund" && (!e.fresh(chain.BTC) || !e.fresh(chain.Blake))) {
			state.Error = "chain observations unavailable; recovery held"
			continue
		}
		if err := job.Validate(e.ownTower().Scripts, job.BPS); err != nil {
			state.Error = err.Error()
			continue
		}
		if _, ok := all[job.Target.Chain]; !ok {
			state.Error = "target-chain scan unavailable"
			continue
		}
		obs, spent := observation(all, job.Target)
		state.Confirmed = 0
		if spent && obs.Confirmations > 0 {
			state.Confirmed = obs.Confirmations
			state.Broadcast = obs.TxID
			continue
		}
		if !e.eligible(job.Target.Chain, job.Lock) || (job.Kind == "claim" && state.Secret == "") {
			continue
		}
		if time.Now().Unix()-state.LastAttempt < 5 {
			continue
		}
		if job.Kind == "claim" && e.clocks[job.Target.Chain] >= job.Target.RefundHeight {
			state.Error = "refund now eligible; competing claim remains valid"
		}
		index := state.Attempt / 3
		if index >= len(job.Templates) {
			index = len(job.Templates) - 1
		}
		estimate, ok := estimates[job.Target.Chain]
		if !ok {
			feeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			estimate = e.estimateFee(feeCtx, job.Target.Chain, 2)
			cancel()
			estimates[job.Target.Chain] = estimate
		}
		if suggested := estimatedTier(estimate, job.Templates); suggested > index {
			index = suggested
			state.Attempt = index * 3
		}
		if !e.fresh(job.Target.Chain) || (job.Kind == "refund" && (!e.fresh(chain.BTC) || !e.fresh(chain.Blake))) {
			state.Error = "chain source changed during fee selection; refresh recovery evidence"
			continue
		}
		tx, err := contract.Parse(job.Templates[index])
		if err != nil {
			return err
		}
		if job.Kind == "claim" {
			secret, err := hex.DecodeString(state.Secret)
			if err != nil {
				return err
			}
			if err = contract.FillSecret(job.Target, tx, secret); err != nil {
				return err
			}
		}
		id := tx.TxHash().String()
		found := false
		for _, previous := range state.Variants {
			if previous == id {
				found = true
			}
		}
		if !found {
			state.Variants = append(state.Variants, id)
		}
		state.LastAttempt = time.Now().Unix()
		state.Attempt++
		if err = e.save(); err != nil {
			return err
		}
		if err = e.broadcast(ctx, job.Target.Chain, contract.Hex(tx)); err != nil {
			state.Error = err.Error()
		} else {
			state.Broadcast = tx.TxHash().String()
		}
	}
	return nil
}

func (e *Engine) eligible(id chain.ID, lock uint32) bool {
	if !e.fresh(id) {
		return false
	}
	if lock >= protocol.TimeLockThreshold {
		return e.clocks[id] > lock
	}
	return e.heights[id] >= lock
}

func (e *Engine) gate(t *protocol.Terms, phase string) error {
	if !e.fresh(chain.BTC) || !e.fresh(chain.Blake) {
		return errors.New("both chains require fresh observations")
	}
	if e.Config.Network != chain.Regtest {
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			if t.StartHeights[id] > e.heights[id] {
				return errors.New("swap scan start is above current chain tip")
			}
		}
	}
	return t.Gate(phase, e.clocks)
}

// A registration precedes funding. Once its contract's refund grace has passed,
// an explicitly absent, never-seen funding transaction is no longer an armed
// obligation. Keep funded jobs guarded even if an indexer later loses history.
func (e *Engine) refreshTowerJobs(ctx context.Context) {
	for _, state := range e.s.TowerJobs {
		state.Expired = false
		if state.FundingSeen {
			continue
		}
		target := state.Job.Target
		if !e.fresh(target.Chain) {
			continue
		}
		tx, err := e.nodes[target.Chain].Transaction(ctx, target.TxID)
		if chain.TransactionNotFound(err) {
			deadline := uint64(target.RefundHeight) + uint64(protocol.RefundDelay(e.Config.Network))
			state.Expired = deadline <= uint64(^uint32(0)) && e.eligible(target.Chain, uint32(deadline))
		} else if err != nil {
			state.Error = err.Error()
		} else {
			funding, parseErr := contract.Parse(tx.Hex)
			script, scriptErr := target.PkScript()
			if parseErr != nil || scriptErr != nil || funding.TxHash().String() != target.TxID {
				state.Error = "invalid funding transaction returned by backend"
				continue // Bad backend data is not evidence that a real obligation is absent.
			}
			if uint64(target.Vout) >= uint64(len(funding.TxOut)) || funding.TxOut[target.Vout].Value != target.Amount || !bytes.Equal(funding.TxOut[target.Vout].PkScript, script) {
				state.Expired = true
				state.Error = "registered funding output does not match the contract"
				continue
			}
			state.FundingSeen = true
		}
	}
}

// A pending owner refund may be replaced. A peer claim in the mempool is not
// an invitation to race it with our refund after learning its secret.
func refundReplaceable(c contract.HTLC, spent bool, obs chain.Observation) bool {
	if !spent {
		return true
	}
	if obs.Confirmations != 0 || obs.Tx == nil {
		return false
	}
	_, claim := contract.ExtractSecret(c, obs.Tx)
	return !claim
}
