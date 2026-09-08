package daemon

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestParentFillFundingSpendsOnlyAssignedInputsAndConsumesOnce(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[r.ID]
	child := e.s.FillRecords[r.ID]
	p := e.s.ParentOrders[child.ParentID]
	before := protocol.Digest(e.s.CoinReservations["swap/"+r.ID])
	e.reconcileReservations()
	if protocol.Digest(e.s.CoinReservations["swap/"+r.ID]) != before {
		t.Fatal("reservation reconciliation reassigned child inputs")
	}
	b := &fundsBackend{outputs: map[string]*chain.TxOut{}}
	for _, coin := range e.knownCoins(chain.Blake) {
		out := &chain.TxOut{Value: coin.Amount, Confirmations: coin.Confirmations}
		out.Script.Hex = coin.Script
		b.outputs[chain.OutpointKey(coin.TxID, coin.Vout)] = out
	}
	e.nodes[chain.Blake] = b
	tx, err := e.fundReserved(context.Background(), s.Short, "swap/"+r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint.Hash.String() != child.Inputs[0].TxID {
		t.Fatal("funding selected another child or parent coin")
	}
	if err := e.commitMakerFill(s, tx); err != nil {
		t.Fatal(err)
	}
	s.ShortFunding, s.Short.TxID = contract.Hex(tx), tx.TxHash().String()
	if err := e.prepare(s, s.Short); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.FillRecords[r.ID].Allocation.Disposition != FillCommitted || saved.ParentOrders[p.Offer.ID].Fees[chain.Blake].Consumed != 26500 || saved.Swaps[r.ID].ShortFunding != s.ShortFunding || len(saved.Swaps[r.ID].SelfRefunds) != len(protocol.RescueFees) {
		t.Fatal("funding bytes, refunds and consumed authorization did not persist together")
	}
	consumed := p.Fees[chain.Blake].Consumed
	if err := e.commitMakerFill(s, tx); err == nil || p.Fees[chain.Blake].Consumed != consumed {
		t.Fatal("funding signing could consume the same child twice")
	}
}

func TestParentFillFundingNeverSubstitutesUnavailableAssignedCoin(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, id)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			script := hex.EncodeToString(e.scripts[id])
			old := e.walletCoins[id][script]
			e.walletCoins[id][script] = old[1:]
			if _, err := e.fundReserved(context.Background(), e.s.Swaps[r.ID].Short, "swap/"+r.ID); err == nil {
				t.Fatal("missing assigned coin was replaced")
			}
			e.walletCoins[id][script] = old
			owner := "swap/" + transport.RandomID()
			if _, err := e.assignedFundingCoins(id, owner); err == nil {
				t.Fatal("missing assignment borrowed wallet funds")
			}
			rv := e.s.CoinReservations["swap/"+r.ID]
			rv.Inputs = append(rv.Inputs, rv.Inputs[0])
			e.s.CoinReservations["swap/"+r.ID] = rv
			if _, err := e.assignedFundingCoins(id, "swap/"+r.ID); err == nil {
				t.Fatal("duplicate assignment accepted")
			}
		})
	}
}
