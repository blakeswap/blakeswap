package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

func (e *Engine) restoredSwap(id string) bool { return e.s.Recovery != nil && e.s.Recovery.Swaps[id] }
func (e *Engine) recoveryTradingReady() error {
	if err := e.archiveHold(); err != nil {
		return err
	}
	if e.s.Recovery == nil {
		return nil
	}
	if len(e.s.Recovery.InvalidatedSettlements) != 0 || e.s.Recovery.Status.State != "ready" || !e.fresh(chain.BTC) || !e.fresh(chain.Blake) || !e.recoveryContextCurrent() {
		return errors.New("wallet recovery is in progress; inspect its unresolved obligations before creating a new trade or payment")
	}
	return nil
}
func (e *Engine) recoveryOwnerPolicy(s *Swap, refund bool) error {
	// This guard is repeated at the publication boundary, including after a
	// backend changes. Only a claim reusing a public secret is independent of
	// the local funding proof; refunds and first revelation remain held.
	if (refund || !s.SecretObserved) && !e.fundingAncestryReady(s) {
		return errFundingAncestry
	}
	if !e.restoredSwap(s.ID) {
		return nil
	}
	if !refund {
		if !s.SecretObserved {
			return errors.New("restored claim cannot make the first revelation; recover a public contract witness or a newer state backup")
		}
		return nil
	}
	if s.IncomingClaimSeen || !e.recoveryRefunds[s.ID] || !e.fresh(chain.BTC) || !e.fresh(chain.Blake) {
		return errors.New("restored refund requires a current confirmed refund of the incoming contract and no previously observed incoming claim")
	}
	return nil
}

func (e *Engine) acceptRecoveryRefund(s *Swap, all map[chain.ID]map[string]chain.Observation) bool {
	if e.recoveryRefunds == nil {
		e.recoveryRefunds = map[string]bool{}
	}
	delete(e.recoveryRefunds, s.ID)
	if s.IncomingClaimSeen || !e.fresh(chain.BTC) || !e.fresh(chain.Blake) || all[chain.BTC] == nil || all[chain.Blake] == nil {
		return false
	}
	incoming := s.Short
	if s.Role == "maker" {
		incoming = s.Long
	}
	obs, ok := observation(all, incoming)
	if !ok || obs.Tx == nil || obs.Confirmations < e.Config.Network.Confirmations() || e.validateContractObservation(incoming, obs) != nil {
		return false
	}
	if _, claimed := contract.ExtractSecret(incoming, obs.Tx); claimed {
		return false
	}
	e.recoveryRefunds[s.ID] = true
	return true
}

// Imported obligations never enter the ordinary funding/revelation state machine.
// Positive observed spends can settle them; a public preimage can authorize a
// target-only recovery claim. Absence cannot establish that an old snapshot never
// learned or published something later.
func (e *Engine) advanceRestoredSwap(ctx context.Context, s *Swap, all map[chain.ID]map[string]chain.Observation) error {
	if e.fatal != nil {
		return e.fatal
	}
	if err := e.rememberSwapWitnesses(s, all); err != nil {
		return err
	}
	if e.recoverySwapInactive(s) {
		return nil
	}
	if s.Terms == nil {
		return errors.New("snapshot precedes accepted terms; waiting for authenticated peer evidence or a newer backup")
	}
	if err := s.Terms.Validate(); err != nil {
		return err
	}
	// Retain any already-proven settlement contradiction before ancestry IO.
	if err := e.reconcileFillContradiction(s, all); err != nil {
		if _, proofOnly := err.(observationEvidenceError); !proofOnly {
			return err
		}
	}
	if err := e.refreshFundingAncestry(ctx, s); err != nil {
		return e.holdFundingAncestry(ctx, s, all, err)
	}
	if err := e.reconcileObservedSwap(ctx, s, all); err != nil {
		return e.holdObservedSpend(ctx, s, all, err)
	}
	terminalStable := e.observeSwapSpends(s, all)
	if e.recoverySwapResolved(s, all) {
		if e.recoverySwapOwnInactive(s) {
			return nil
		} // Preserve the irreversible nonfunding decision.
		long, _ := observation(all, s.Long)
		short, _ := observation(all, s.Short)
		lc, sc := false, false
		if long.Tx != nil {
			_, lc = contract.ExtractSecret(s.Long, long.Tx)
		}
		if short.Tx != nil {
			_, sc = contract.ExtractSecret(s.Short, short.Tx)
		}
		if lc && sc {
			if err := e.settleRestoredMakerFill(s, FillFilled, all); err != nil {
				return err
			}
			s.Stage = "completed"
		} else if !lc && !sc {
			if err := e.settleRestoredMakerFill(s, FillReleased, all); err != nil {
				return err
			}
			s.Stage = "refunded"
		} else {
			s.Stage = "contested outcome"
		}
		// Recovery may also resolve an unfunded peer leg from one refund. Only
		// two exact confirmed spends establish a reusable exposure proof.
		if long.Tx != nil && short.Tx != nil && long.Confirmations >= e.Config.Network.Confirmations() && short.Confirmations >= e.Config.Network.Confirmations() && (s.Stage == "completed" || s.Stage == "refunded") {
			e.recordStrategyExposure(s)
		}
		return nil
	}
	if e.recoverySwapOwnInactive(s) {
		return errors.New("own funding is durably canceled; waiting for positive refund of the known peer contract")
	}
	if terminalStable {
		// Connection uncertainty changes recovery readiness, not a previously
		// confirmed outcome. This also preserves refunded history without a secret.
		return nil
	}
	if terminalSwapStage(s.Stage) {
		// Keep the contradiction after updating the display observations, so the
		// isolated claimant cannot mistake newly cleared fields for stable history.
		s.Stage = "recovery awaiting positive settlement evidence"
	}
	own := s.Long
	if s.Role == "maker" {
		own = s.Short
	}
	if e.acceptRecoveryRefund(s, all) {
		obs, spent := observation(all, own)
		if own.TxID != "" && refundReplaceable(own, spent, obs) && e.eligible(own.Chain, own.RefundHeight) && len(s.SelfRefunds) > 0 {
			if err := e.recoveryRefundTarget(ctx, own, spent); err != nil {
				return err
			}
			// Preserve the imported signed authorization and fee caps; never replace a
			// missing recovery bundle with a newly negotiated funding obligation.
			s.Stage = "refunding after confirmed incoming refund"
			if err := e.save(); err != nil {
				return err
			}
			return e.broadcastOwner(ctx, s, own.Chain, true)
		}
	}
	if s.SecretObserved {
		return e.advanceIsolatedSwap(ctx, s, all)
	}
	s.Stage = "recovery awaiting positive settlement evidence"
	return errors.New("funding and first revelation are held; retain the original installation or obtain a newer backup/peer evidence; a restored refund needs the incoming contract's confirmed refund")
}

func (e *Engine) recoverySwapResolved(s *Swap, all map[chain.ID]map[string]chain.Observation) bool {
	if e.recoverySwapInactive(s) {
		return true
	}
	if s == nil || !e.fundingAncestryReady(s) || s.Terms == nil || !e.fresh(chain.BTC) || !e.fresh(chain.Blake) || all[chain.BTC] == nil || all[chain.Blake] == nil {
		return false
	}
	own, incoming := s.Long, s.Short
	incomingRaw, incomingSent := s.ShortFunding, s.ShortSent
	if s.Role == "maker" {
		own, incoming = s.Short, s.Long
		incomingRaw, incomingSent = s.LongFunding, s.LongSent
	}
	refunded := func(c contract.HTLC) bool {
		obs, ok := observation(all, c)
		if c.TxID == "" || !ok || obs.Tx == nil || obs.Tx.TxHash().String() != obs.TxID || obs.Confirmations < e.Config.Network.Confirmations() {
			return false
		}
		_, claimed := contract.ExtractSecret(c, obs.Tx)
		return !claimed && e.validateContractObservation(c, obs) == nil
	}
	if e.recoverySwapOwnInactive(s) {
		return refunded(incoming)
	}
	if incoming.TxID == "" && incomingRaw == "" && !incomingSent {
		return refunded(own)
	}
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		obs, ok := observation(all, c)
		if c.TxID == "" || !ok || obs.Confirmations < e.Config.Network.Confirmations() || e.validateContractObservation(c, obs) != nil {
			return false
		}
	}
	return true
}

// Recompute readiness every cycle, including after a reorg. The persisted status
// is explanatory only and is reset before an engine opens. Every original ID is
// retained, so a missing record is uncertainty rather than an empty success set.
func (e *Engine) reconcileRecovery(all, towers map[chain.ID]map[string]chain.Observation) {
	r := e.s.Recovery
	if r == nil {
		return
	}
	offers, messages := recoveryQuarantineCounts(e.s)
	status := RecoveryStatus{State: "recovering", ImportedAt: r.ImportedAt, SnapshotAt: r.SnapshotAt, Legacy: r.Legacy, CheckedAt: time.Now().Unix(), QuarantinedOffers: offers, QuarantinedMessages: messages, Coverage: recoveryCoverage}
	if !e.fresh(chain.BTC) || !e.fresh(chain.Blake) || all[chain.BTC] == nil || all[chain.Blake] == nil {
		status.Issues = append(status.Issues, RecoveryIssue{Kind: "chains", Reason: "Complete current wallet and contract observations from both chains are required."})
	}
	for id := range r.Swaps {
		if e.recoverySwapResolved(e.s.Swaps[id], all) {
			delete(r.InvalidatedSettlements, "swap/"+id)
		} else {
			status.Issues = append(status.Issues, RecoveryIssue{Kind: "swap", ID: id, Reason: "Known contract outcomes are not both positively confirmed; funding and first revelation remain held."})
		}
	}
	for id := range r.Sends {
		send := e.s.Sends[id]
		if send != nil && e.fresh(send.Chain) && e.recoverySends[id] {
			delete(r.InvalidatedSettlements, "send/"+id)
		} else {
			status.Issues = append(status.Issues, RecoveryIssue{Kind: "send", ID: id, Reason: "Waiting for a recorded signed payment variant to confirm; exact signed retries remain available."})
		}
	}
	for id := range r.TowerJobs {
		state := e.s.TowerJobs[id]
		resolved := false
		if state != nil && e.fresh(state.Job.Target.Chain) && towers[state.Job.Target.Chain] != nil {
			obs, ok := observation(towers, state.Job.Target)
			resolved = ok && obs.Tx != nil && obs.Confirmations >= e.Config.Network.Confirmations()
		}
		if resolved {
			delete(r.InvalidatedSettlements, "tower/"+id)
		} else {
			status.Issues = append(status.Issues, RecoveryIssue{Kind: "tower", ID: id, Reason: "Waiting for a confirmed target spend; witnessed claims may proceed, while a stale standalone refund lacks peer safety evidence."})
		}
	}
	sort.Slice(status.Issues, func(i, j int) bool {
		a, b := status.Issues[i], status.Issues[j]
		if a.Kind == b.Kind {
			return a.ID < b.ID
		}
		return a.Kind < b.Kind
	})
	if len(status.Issues) == 0 {
		status.State = "ready"
	}
	e.recoveryReconciled = map[chain.ID]recoveryCheckpoint{}
	for id, checkpoint := range e.recoveryCheckpoints {
		e.recoveryReconciled[id] = checkpoint
	}
	r.Status = status
}

func (e *Engine) recoveryRefundTarget(ctx context.Context, own contract.HTLC, spent bool) error {
	if spent {
		return nil
	} // The complete scan already restricts this to a replaceable mempool refund.
	out, err := e.nodes[own.Chain].Output(ctx, own.TxID, own.Vout)
	if err != nil {
		return err
	}
	script, err := own.PkScript()
	if err != nil {
		return err
	}
	if out == nil || out.Confirmations < e.Config.Network.Confirmations() || int64(out.Value) != own.Amount || out.Script.Hex != hex.EncodeToString(script) {
		return errors.New("restored refund target is not the confirmed agreed contract")
	}
	return nil
}
