package daemon

import (
	"encoding/json"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestRealSendFeeAccelerationBothChains(t *testing.T) {
	h := newHarness(t, 0)
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			e := h.engines["maker"]
			estimate := e.estimateFee(h.ctx, id, 6)
			if estimate.Chain != id || (estimate.State != "available" && estimate.State != "unavailable") {
				t.Fatal("invalid real estimate", estimate)
			}
			t.Logf("native fee estimate: %+v", estimate)
			coins := e.knownCoins(id)
			var selected []CoinOutpoint
			for _, coin := range coins {
				selected = append(selected, CoinOutpoint{coin.TxID, coin.Vout})
			}
			p := SendRequest{ID: transport.RandomID(), Chain: id, Destination: h.engines["taker"].addresses[id], Amount: 1000000, Fee: 1, MaxFee: 20000, Inputs: selected, ExpectedNetwork: "regtest"}
			raw, _ := json.Marshal(p)
			sent, err := e.sendCoins(h.ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if sent.Submitted || sent.Error == "" {
				t.Fatal("fixture did not reject below-relay fee", sent)
			}
			original := sent.TxID
			h.offline("maker")
			h.online("maker")
			e = h.engines["maker"]
			bump, err := e.bumpSend(h.ctx, BumpRequest{ID: p.ID, Kind: "send", Fee: 6000, ExpectedTxID: original})
			if err != nil {
				t.Fatal(err)
			}
			if bump.Error != "" || bump.State != "broadcast" {
				t.Fatal("replacement not accepted", bump)
			}
			second := bump.TxID
			bump, err = e.bumpSend(h.ctx, BumpRequest{ID: p.ID, Kind: "send", Fee: 20000, ExpectedTxID: second})
			if err != nil || bump.Error != "" {
				t.Fatal("mempool RBF failed", bump, err)
			}
			s := e.s.Sends[p.ID]
			for _, v := range s.History {
				tx, err := contract.Parse(v.Raw)
				if err != nil || tx.TxOut[0].Value != p.Amount || len(tx.TxIn) != len(p.Inputs) {
					t.Fatal("payment changed", err)
				}
			}
			h.mine(id, 2)
			e.advanceSends(h.ctx)
			if s.Confirmations < 2 || s.TxID != bump.TxID {
				t.Fatal("replacement not confirmed", s.public())
			}
			var blockHash string
			if err := h.nodes[id].Call(h.ctx, "getblockhash", &blockHash, h.engines["maker"].heights[id]+1); err != nil {
				t.Fatal(err)
			}
			if err := h.nodes[id].Call(h.ctx, "invalidateblock", nil, blockHash); err != nil {
				t.Fatal(err)
			}
			e.advanceSends(h.ctx)
			if s.Confirmations != 0 || len(s.History) != 3 || !e.reservedCoins(id, "")[pointKey(p.Inputs[0])] {
				t.Fatal("reorg lost lineage/reservations")
			}
			if err := h.nodes[id].Call(h.ctx, "reconsiderblock", nil, blockHash); err != nil {
				t.Fatal(err)
			}
			e.advanceSends(h.ctx)
			if s.Confirmations < 2 {
				t.Fatal("replacement not restored")
			}
			t.Logf("%s rejected=%s intermediate=%s confirmed=%s", id, original, second, bump.TxID)
		})
	}
}

func TestRealReviewedFundingAndOwnerAcceleration(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 0)
			id := h.fundBothFees(sell, 0, 6500, 20000)
			maker := h.swap("maker", id)
			for _, target := range []contract.HTLC{maker.Long, maker.Short} {
				record, err := h.nodes[target.Chain].Transaction(h.ctx, target.TxID)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := contract.Parse(record.Hex)
				if err != nil {
					t.Fatal(err)
				}
				var total int64
				for _, input := range tx.TxIn {
					prev, err := h.nodes[target.Chain].Transaction(h.ctx, input.PreviousOutPoint.Hash.String())
					if err != nil {
						t.Fatal(err)
					}
					previous, err := contract.Parse(prev.Hex)
					if err != nil {
						t.Fatal(err)
					}
					total += previous.TxOut[input.PreviousOutPoint.Index].Value
				}
				for _, out := range tx.TxOut {
					total -= out.Value
				}
				if total != 6500 {
					t.Fatal("reviewed funding fee changed", total)
				}
			}
			h.offline("maker")
			h.online("taker")
			h.tick("taker")
			taker := h.swap("taker", id)
			if taker.OwnerFeeCap != 20000 || len(taker.SelfClaims) != 3 {
				t.Fatal("owner consent/variants lost after restart")
			}
			txid, fee := settlementVariant(taker.SelfClaims, taker.ClaimVariant, taker.Long, taker.Short, false)
			if fee < 20000 {
				raw, _ := json.Marshal(BumpRequest{ID: id, Kind: "claim", Fee: 20000, ExpectedTxID: txid})
				if result, err := h.engines["taker"].bumpTransaction(h.ctx, raw); err != nil || result.Error != "" {
					t.Fatal("owner acceleration failed", result, err)
				}
			}
			h.mine(taker.Short.Chain, 2)
			h.online("maker")
			h.tick("maker")
			maker = h.swap("maker", id)
			if maker.OwnerFeeCap != 20000 || len(maker.SelfClaims) != 3 {
				t.Fatal("maker claim policy lost")
			}
			h.mine(maker.Long.Chain, 2)
			h.tick("maker", "taker")
			if h.swap("maker", id).Stage != "completed" || h.swap("taker", id).Stage != "completed" {
				t.Fatal("accelerated swap did not complete")
			}
		})
	}
}
