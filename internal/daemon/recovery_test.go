package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/wire"
)

func markRestored(t *testing.T, e *Engine) {
	t.Helper()
	if err := PrepareRecovery(&e.s, 100, false); err != nil {
		t.Fatal(err)
	}
	e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
	e.recoveryCheckpoints = map[chain.ID]recoveryCheckpoint{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if e.heights[id] == 0 {
			e.heights[id] = 200
		}
		e.recoveryCheckpoints[id] = recoveryCheckpoint{Height: e.heights[id], Hash: "test-canonical-tip", Generation: e.chainGeneration[id]}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
}
func recoverySpend(t *testing.T, e *Engine, s *Swap, c contract.HTLC, refund bool, secret []byte) chain.Observation {
	t.Helper()
	key := isolatedSpendKey(t, e, s, c, refund)
	lock := uint32(0)
	if refund {
		lock = c.RefundHeight
		secret = nil
	}
	tx, err := contract.Spend(c, key, e.scripts[c.Chain], 2000, refund, lock, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	return chain.Observation{Tx: tx, TxID: tx.TxHash().String(), Confirmations: 2}
}

func TestRestoredPrivateClaimAndFundingRemainHeld(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			markRestored(t, e)
			incoming := s.Short
			if role == "maker" {
				incoming = s.Long
			}
			prepared := recoverySpend(t, e, s, incoming, false, secret)
			s.SelfClaim = contract.Hex(prepared.Tx)
			s.SecretExposed = true
			calls := 0
			b.broadcast = func(string) (string, error) { calls++; return "", nil }
			beforeLong, beforeShort := s.LongFunding, s.ShortFunding
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err == nil {
				t.Fatal("private restored claim was considered ready")
			}
			if calls != 0 || s.SecretObserved || s.LongFunding != beforeLong || s.ShortFunding != beforeShort {
				t.Fatal("restore funded or revealed from private snapshot")
			}
			if err := e.broadcastOwner(context.Background(), s, incoming.Chain, false); err == nil || calls != 0 {
				t.Fatal("manual/automatic publication bypassed restored first-reveal hold", err)
			}
		})
	}
}

func TestRestoredPublicWitnessSurvivesRestartAndPeerOutage(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			markRestored(t, e)
			own, incoming := s.Long, s.Short
			if role == "maker" {
				own, incoming = s.Short, s.Long
			}
			witness := recoverySpend(t, e, s, own, false, secret)
			if err := e.rememberSwapWitnesses(s, map[chain.ID]map[string]chain.Observation{own.Chain: {chain.OutpointKey(own.TxID, own.Vout): witness}}); err != nil {
				t.Fatal(err)
			}
			var restored State
			if _, err := e.vault.Load(&restored); err != nil {
				t.Fatal(err)
			}
			e.s = restored
			s = e.s.Swaps[s.ID]
			e.chainFresh[own.Chain] = false
			calls := 0
			b.broadcast = func(raw string) (string, error) {
				calls++
				tx, err := contract.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := contract.ExtractSecret(incoming, tx); !ok {
					t.Fatal("restored claim changed witness")
				}
				return tx.TxHash().String(), nil
			}
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {}}); err != nil || calls != 1 {
				t.Fatal("restored public claim failed", err, calls)
			}
		})
	}
}

func TestRestoredRefundRequiresPositiveIncomingRefund(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			markRestored(t, e)
			own, incoming := s.Long, s.Short
			if role == "maker" {
				own, incoming = s.Short, s.Long
			}
			script, _ := own.PkScript()
			b.coins = []chain.UTXO{{TxID: own.TxID, Vout: own.Vout, Amount: chain.Coins(own.Amount), Script: hex.EncodeToString(script), Confirmations: 2}}
			e.nodes[own.Chain] = b
			calls := 0
			b.broadcast = func(raw string) (string, error) {
				calls++
				tx, err := contract.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, claim := contract.ExtractSecret(own, tx); claim {
					t.Fatal("refund became claim")
				}
				return tx.TxHash().String(), nil
			}
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			_ = e.advanceSwap(context.Background(), s, all)
			if calls != 0 {
				t.Fatal("missing peer observation authorized refund")
			}
			all[incoming.Chain][chain.OutpointKey(incoming.TxID, incoming.Vout)] = recoverySpend(t, e, s, incoming, true, nil)
			if err := e.advanceSwap(context.Background(), s, all); err != nil || calls != 1 {
				t.Fatal("positive refund recovery failed", err, calls)
			}
			claim := recoverySpend(t, e, s, incoming, false, secret)
			if err := e.rememberSwapWitnesses(s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {chain.OutpointKey(incoming.TxID, incoming.Vout): claim}}); err != nil {
				t.Fatal(err)
			}
			var restored State
			if _, err := e.vault.Load(&restored); err != nil {
				t.Fatal(err)
			}
			e.s = restored
			s = e.s.Swaps[s.ID]
			s.RefundLastAttempt = 0
			if err := e.recoveryOwnerPolicy(s, true); err == nil {
				t.Fatal("restart or later refund erased incoming claim hold")
			}
		})
	}
}

func TestRestoredReadinessRequiresPositiveKnownResolution(t *testing.T) {
	e, s, _, secret := isolatedFixture(t, "maker")
	markRestored(t, e)
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("absence was treated as complete recovery")
	}
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, false, secret)
	}
	if err := e.advanceSwap(context.Background(), s, all); err != nil {
		t.Fatal(err)
	}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() != nil || s.Stage != "completed" {
		t.Fatal("positive completed recovery did not become ready", e.s.Recovery.Status)
	}
	delete(all[s.Long.Chain], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil || !s.SecretObserved {
		t.Fatal("reorg did not reopen hold or erased public secret")
	}
	empty, _, _ := sendFixture(t)
	empty.s.Swaps = map[string]*Swap{}
	markRestored(t, empty)
	empty.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil)
	if err := empty.recoveryTradingReady(); err != nil {
		t.Fatal("fully synchronized wallet with no recorded obligations was permanently disabled", err)
	}
}

type checkpointSendBackend struct {
	*sendBackend
	hash       string
	generation uint64
	onHash     func() string
}

func (b *checkpointSendBackend) BlockHash(context.Context, uint32) (string, error) {
	if b.onHash != nil {
		return b.onHash(), nil
	}
	return b.hash, nil
}

func TestRestoredSendReadinessRejectsSkippedCachedConfirmations(t *testing.T) {
	e, backend, request := sendFixture(t)
	backend.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
	raw, _ := json.Marshal(request)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	send := e.s.Sends[request.ID]
	send.Confirmations = 42 // Stale imported display data is never readiness evidence.
	markRestored(t, e)
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.advanceSends(ctx) // Simulate a rotating slice that never reaches this record.
	e.reconcileRecovery(all, nil)
	if e.s.Recovery.Status.State == "ready" || e.recoveryTradingReady() == nil {
		t.Fatal("skipped stale-confirmed send made restore ready")
	}
	b := &checkpointSendBackend{sendBackend: backend, hash: "canonical-a"}
	e.nodes[send.Chain] = b
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	backend.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
	}
	e.advanceSends(context.Background())
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal("fresh recorded variant failed readiness", err)
	}
	// A bounded skipped slice can retain positive proof only on the same verified
	// history, allowing more records than fit in a single scan to make progress.
	e.advanceSends(ctx)
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal("unchanged checkpoint lost bounded progress", err)
	}
	e.heights[send.Chain]++
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	if e.recoveryTradingReady() == nil {
		t.Fatal("new checkpoint reused prior reconciliation")
	}
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal("verified descendant lost confirmed payment", err)
	}
	// Reorg during a skipped slice must remove the old proof even while the
	// persisted display count remains confirmed.
	b.hash = "reorg-b"
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	e.advanceSends(ctx)
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("reorg reused cached positive send proof")
	}
	e.advanceSends(context.Background())
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
	// A same-height reorg between the ancestry read and tip read must also
	// invalidate proofs before the new checkpoint is accepted.
	count := 0
	b.onHash = func() string {
		count++
		if count == 1 {
			return "reorg-b"
		}
		return "reorg-c"
	}
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err == nil {
		t.Fatal("mixed checkpoint reads were accepted")
	}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("reorg between checkpoint reads preserved payment proof")
	}
	b.onHash = nil
	b.hash = "reorg-c"
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	e.advanceSends(context.Background())
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
	// A source generation change also starts a new evidence context.
	e.chainGeneration[send.Chain]++
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("source switch reused payment proof")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.recoverySends = nil
	e.recoveryCheckpoints = nil
	e.recoveryReconciled = nil
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("reload trusted persisted confirmations")
	}
}

func TestRestoredSendSlicesAccumulateOnlyCurrentPositiveVariants(t *testing.T) {
	e, b, request := sendFixture(t)
	b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
	raw, _ := json.Marshal(request)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	send := e.s.Sends[request.ID]
	second := *send
	send.ID = "a"
	second.ID = "b"
	e.s.Sends = map[string]*WalletSend{"a": send, "b": &second}
	markRestored(t, e)
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	// The second lookup consumes each slice. The cursor moves so the other record
	// is positively observed in the next slice without extending the total budget.
	for cycle := 0; cycle < 2; cycle++ {
		calls := 0
		b.transaction = func(ctx context.Context, id string) (chain.Transaction, error) {
			calls++
			if calls == 2 {
				<-ctx.Done()
				return chain.Transaction{}, ctx.Err()
			}
			return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		e.advanceSends(ctx)
		cancel()
		e.reconcileRecovery(all, nil)
		if cycle == 0 && e.recoveryTradingReady() == nil {
			t.Fatal("incomplete first slice became ready")
		}
	}
	// Both records become ready in two bounded slices; previously verified A
	// does not repeatedly consume the budget needed to check B.
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal("bounded slices made no progress", err)
	}
	// A successful ordinary refresh keeps that ready path usable.
	b.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
	}
	e.advanceSends(context.Background())
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
}

// Build current requests with retained identities before recording an unfunded
// decision. Maker refusal goes through real acceptance and retirement so its
// parent/child quantities and exact funding authorization agree.
func recoveryUnfundedFixture(t *testing.T, role, stage string, expires int64) (*Engine, *Swap) {
	t.Helper()
	if role == "maker" {
		e, maker, now := fillAdmissionEngine(t, chain.BTC)
		r := admissionRequest(t, e, maker, 400000)
		var err error
		r.Keys, err = e.swapKeys(isolatedPeerID(r.ID))
		if err != nil {
			t.Fatal(err)
		}
		if err := applyFillRequest(t, e, r, now); err != nil {
			t.Fatal(err)
		}
		s := e.s.Swaps[r.ID]
		e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
		if err := e.retireUnfundedMaker(s, e.gate(s.Terms, "fund-short")); err != nil {
			t.Fatal(err)
		}
		if s.Stage != stage || !e.s.FillRecords[s.ID].FundingDisabled {
			t.Fatal("fixture lacks durable never-funded refusal")
		}
		return e, s
	}
	e, _, _ := sendFixture(t)
	parent, maker := fillParentFixture(t, chain.BTC, 0)
	if expires != 0 {
		parent.Offer.Expires = expires
	}
	r := fillRequestFixture(t, parent, maker, 400000)
	r.Taker = e.identity.Public().Hex()
	var err error
	r.Keys, err = e.swapKeys(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := &Swap{ID: r.ID, Role: role, Stage: stage, Request: r}
	if stage == "expired before funding" {
		keys, err := e.swapKeys(isolatedPeerID(r.ID))
		if err != nil {
			t.Fatal(err)
		}
		terms, err := protocol.NewTerms(r, keys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100})
		if err != nil {
			t.Fatal(err)
		}
		s.Terms, s.Long, s.Short = &terms, terms.Long, terms.Short
	}
	e.s.Swaps[s.ID] = s
	if err := e.retainSwapIdentity(s); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, s
}

func TestRestoredFinalUnfundedDecisionsRemainInactive(t *testing.T) {
	for _, stage := range []string{"rejected", "expired before acceptance", "expired before funding", "expired before maker funding"} {
		t.Run(stage, func(t *testing.T) {
			role := "taker"
			if stage == "expired before maker funding" {
				role = "maker"
			}
			e, s := recoveryUnfundedFixture(t, role, stage, 0)
			markRestored(t, e)
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			if err := e.advanceSwap(context.Background(), s, all); err != nil {
				t.Fatal(err)
			}
			e.reconcileRecovery(all, nil)
			if err := e.recoveryTradingReady(); err != nil || s.Stage != stage {
				t.Fatal("final empty obligation held", err, s.Stage)
			}
			s.LongFunding = "prepared-private-funding"
			e.reconcileRecovery(all, nil)
			if e.recoveryTradingReady() == nil {
				t.Fatal("stage label bypassed prepared funding hold")
			}
			s.LongFunding = ""
			s.Stage = "request queued"
			if role == "maker" {
				// A display label cannot erase an irreversible child refusal.
				e.reconcileRecovery(all, nil)
				if e.recoveryTradingReady() != nil {
					t.Fatal("presentation change erased durable refusal")
				}
				// Missing durable authority is unknown, even with empty scans.
				delete(e.s.FillRecords, s.ID)
			}
			e.reconcileRecovery(all, nil)
			if e.recoveryTradingReady() == nil {
				t.Fatal("active unknown request treated as terminal")
			}
		})
	}
}

func TestRestoredPendingRequestCannotAcquireTerminalExpiryEvidence(t *testing.T) {
	e, s := recoveryUnfundedFixture(t, "taker", "request queued", time.Now().Add(-time.Hour).Unix())
	markRestored(t, e)
	if e.expirePendingRequest(s, time.Now().Unix()) {
		t.Fatal("elapsed time invented post-restore expiry evidence")
	}
	e.reconcileReservations()
	if s.Stage != "request queued" {
		t.Fatal("wallet refresh fabricated a terminal decision", s.Stage)
	}
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	if err := e.advanceSwap(context.Background(), s, all); err == nil {
		t.Fatal("pre-acceptance snapshot was considered resolved")
	}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil || e.s.Recovery.Status.State == "ready" {
		t.Fatal("old unknown obligation became ready after expiration")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vault.Load(&e.s); err != nil {
		t.Fatal(err)
	}
	if e.expirePendingRequest(e.s.Swaps[s.ID], time.Now().Unix()) {
		t.Fatal("restart bypassed restored-expiry hold")
	}
}

func TestRestoredPositiveSingleLegRefund(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, _, _ := isolatedFixture(t, role)
			own := s.Long
			if role == "maker" {
				own = s.Short
				s.Long.TxID = ""
				s.LongFunding = ""
				s.LongSent = false
			} else {
				s.Short.TxID = ""
				s.ShortFunding = ""
				s.ShortSent = false
			}
			s.Stage = "refunded"
			markRestored(t, e)
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			all[own.Chain][chain.OutpointKey(own.TxID, own.Vout)] = recoverySpend(t, e, s, own, true, nil)
			if err := e.advanceSwap(context.Background(), s, all); err != nil {
				t.Log(err)
			}
			e.reconcileRecovery(all, nil)
			if e.recoveryTradingReady() != nil {
				t.Fatal("positive refund of sole known funded leg did not resolve recorded obligation", e.s.Recovery.Status)
			}
			delete(all[own.Chain], chain.OutpointKey(own.TxID, own.Vout))
			e.reconcileRecovery(all, nil)
			if e.recoveryTradingReady() == nil {
				t.Fatal("reorged sole refund retained readiness")
			}
		})
	}
}
func TestRestoredExpiredMakerWithPeerRefund(t *testing.T) {
	e, s := recoveryUnfundedFixture(t, "maker", "expired before maker funding", 0)
	// Retain the peer's known contract without inventing any own funding.
	pk, err := s.Long.PkScript()
	if err != nil {
		t.Fatal(err)
	}
	funding := wire.NewMsgTx(2)
	funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	funding.AddTxOut(wire.NewTxOut(s.Long.Amount, pk))
	s.Long.TxID, s.LongFunding = funding.TxHash().String(), contract.Hex(funding)
	markRestored(t, e)
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	all[s.Long.Chain][chain.OutpointKey(s.Long.TxID, s.Long.Vout)] = recoverySpend(t, e, s, s.Long, true, nil)
	if err := e.advanceSwap(context.Background(), s, all); err != nil {
		t.Log(err)
	}
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() != nil {
		t.Fatal("final own nonfunding plus confirmed peer refund did not reconcile", e.s.Recovery.Status)
	}
	if !e.recoverySwapOwnInactive(s) || !e.s.FillRecords[s.ID].FundingDisabled || e.s.FillRecords[s.ID].Allocation.currentQuantity() != 0 {
		t.Fatal("lost durable nonfunding decision")
	}
	delete(all[s.Long.Chain], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("peer refund reorg retained readiness")
	}
	s.Stage = "awaiting chain confirmations"
	all[s.Long.Chain][chain.OutpointKey(s.Long.TxID, s.Long.Vout)] = recoverySpend(t, e, s, s.Long, true, nil)
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() != nil {
		t.Fatal("display stage erased durable nonfunding decision")
	}
	delete(e.s.FillRecords, s.ID)
	e.reconcileRecovery(all, nil)
	if e.recoveryTradingReady() == nil {
		t.Fatal("unknown own publication resolved from peer refund")
	}
}

type recoveryExtensionRaceBackend struct {
	*sendBackend
	oldHeight      uint32
	trigger, reorg bool
}

func (b *recoveryExtensionRaceBackend) BlockHash(_ context.Context, height uint32) (string, error) {
	if b.trigger && height > b.oldHeight {
		b.reorg = true
	}
	if b.reorg {
		return "competing-history", nil
	}
	return "prior-history", nil
}
func TestRestoredExtensionReorgCannotCarrySkippedPaymentProof(t *testing.T) {
	e, rawBackend, request := sendFixture(t)
	rawBackend.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
	raw, _ := json.Marshal(request)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	send := e.s.Sends[request.ID]
	markRestored(t, e)
	b := &recoveryExtensionRaceBackend{sendBackend: rawBackend, oldHeight: e.heights[send.Chain]}
	e.nodes[send.Chain] = b
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	rawBackend.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
	}
	e.advanceSends(context.Background())
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
	b.trigger = true
	e.heights[send.Chain]++
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	// Bounded send work skips this record. A current tip from a different fork
	// must not inherit its old proof merely because the ancestry read ran first.
	e.reconcileRecovery(all, nil)
	if e.recoverySends[send.ID] || e.recoveryTradingReady() == nil {
		t.Fatal("extension tip adopted old payment proof across a reorg")
	}
}

func TestRestoredInverseReorgRejectsMixedTipAndAncestry(t *testing.T) {
	e, backend, request := sendFixture(t)
	backend.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
	raw, _ := json.Marshal(request)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	send := e.s.Sends[request.ID]
	markRestored(t, e)
	b := &checkpointSendBackend{sendBackend: backend, hash: "original-history"}
	e.nodes[send.Chain] = b
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	backend.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		return chain.Transaction{TxID: id, Hex: send.Raw, Height: 190, Confirmations: 11}, nil
	}
	e.advanceSends(context.Background())
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal(err)
	}
	e.heights[send.Chain]++
	calls := 0
	b.onHash = func() string {
		calls++
		if calls == 1 {
			return "temporary-competing-tip"
		}
		return "original-history"
	}
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err == nil {
		t.Fatal("temporary fork tip was assigned to old-history proofs")
	}
	e.reconcileRecovery(all, nil)
	if e.recoverySends[send.ID] || e.recoveryTradingReady() == nil || e.recoveryCheckpoints[send.Chain].Hash != "" {
		t.Fatal("mixed checkpoint retained recovery authority")
	}
	b.onHash = nil
	if err := e.refreshRecoveryCheckpoint(context.Background(), send.Chain); err != nil {
		t.Fatal(err)
	}
	e.advanceSends(context.Background())
	e.reconcileRecovery(all, nil)
	if err := e.recoveryTradingReady(); err != nil {
		t.Fatal("stable current chain failed to recover", err)
	}
}
