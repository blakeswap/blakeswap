package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

type publicationBackend struct {
	chain.Backend
	height      uint32
	afterOutput func()
}

func (b *publicationBackend) Height(context.Context) (uint32, error) { return b.height, nil }
func (b *publicationBackend) Output(ctx context.Context, id string, n uint32) (*chain.TxOut, error) {
	out, err := b.Backend.Output(ctx, id, n)
	if b.afterOutput != nil {
		b.afterOutput()
	}
	return out, err
}

func TestPublicationOwnerFeeSelectionRechecksRequiredPeer(t *testing.T) {
	for _, kind := range []string{"refund", "private_claim", "witnessed_claim"} {
		t.Run(kind, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, "taker")
			target, peer := s.Short.Chain, s.Long.Chain
			refund := kind == "refund"
			if refund {
				target, peer = s.Long.Chain, s.Short.Chain
			}
			s.OwnerFeeCap = 20000
			key, _ := e.swapKey(s.Short.Chain, s.ID)
			claim, err := contract.Spend(s.Short, key, e.scripts[s.Short.Chain], 2000, false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			s.SelfClaim = contract.Hex(claim)
			s.SecretObserved = kind == "witnessed_claim"
			e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
			e.nodes[target] = &switchingEstimateBackend{Backend: b, id: target, onEstimate: func() { e.chainFresh[peer] = false }}
			broadcasts := 0
			b.broadcast = func(raw string) (string, error) {
				broadcasts++
				tx, _ := contract.Parse(raw)
				return tx.TxHash().String(), nil
			}
			err = e.broadcastOwner(context.Background(), s, target, refund)
			if kind == "witnessed_claim" {
				if err != nil || broadcasts != 1 {
					t.Fatal("witnessed claim was blocked", err, broadcasts)
				}
			} else if err == nil || broadcasts != 0 || s.RefundLastAttempt != 0 || s.ClaimLastAttempt != 0 {
				t.Fatal("publication escaped changed peer", err, broadcasts)
			}
		})
	}
}

func TestPublicationManualRefundRechecksPeerAfterSecondScan(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			sell := chain.BTC
			if role == "maker" {
				sell = chain.Blake
			}
			e, s, b, _ := isolatedFixtureSell(t, role, sell)
			own := s.Long
			if role == "maker" {
				own = s.Short
			}
			if own.Chain != chain.Blake {
				t.Fatal("fixture order")
			}
			e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
			e.nodes[chain.Blake] = &recoveryClockBackend{Backend: b}
			changed := false
			e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &callbackRecoveryScanner{observations: map[string]chain.Observation{}, beforeReturn: func() { e.chainFresh[chain.BTC] = false; changed = true }}}
			broadcasts := 0
			b.broadcast = func(string) (string, error) { broadcasts++; return "", errors.New("unexpected publication") }
			base, _ := contract.Parse(s.SelfRefunds[0])
			raw, _ := json.Marshal(BumpRequest{ID: s.ID, Kind: "refund", Fee: 6000, ExpectedTxID: base.TxHash().String()})
			_, err := e.bumpTransaction(context.Background(), raw)
			if !changed || err == nil || broadcasts != 0 || s.RefundLastAttempt != 0 {
				t.Fatal("manual refund escaped peer source change", err, broadcasts)
			}
		})
	}
}

func TestPublicationManualPrivateClaimRechecksAfterVariantLookup(t *testing.T) {
	e, s, b, secret := isolatedFixtureSell(t, "taker", chain.Blake)
	s.OwnerFeeCap = 20000
	target, peer := s.Short, s.Long
	key, _ := e.swapKey(target.Chain, s.ID)
	claim, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	s.SelfClaim = contract.Hex(claim)
	pk, _ := peer.PkScript()
	b.coins = append(b.coins, chain.UTXO{TxID: peer.TxID, Vout: peer.Vout, Amount: chain.Coins(peer.Amount), Script: hex.EncodeToString(pk), Confirmations: 2})
	e.nodes = map[chain.ID]chain.Backend{chain.BTC: &publicationBackend{Backend: b, height: 101}, chain.Blake: &publicationBackend{Backend: b, height: 101}}
	e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
	changed := false
	b.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		if id == s.Long.TxID {
			return chain.Transaction{TxID: id, Hex: s.LongFunding, Confirmations: 2}, nil
		}
		if id == s.Short.TxID {
			return chain.Transaction{TxID: id, Hex: s.ShortFunding, Confirmations: 2}, nil
		}
		e.chainFresh[peer.Chain] = false
		changed = true
		return chain.Transaction{}, &chain.RPCError{Code: -5, Message: "not found"}
	}
	broadcasts := 0
	b.broadcast = func(string) (string, error) { broadcasts++; return "", errors.New("unexpected publication") }
	raw, _ := json.Marshal(BumpRequest{ID: s.ID, Kind: "claim", Fee: 6000, ExpectedTxID: claim.TxHash().String()})
	_, err = e.bumpTransaction(context.Background(), raw)
	if !changed || err == nil || broadcasts != 0 || s.ClaimLastAttempt != 0 {
		t.Fatal("private claim escaped post-validation peer change", err, broadcasts, changed)
	}
}

func TestPublicationMakerFundingRechecksAfterFinalOutput(t *testing.T) {
	e, s, b, _ := isolatedFixture(t, "maker")
	s.ShortSent = false
	e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
	e.heights = map[chain.ID]uint32{chain.BTC: 101, chain.Blake: 101}
	e.clocks = e.heights
	changed := false
	e.nodes[s.Long.Chain] = &publicationBackend{Backend: b, height: 101, afterOutput: func() { e.chainFresh[s.Long.Chain] = false; changed = true }}
	e.nodes[s.Short.Chain] = b
	broadcasts := 0
	b.broadcast = func(string) (string, error) { broadcasts++; return "", errors.New("unexpected publication") }
	if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); !changed || err == nil || broadcasts != 0 || s.ShortSent {
		t.Fatal("funding escaped final source change", err, broadcasts, changed)
	}
}
