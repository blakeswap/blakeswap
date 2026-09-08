package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestFundingAncestryManualRefundRequiresFreshProof(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(role+"/"+string(sell), func(t *testing.T) {
				e, children, nodes := ancestryGraph(t, role, sell)
				s := children[1]
				own, _, _ := localFunding(s)
				nodes[own.Chain].errors[own.TxID] = context.DeadlineExceeded
				if err := e.refreshFundingAncestry(context.Background(), s); err == nil || !s.FundingAncestryHeld {
					t.Fatal("fixture did not establish an ancestry hold", err)
				}
				e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &settlementFeeScanner{observations: map[string]chain.Observation{}}, chain.Blake: &settlementFeeScanner{observations: map[string]chain.Observation{}}}
				base, err := contract.Parse(s.SelfRefunds[0])
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(BumpRequest{ID: s.ID, Kind: "refund", Fee: 6000, ExpectedTxID: base.TxHash().String()})
				custody, money := fundingCustodyHash(s), protocol.Digest(e.s.FillRecords[s.ID])
				if result, err := e.bumpTransaction(context.Background(), raw); !errors.Is(err, errFundingAncestry) || len(nodes[own.Chain].broadcasts) != 0 {
					t.Fatal("manual refund bypassed unknown ancestry", result, err)
				}
				if s.RefundVariant != 0 || s.RefundLastAttempt != 0 || !s.FundingAncestryHeld || custody != fundingCustodyHash(s) || money != protocol.Digest(e.s.FillRecords[s.ID]) {
					t.Fatal("held refund changed custody or publication state")
				}
				delete(nodes[own.Chain].errors, own.TxID)
				result, err := e.bumpTransaction(context.Background(), raw)
				if err != nil || result.State != "broadcast" || result.Error != "" || !e.fundingAncestryReady(s) || len(nodes[own.Chain].broadcasts) != 1 || nodes[own.Chain].broadcasts[0] != s.SelfRefunds[1] {
					t.Fatal("fresh canonical proof did not release exact authorized refund", result, err)
				}
				if custody != fundingCustodyHash(s) || money != protocol.Digest(e.s.FillRecords[s.ID]) {
					t.Fatal("manual retry changed signed custody or inventory")
				}
			})
		}
	}
}

func TestFundingAncestryPublicationPolicyRechecksProof(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(role+"/"+string(sell), func(t *testing.T) {
				e, children, nodes := ancestryGraph(t, role, sell)
				s := children[1]
				own, _, _ := localFunding(s)
				for _, refund := range []bool{false, true} {
					if err := e.recoveryOwnerPolicy(s, refund); !errors.Is(err, errFundingAncestry) {
						t.Fatal("missing current proof authorized private settlement", refund, err)
					}
				}
				if err := e.refreshFundingAncestry(context.Background(), s); err != nil {
					t.Fatal(err)
				}
				for _, refund := range []bool{false, true} {
					if err := e.recoveryOwnerPolicy(s, refund); err != nil {
						t.Fatal("current proof rejected", refund, err)
					}
				}
				// The publication callback runs again after backend selection, so a
				// proof from the previous source must no longer authorize a spend.
				nodes[own.Chain].generation++
				for _, refund := range []bool{false, true} {
					if err := e.recoveryOwnerPolicy(s, refund); !errors.Is(err, errFundingAncestry) {
						t.Fatal("source change retained private settlement authority", refund, err)
					}
				}
				s.SecretObserved = true
				if err := e.recoveryOwnerPolicy(s, false); err != nil {
					t.Fatal("already public secret rescue was held", err)
				}
				if err := e.recoveryOwnerPolicy(s, true); !errors.Is(err, errFundingAncestry) {
					t.Fatal("public secret authorized unsafe refund", err)
				}
			})
		}
	}
}
