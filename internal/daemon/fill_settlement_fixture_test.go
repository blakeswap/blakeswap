package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/wire"
)

// Bind a confirmed settlement to this child's retained signed authorization.
// A later receive-address rotation cannot change a previously signed payout.
func partialRetainedPayout(e *Engine, s *Swap, c contract.HTLC, actual *wire.MsgTx, refund bool, towerBPS int64) ([]byte, error) {
	if s == nil || s.Terms == nil || s.ID != s.Request.ID || protocol.Digest(s.Request) != protocol.Digest(s.Terms.Request) || s.Terms.Party(s.Role) != e.identity.Public().Hex() {
		return nil, errors.New("settlement fixture has the wrong owner or child")
	}
	own, _, _ := localFunding(s)
	incoming := s.Short
	if s.Role == "maker" {
		incoming = s.Long
	}
	target := incoming
	if refund {
		target = own
	}
	if c != target || contract.VerifyObservedSpend(c, actual, nil) != nil {
		return nil, errors.New("settlement fixture has the wrong contract or signature")
	}
	var variants []string
	if towerBPS > 0 {
		if !refund {
			return nil, errors.New("fixture expects owner claims")
		}
		for _, job := range s.Jobs {
			if job.SwapID == s.ID && job.Owner == e.identity.Public().Hex() && job.TermsHash == protocol.Digest(s.Terms) && job.Kind == "refund" && job.Target == c {
				if err := job.Validate(s.protection().Scripts, towerBPS); err != nil {
					return nil, err
				}
				variants = append(variants, job.Templates...)
			}
		}
	} else if refund {
		variants = append(variants, s.SelfRefunds...)
	} else {
		variants = append(variants, s.SelfClaims...)
		if s.SelfClaim != "" {
			variants = append(variants, s.SelfClaim)
		}
	}
	for _, raw := range variants {
		saved, err := contract.Parse(raw)
		if err != nil {
			return nil, err
		}
		// Witness identity also rejects unsigned/substituted payout variants.
		if saved.TxHash() == actual.TxHash() && saved.WitnessHash() == actual.WitnessHash() {
			return append([]byte(nil), saved.TxOut[0].PkScript...), nil
		}
	}
	return nil, errors.New("actual settlement has no exact retained signed authorization")
}

func TestPartialSettlementPayoutSurvivesReceiveRotation(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, role := range []string{"maker", "taker"} {
			t.Run(string(sell)+"/"+role, func(t *testing.T) {
				e, s, _, secret := isolatedFixtureSell(t, role, sell)
				own, _, _ := localFunding(s)
				incoming := s.Short
				if role == "maker" {
					incoming = s.Long
				}
				key := isolatedSpendKey(t, e, s, incoming, false)
				claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
				if err != nil {
					t.Fatal(err)
				}
				s.SelfClaim = contract.Hex(claim)
				// The existing fixture supplies confirmed-receipt observations. Exercise
				// the production rotation/save path, without claiming actual-chain proof.
				for _, id := range []chain.ID{chain.BTC, chain.Blake} {
					backend := e.watch[id].(*receiveBackend)
					e.nodes[id] = backend
					backend.used[e.addresses[id]] = true
					if err := e.rotateReceiveAddress(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				}
				for _, item := range []struct {
					c      contract.HTLC
					raw    string
					refund bool
				}{{incoming, s.SelfClaim, false}, {own, s.SelfRefunds[1], true}} {
					actual, err := contract.Parse(item.raw)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Equal(actual.TxOut[0].PkScript, e.scripts[item.c.Chain]) {
						t.Fatal("fixture did not rotate signed payout")
					}
					payout, err := partialRetainedPayout(e, s, item.c, actual, item.refund, 0)
					if err != nil || !bytes.Equal(payout, actual.TxOut[0].PkScript) {
						t.Fatal("retained payout lost after rotation", err)
					}
					bad := *s
					bad.ID = "another-child"
					if _, err := partialRetainedPayout(e, &bad, item.c, actual, item.refund, 0); err == nil {
						t.Fatal("another child authorized payout")
					}
					changed := actual.Copy()
					changed.TxOut[0].PkScript = append([]byte(nil), e.scripts[item.c.Chain]...)
					if _, err := partialRetainedPayout(e, s, item.c, changed, item.refund, 0); err == nil {
						t.Fatal("unsigned current-address substitution accepted")
					}
					target := item.c
					target.Vout++
					if _, err := partialRetainedPayout(e, s, target, actual, item.refund, 0); err == nil {
						t.Fatal("wrong target authorized")
					}
				}
				// A fully signed tower refund retains its own payout and exact child job.
				s.Protection = &protocol.Tower{PubKey: e.identity.Public().Hex(), BPS: 50, Scripts: map[chain.ID]string{chain.BTC: hex.EncodeToString(e.scripts[chain.BTC]), chain.Blake: hex.EncodeToString(e.scripts[chain.Blake])}}
				job, err := e.makeJob(s, own, "refund", nil, own.RefundHeight+protocol.RefundDelay(e.Config.Network))
				if err != nil {
					t.Fatal(err)
				}
				s.Jobs = append(s.Jobs, job)
				actual, err := contract.Parse(job.Templates[1])
				if err != nil {
					t.Fatal(err)
				}
				payout, err := partialRetainedPayout(e, s, own, actual, true, 50)
				if err != nil || hex.EncodeToString(payout) != job.Payout {
					t.Fatal("exact tower payout refused", err)
				}
				s.Jobs[len(s.Jobs)-1].TermsHash = protocol.Digest("other terms")
				if _, err := partialRetainedPayout(e, s, own, actual, true, 50); err == nil {
					t.Fatal("other child terms authorized tower refund")
				}
			})
		}
	}
}
