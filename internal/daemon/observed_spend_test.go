package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type observedPrevoutBackend struct {
	chain.Backend
	previous chain.Transaction
	err      error
	before   func(context.Context)
	calls    int
}

func (b *observedPrevoutBackend) Transaction(ctx context.Context, id string) (chain.Transaction, error) {
	if id != b.previous.TxID {
		return b.Backend.Transaction(ctx, id)
	}
	b.calls++
	if b.before != nil {
		b.before(ctx)
	}
	return b.previous, b.err
}

// A second, independently hash-bound funding transaction adds a real input at
// index zero. The HTLC is re-signed at index one with its actual sequence.
func observedMultiInput(t *testing.T, e *Engine, s *Swap, c contract.HTLC, refund bool, secret []byte) (chain.Observation, chain.Transaction) {
	t.Helper()
	obs := recoverySpend(t, e, s, c, refund, secret)
	previous := wire.NewMsgTx(2)
	op, _ := contract.Outpoint(transport.RandomID(), 0)
	previous.AddTxIn(wire.NewTxIn(&op, nil, nil))
	previous.AddTxOut(wire.NewTxOut(10000, []byte{txscript.OP_TRUE}))
	extra := wire.OutPoint{Hash: previous.TxHash()}
	tx := obs.Tx.Copy()
	tx.TxIn = append([]*wire.TxIn{wire.NewTxIn(&extra, nil, nil)}, tx.TxIn...)
	tx.TxIn[1].Sequence = wire.MaxTxInSequenceNum - 1
	if refund {
		tx.LockTime = c.RefundHeight + 1
	}
	pk, _ := c.PkScript()
	script, _ := c.Script()
	spent := []*wire.TxOut{previous.TxOut[0], wire.NewTxOut(c.Amount, pk)}
	digest, err := contract.Digest(c.Chain, tx, 1, script, spent)
	if err != nil {
		t.Fatal(err)
	}
	typ := byte(txscript.SigHashAll)
	if c.Chain == chain.Blake {
		typ = byte(contract.UnifiedAll)
	}
	tx.TxIn[1].Witness[0] = append(ecdsa.Sign(isolatedSpendKey(t, e, s, c, refund), digest).Serialize(), typ)
	if err := contract.VerifyObservedSpend(c, tx, spent); err != nil {
		t.Fatal(err)
	}
	if c.Chain == chain.BTC {
		fetch := txscript.NewMultiPrevOutFetcher(map[wire.OutPoint]*wire.TxOut{extra: spent[0], tx.TxIn[1].PreviousOutPoint: spent[1]})
		hashes := txscript.NewTxSigHashes(tx, fetch)
		for i, output := range spent {
			vm, err := txscript.NewEngine(output.PkScript, tx, i, txscript.StandardVerifyFlags, nil, hashes, output.Value, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if err := vm.Execute(); err != nil {
				t.Fatal("independent script execution", err)
			}
		}
	}
	obs.Tx, obs.TxID = tx, tx.TxHash().String()
	return obs, chain.Transaction{TxID: previous.TxHash().String(), Hex: contract.Hex(previous)}
}

func TestObservedMultiInputFillSettlement(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, restored := range []bool{false, true} {
			t.Run(string(sell)+map[bool]string{false: "/local", true: "/restored"}[restored], func(t *testing.T) {
				e, children, secrets := fundedFillPair(t, sell)
				all := fillPairOutcomes(t, e, children, secrets, false)
				for i, s := range children {
					obs, previous := observedMultiInput(t, e, s, s.Short, i == 1, secrets[i])
					all[s.Short.Chain][chain.OutpointKey(s.Short.TxID, s.Short.Vout)] = obs
					e.nodes[sell] = &observedPrevoutBackend{Backend: e.nodes[sell], previous: previous}
				}
				if restored {
					markRestored(t, e)
				}
				for _, s := range children {
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
				}
				p := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
				if p.Quantities.Filled != 400000 || p.Quantities.Released != 600000 || p.Quantities.Available != 0 || p.Quantities.Committed != 0 {
					t.Fatal("peer settlement did not conserve exact child bins", p.Quantities)
				}
				if restored {
					for _, s := range children {
						if !e.recoverySwapResolved(s, all) {
							t.Fatal("pure recovery lost verified multi-input outcome")
						}
					}
				}
				before := protocol.Digest([]any{p.Quantities, p.Fees, p.Bounties, e.s.FillRecords})
				for _, s := range children {
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
				}
				if protocol.Digest([]any{p.Quantities, p.Fees, p.Bounties, e.s.FillRecords}) != before {
					t.Fatal("repeat outcome consumed authority")
				}
			})
		}
	}
}

func TestObservedUnknownPeerProofStillClaimsPublicSecret(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, problem := range []string{"unavailable", "malformed", "wrong hash", "wrong vout", "deadline", "input bound", "byte bound", "source changed"} {
			t.Run(problem+map[bool]string{false: "/local", true: "/restored"}[restored], func(t *testing.T) {
				e, s, b, secret := isolatedFixtureSell(t, "maker", chain.Blake)
				e.chainFresh[chain.Blake] = true
				obs, previous := observedMultiInput(t, e, s, s.Short, false, secret)
				backend := &observedPrevoutBackend{Backend: &fundingLookupBackend{}, previous: previous}
				e.nodes[chain.Blake] = backend
				switch problem {
				case "unavailable":
					backend.err = errors.New("injected missing previous transaction")
				case "malformed":
					backend.previous.Hex = "broken"
				case "wrong hash":
					tx, _ := contract.Parse(previous.Hex)
					tx.LockTime++
					backend.previous.Hex = contract.Hex(tx)
				case "wrong vout":
					obs.Tx.TxIn[0].PreviousOutPoint.Index = 1
					obs.TxID = obs.Tx.TxHash().String()
				case "deadline":
					backend.before = func(ctx context.Context) { <-ctx.Done() }
					backend.err = context.DeadlineExceeded
				case "input bound":
					for len(obs.Tx.TxIn) <= observedPrevoutReads {
						op, _ := contract.Outpoint(transport.RandomID(), 0)
						obs.Tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
					}
					obs.TxID = obs.Tx.TxHash().String()
				case "byte bound":
					backend.previous.Hex = strings.Repeat("00", observedPrevoutBytes+1)
				case "source changed":
					backend.before = func(context.Context) { e.chainFresh[chain.Blake] = false }
				}
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {chain.OutpointKey(s.Short.TxID, s.Short.Vout): obs}}
				if restored {
					markRestored(t, e)
				}
				before := protocol.Digest([]any{s.LongFunding, s.ShortFunding, s.SelfRefunds, e.s.ParentOrders, e.s.FillRecords})
				calls := 0
				b.broadcast = func(string) (string, error) { calls++; return "", nil }
				_ = e.advanceSwap(context.Background(), s, all)
				if !s.SecretObserved || s.SelfClaim == "" || calls == 0 {
					t.Fatal("unrelated proof uncertainty suppressed exact incoming rescue", s.Error, s.Stage, backend.calls, calls)
				}
				if protocol.Digest([]any{s.LongFunding, s.ShortFunding, s.SelfRefunds, e.s.ParentOrders, e.s.FillRecords}) != before {
					t.Fatal("proof uncertainty changed funding/refund/accounting authority")
				}
				if e.recoverySwapResolved(s, all) {
					t.Fatal("unknown proof became settlement evidence")
				}
			})
		}
	}
}

func TestObservedProofBindsWitnessAndRetriesAfterScan(t *testing.T) {
	e, s, _, secret := isolatedFixtureSell(t, "maker", chain.Blake)
	e.chainFresh[chain.Blake] = true
	obs, previous := observedMultiInput(t, e, s, s.Short, false, secret)
	b := &observedPrevoutBackend{Backend: &fundingLookupBackend{}, previous: previous, err: context.DeadlineExceeded}
	e.nodes[chain.Blake] = b
	all := map[chain.ID]map[string]chain.Observation{chain.Blake: {chain.OutpointKey(s.Short.TxID, s.Short.Vout): obs}}
	e.prepareObservedSpends(context.Background(), s, all)
	b.err = nil
	e.prepareObservedSpends(context.Background(), s, all)
	if b.calls != 1 || e.validateContractObservation(s.Short, obs) == nil {
		t.Fatal("nested call retried failed evidence")
	}
	e.resetObservedSpendWork()
	e.prepareObservedSpends(context.Background(), s, all)
	if b.calls != 2 || e.validateContractObservation(s.Short, obs) != nil {
		t.Fatal("next scan did not retry complete proof")
	}
	e.resetObservedSpendWork()
	e.prepareObservedSpends(context.Background(), s, all)
	if b.calls != 2 {
		t.Fatal("immutable successful signature fetched again")
	}
	bad := obs
	bad.Tx = obs.Tx.Copy()
	bad.Tx.TxIn[1].Witness[0] = []byte{1, 2, byte(contract.UnifiedAll)}
	if bad.Tx.TxHash() != obs.Tx.TxHash() || e.validateContractObservation(s.Short, bad) == nil {
		t.Fatal("nonwitness transaction id authorized altered witness")
	}
	e.chainGeneration[chain.Blake]++
	if e.validateContractObservation(s.Short, obs) == nil {
		t.Fatal("source generation reused observation qualification")
	}
	delete(e.s.Swaps, s.ID)
	e.resetObservedSpendWork()
	if len(e.observedSpendProofs) != 0 {
		t.Fatal("cold child retained active proof cache")
	}
}

func TestObservedProofHoldPreservesExactClaimTargetGate(t *testing.T) {
	for _, targetCase := range []string{"valid confirmed", "invalid confirmed unspent", "invalid mempool spent", "valid mempool spent", "missing scan", "stale target", "wrong amount"} {
		t.Run(targetCase, func(t *testing.T) {
			e, s, b, secret := isolatedFixtureSell(t, "maker", chain.Blake)
			e.chainFresh[chain.Blake] = true
			peer, previous := observedMultiInput(t, e, s, s.Short, false, secret)
			e.nodes[chain.Blake] = &observedPrevoutBackend{Backend: &fundingLookupBackend{}, previous: previous, err: context.DeadlineExceeded}
			target := recoverySpend(t, e, s, s.Long, false, secret)
			switch targetCase {
			case "invalid confirmed unspent", "invalid mempool spent":
				target.Tx.TxIn[0].Witness[0] = []byte{1, 2}
			case "missing scan":
			case "stale target":
				e.chainFresh[chain.BTC] = false
			case "wrong amount":
				b.coins[0].Amount++
			}
			if strings.Contains(targetCase, "mempool") {
				target.Confirmations = 0
				b.coins = nil
			}
			all := map[chain.ID]map[string]chain.Observation{chain.Blake: {chain.OutpointKey(s.Short.TxID, s.Short.Vout): peer}, chain.BTC: {chain.OutpointKey(s.Long.TxID, s.Long.Vout): target}}
			if targetCase == "missing scan" {
				all[chain.BTC] = nil
			}
			if targetCase == "wrong amount" {
				delete(all[chain.BTC], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
			}
			calls := 0
			b.broadcast = func(string) (string, error) { calls++; return "", nil }
			_ = e.advanceSwap(context.Background(), s, all)
			want := targetCase == "invalid confirmed unspent" || targetCase == "valid mempool spent"
			if (calls > 0) != want || (s.SelfClaim != "") != want {
				t.Fatal("proof hold bypassed exact target evidence", s.Stage, calls)
			}
			if !s.SecretObserved {
				t.Fatal("target gate erased observed preimage")
			}
		})
	}
}

func TestObservedProofHoldCannotRevealPrivateSecretOrRefund(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, role := range []string{"maker", "taker"} {
			t.Run(role+map[bool]string{false: "/local", true: "/restored"}[restored], func(t *testing.T) {
				e, s, b, secret := isolatedFixtureSell(t, role, chain.Blake)
				e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
				own, incoming := s.Long, s.Short
				if role == "maker" {
					own, incoming = s.Short, s.Long
				}
				obs := recoverySpend(t, e, s, own, true, nil)
				obs.Tx.TxIn[0].Witness[0] = []byte{1, 2}
				private := recoverySpend(t, e, s, incoming, false, secret)
				s.SelfClaim = contract.Hex(private.Tx)
				if restored {
					markRestored(t, e)
				}
				before := protocol.Digest([]any{s.LongFunding, s.ShortFunding, s.SelfRefunds, s.SelfClaim, e.s.ParentOrders, e.s.FillRecords})
				calls := 0
				b.broadcast = func(string) (string, error) { calls++; return "", nil }
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				all[own.Chain][chain.OutpointKey(own.TxID, own.Vout)] = obs
				if err := e.advanceSwap(context.Background(), s, all); err == nil {
					t.Fatal("invalid peer proof fell through execution")
				}
				if s.SecretObserved || calls != 0 || protocol.Digest([]any{s.LongFunding, s.ShortFunding, s.SelfRefunds, s.SelfClaim, e.s.ParentOrders, e.s.FillRecords}) != before {
					t.Fatal("unknown spend authorized private reveal, funding or refund")
				}
			})
		}
	}
}

func TestObservedDemotionSaveFailurePrecedesPrevoutLookupAndRescue(t *testing.T) {
	e, children, secrets := fundedFillPair(t, chain.Blake)
	all := fillPairOutcomes(t, e, children, secrets, false)
	s := children[0]
	if err := e.advanceSwap(context.Background(), s, all); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	obs, previous := observedMultiInput(t, e, s, s.Short, false, secrets[0])
	all[chain.Blake][chain.OutpointKey(s.Short.TxID, s.Short.Vout)] = obs
	delete(all[chain.BTC], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
	b := &observedPrevoutBackend{Backend: &fundingLookupBackend{}, previous: previous}
	e.nodes[chain.Blake] = b
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.advanceSwap(context.Background(), s, all); err == nil || e.fatal == nil {
		t.Fatal("failed demotion checkpoint did not stop execution", err)
	}
	if b.calls != 0 || s.SelfClaim != "" {
		t.Fatal("proof lookup or rescue ran before durable contradiction", b.calls)
	}
}

func TestObservedProofWorkDoesNotEvictEarlierChildren(t *testing.T) {
	e, s, _, secret := isolatedFixtureSell(t, "maker", chain.Blake)
	e.chainFresh[chain.Blake] = true
	all := map[chain.ID]map[string]chain.Observation{chain.Blake: {}}
	var children []*Swap
	var backends []*observedPrevoutBackend
	for i := 0; i < 18; i++ {
		child := *s
		child.ID = transport.RandomID()
		child.Short.TxID = transport.RandomID()
		// This control exercises the immutable proof cache, not admission. The
		// fixture's original private key signs every separate synthetic outpoint.
		obs, previous := observedMultiInput(t, e, s, child.Short, false, secret)
		all[chain.Blake][chain.OutpointKey(child.Short.TxID, child.Short.Vout)] = obs
		e.s.Swaps[child.ID] = &child
		b := &observedPrevoutBackend{Backend: e.nodes[chain.Blake], previous: previous}
		e.nodes[chain.Blake] = b
		backends = append(backends, b)
		children = append(children, &child)
	}
	e.observedSpendReads = observedPrevoutReads - 1
	for _, child := range children {
		e.prepareObservedSpends(context.Background(), child, all)
	}
	if backends[0].calls != 1 || backends[1].calls != 0 {
		t.Fatal("aggregate read budget was exceeded")
	}
	e.resetObservedSpendWork()
	for _, child := range children {
		e.prepareObservedSpends(context.Background(), child, all)
	}
	for i, child := range children {
		obs, _ := observation(all, child.Short)
		if e.validateContractObservation(child.Short, obs) != nil || backends[i].calls != 1 {
			t.Fatal("later child or cache eviction lost earlier proof", i, backends[i].calls)
		}
	}
}

func TestObservedMissingScanCannotEnterFundingOrRetirement(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[r.ID]
	f := e.s.FillRecords[s.ID]
	before := protocol.Digest([]any{f, e.s.ParentOrders, s.ShortFunding, s.SelfRefunds})
	e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
	if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}}); err == nil {
		t.Fatal("missing peer scan was treated as empty")
	}
	if protocol.Digest([]any{f, e.s.ParentOrders, s.ShortFunding, s.SelfRefunds}) != before {
		t.Fatal("unknown scan entered funding or quantity retirement")
	}
	if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil || !f.FundingDisabled {
		t.Fatal("complete positive local expiry control failed", err)
	}
}
