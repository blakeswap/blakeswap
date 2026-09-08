package daemon

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
)

// This measures the admitted payment shape using actual signed bytes on both
// chains. The transport allowance below is deliberately larger than these
// local records; arbitrary legacy imported data remains governed by actual
// encoded bytes and disk checks, not this normal-operation estimate.
func TestCapacityPaymentContinuationEncoding(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			e, _ := receiveEngine(t)
			entry := e.receiveBook[id][0]
			coins := make([]chain.UTXO, 50)
			for i := range coins {
				coins[i] = chain.UTXO{TxID: fmt.Sprintf("%064x", i+1), Amount: 100000, Script: hex.EncodeToString(entry.script), Confirmations: 1}
			}
			s := &WalletSend{PublicSend: PublicSend{ID: transport.RandomID(), Chain: id, Destination: entry.address, Amount: 1000000, MaxFee: 1000000}, Coins: coins}
			maximumRaw := 0
			for i := 0; i < 16; i++ {
				fee := int64(2000 + i*4000)
				tx, err := contract.PayWithKeys(id, s.Amount, entry.script, coins, map[string]*btcec.PrivateKey{coins[0].Script: entry.key}, entry.script, fee)
				if err != nil {
					t.Fatal(err)
				}
				raw := contract.Hex(tx)
				maximumRaw = max(maximumRaw, len(raw))
				variant := SignedVariant{PublicVariant: PublicVariant{TxID: tx.TxHash().String(), Fee: fee, Submitted: true}, Raw: raw}
				s.History = append(s.History, variant)
				s.Variants = append(s.Variants, variant.PublicVariant)
				s.Raw, s.TxID, s.Fee = raw, variant.TxID, fee
			}
			encoded, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			// 50 inputs, 17 serialized copies (16 history + current), hex
			// expansion, and two extra DER-signature bytes per input cover
			// signature-length differences between deterministic fixtures.
			signatureHeadroom := 50 * 17 * 2 * 2
			if len(encoded)+signatureHeadroom >= activeRecoveryReserve/2 {
				t.Fatal("maximum admitted payment no longer fits its continuation estimate", len(encoded), signatureHeadroom)
			}
			t.Logf("chain=%s signed_inputs=50 variants=16 max_raw_hex=%d encoded_record=%d DER_headroom=%d", id, maximumRaw, len(encoded), signatureHeadroom)
		})
	}
}

func TestCapacitySwapContinuationEncoding(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, s, _, secret := isolatedFixtureSell(t, "maker", sell, FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000})
			s.Protection = &protocol.Tower{Version: protocol.Version, Network: chain.Regtest, PubKey: e.identity.Public().Hex(), BPS: 100, Scripts: map[chain.ID]string{chain.BTC: hex.EncodeToString(e.scripts[chain.BTC]), chain.Blake: hex.EncodeToString(e.scripts[chain.Blake])}}
			for _, c := range []*contract.HTLC{&s.Long, &s.Short} {
				entry := e.receiveBook[c.Chain][0]
				coins := make([]chain.UTXO, 50)
				for i := range coins {
					coins[i] = chain.UTXO{TxID: fmt.Sprintf("%064x", i+1), Amount: 100000, Script: hex.EncodeToString(entry.script), Confirmations: 1}
				}
				tx, err := contract.FundWithKeys(*c, coins, map[string]*btcec.PrivateKey{coins[0].Script: entry.key}, entry.script, 2000)
				if err != nil {
					t.Fatal(err)
				}
				c.TxID = tx.TxHash().String()
				if c == &s.Short {
					inputs := make([]CoinOutpoint, len(coins))
					for i, coin := range coins {
						inputs[i] = CoinOutpoint{TxID: coin.TxID, Vout: coin.Vout}
					}
					e.s.FillRecords[s.ID].Inputs = inputs
					e.s.CoinReservations["swap/"+s.ID] = CoinReservation{Chain: c.Chain, Inputs: append([]CoinOutpoint{}, inputs...)}
				}
				if c == &s.Long {
					s.LongFunding = contract.Hex(tx)
				} else {
					s.ShortFunding = contract.Hex(tx)
				}
			}
			s.SelfRefunds = nil
			s.Jobs = nil
			// This maximal-input model is a separate initial checkpoint. Its
			// exact input set and protection cannot replace saved child custody.
			offer := s.Terms.Offer()
			offer.TowerBPS, offer.Tower = s.Protection.BPS, s.Protection
			policy := e.s.FundingFees["swap/"+s.ID]
			parent, err := newParentOrder(offer, policy, FillOrderFields{FillPolicy: offer.FillPolicy, FeeBudgets: map[chain.ID]int64{sell: 22000, sell.Other(): 20000}, BountyBudgets: map[chain.ID]int64{sell: protocol.Bounty(s.Short.Amount, offer.TowerBPS), sell.Other(): protocol.Bounty(s.Long.Amount, offer.TowerBPS)}}, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			reserved, allocation, err := parent.reserveFill(s.Request)
			if err != nil {
				t.Fatal(err)
			}
			allocation.Inputs = append([]CoinOutpoint{}, e.s.FillRecords[s.ID].Inputs...)
			committed, allocationValue, err := reserved.transitionFill(*allocation, FillCommitted, false)
			if err != nil {
				t.Fatal(err)
			}
			e.s.ParentOrders[offer.ID], e.s.FillRecords[s.ID] = &committed, &allocationValue
			e.s.FillKeys = map[string]string{}
			if err := e.retainSwapIdentity(s); err != nil {
				t.Fatal(err)
			}
			source := e
			e = conservationRestoredEngine(t, source, source.s)
			s = e.s.Swaps[s.ID]
			if err := e.prepare(s, s.Short); err != nil {
				t.Fatal(err)
			}
			key, err := e.swapKey(s.Long.Chain, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := contract.Spend(s.Long, key, e.scripts[s.Long.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			s.SelfClaim = contract.Hex(claim)
			if err := e.prepareClaimVariants(s); err != nil {
				t.Fatal(err)
			}
			if len(s.SelfClaims) != 3 || len(s.SelfRefunds) != 3 || len(s.Jobs) != 2 {
				t.Fatal("fixture lacks full owner/rescue ladders")
			}
			s.Receipts = map[string]protocol.Receipt{}
			e.s.TowerJobs = map[string]*TowerJob{}
			for _, job := range s.Jobs {
				s.Receipts[job.ID] = protocol.Receipt{Version: protocol.Version, JobID: job.ID, Digest: protocol.Digest(job)}
				e.s.TowerJobs[job.ID] = &TowerJob{Job: job, Secret: s.Secret, Variants: transactionIDs(job.Templates)}
			}
			for _, message := range []struct {
				typ  string
				body any
			}{
				{"accepted", s.Terms}, {"long-funded", fundingMessage{protocol.Digest(s.Terms), s.LongFunding}}, {"short-funded", fundingMessage{protocol.Digest(s.Terms), s.ShortFunding}},
			} {
				if err := e.queue(s.Request.Taker, message.typ, s.ID, message.body); err != nil {
					t.Fatal(err)
				}
			}
			markRestored(t, e)
			encoded, err := json.Marshal(e.s)
			if err != nil {
				t.Fatal(err)
			}
			// Include both separately retained tower registrations as well as
			// the owner's two job copies and all outbound encrypted messages.
			if len(encoded) >= activeRecoveryReserve/2 {
				t.Fatal("normal maximal-input continuation exceeds estimate", len(encoded))
			}
			t.Logf("sell=%s encoded_checkpoint=%d core_jobs=2 owner_refunds=3 owner_claims=3 funding_inputs_per_leg=50 retained_outbox=%d", sell, len(encoded), len(e.s.Recovery.Outbox))
		})
	}
}
