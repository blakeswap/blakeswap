package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestParentFillProjectionKeepsChildFeeAndQuantityAcrossColdDetail(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, sell)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s, f := e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
			p := e.publicSwap(s)
			if p.ParentID != f.ParentID || p.ParentMaker != maker.Public().Hex() || p.ParentRevision != r.Revision || p.Quantity != 400000 || p.FundingFee != 6500 || !p.AllocationKnown || p.AllocatedQuantity != 400000 || p.Allocation != FillReserved {
				t.Fatal("accepted child projection omitted its own policy or allocation", p)
			}
			a := e.s.Activities["swap/"+s.ID]
			if a.Principal != 400000 || a.CounterAmount != 520001 {
				t.Fatal("activity projected the full parent instead of exact child quantities", a.Principal, a.CounterAmount)
			}
			backend := &fundsBackend{outputs: map[string]*chain.TxOut{}}
			for _, coin := range e.knownCoins(sell) {
				out := &chain.TxOut{Value: coin.Amount, Confirmations: coin.Confirmations}
				out.Script.Hex = coin.Script
				backend.outputs[chain.OutpointKey(coin.TxID, coin.Vout)] = out
			}
			e.nodes[sell] = backend
			tx, err := e.fundReserved(context.Background(), s.Short, "swap/"+s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.commitMakerFill(s, tx); err != nil {
				t.Fatal(err)
			}
			s.ShortFunding, s.Short.TxID = contract.Hex(tx), tx.TxHash().String()
			if err := e.prepare(s, s.Short); err != nil {
				t.Fatal(err)
			}
			a = e.s.Activities["swap/"+s.ID+"/funding"]
			if a.Principal != 400000 || a.Amount != 406500 || !a.FeeKnown || a.Fee != 6500 {
				t.Fatal("funding activity lost child fee", a.Principal, a.Amount, a.Fee)
			}
			// Parent fee placement must not affect the active child's fee.
			if err := e.stageArchive("funding_fees", "offer/"+f.ParentID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if p := e.publicSwap(s); p.FundingFee != 6500 || p.Allocation != FillCommitted {
				t.Fatal("cold parent changed active child policy", p.FundingFee, p.Allocation)
			}
			// Explicit storage placement exercises detail custody independently of
			// the later canonical-depth policy which decides normal compaction.
			for _, record := range [][2]string{{"swaps", s.ID}, {"funding_fees", "swap/" + s.ID}} {
				if err := e.stageArchive(record[0], record[1]); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if e.s.FillRecords[s.ID] != nil {
				t.Fatal("cold core retained a hot child allocation")
			}
			raw, _ := json.Marshal(map[string]string{"kind": "swap", "id": s.ID, "expected_wallet": e.Config.Name, "expected_network": "regtest"})
			before := protocol.Digest(e.s)
			detail, err := e.recordDetail(raw)
			if err != nil || !detail.Archived || detail.Swap.FundingFee != 6500 || detail.Swap.Allocation != FillCommitted || detail.Swap.Quantity != 400000 || detail.Swap.Error != s.Error {
				t.Fatal("archived child detail lost exact private context", detail, err)
			}
			if protocol.Digest(e.s) != before {
				t.Fatal("detail read reactivated child authority")
			}
			if _, err := e.activateArchived("swaps", s.ID); err != nil {
				t.Fatal(err)
			}
			if e.s.FillRecords[s.ID] == nil || e.s.FundingFees["swap/"+s.ID].FundingFee != 6500 || e.s.Swaps[s.ID] == nil {
				t.Fatal("activation split core/allocation/fee companions")
			}
		})
	}
}
