package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestParentFillRetirementReturnsOnlyUnfundedChildAndCannotRevive(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "open parent"
		if closed {
			name = "cancelled parent"
		}
		t.Run(name, func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.Blake)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s, f := e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
			p := e.s.ParentOrders[f.ParentID]
			if closed {
				raw, _ := json.Marshal(map[string]string{"id": p.Offer.ID, "expected_event_id": e.s.Offers[p.Offer.ID].ID.Hex()})
				if _, err := e.cancelOffer(raw); err != nil {
					t.Fatal(err)
				}
				if p.Quantities.Withdrawn != 600000 || p.Quantities.Reserved != 400000 || len(e.s.CoinReservations["swap/"+s.ID].Inputs) != 1 {
					t.Fatal("parent cancellation released accepted child resources")
				}
			}
			e.clocks[s.Long.Chain] = s.Terms.Long.RefundHeight
			e.clocks[s.Short.Chain] = s.Terms.Short.RefundHeight
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil {
				t.Fatal(err)
			}
			if !f.FundingDisabled || f.Allocation.EverCommitted || p.Quantities.Reserved != 0 || p.Fees[chain.Blake].Reserved != 0 || p.Fees[chain.Blake].Consumed != 0 || len(e.s.CoinReservations["swap/"+s.ID].Inputs) != 0 {
				t.Fatal("unfunded child did not irreversibly retire with its resource release")
			}
			if closed {
				if f.Allocation.Disposition != FillReleased || f.Allocation.currentQuantity() != 400000 || p.Quantities.Released != 1000000 || p.Quantities.Available != 0 {
					t.Fatal("cancelled-parent child was returned to available quantity")
				}
			} else if f.Allocation.Disposition != FillRetired || f.Allocation.currentQuantity() != 0 || p.Quantities.Available != 1000000 || p.Quantities.Released != 0 {
				t.Fatal("returned child double-counted its original quantity")
			}
			for _, d := range e.s.Outbox {
				if d.SwapID == s.ID && d.Type == "accepted" && (!d.Retired || d.Acknowledged) {
					t.Fatal("old acceptance was not retired distinctly from acknowledgment")
				}
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if saved.FillRecords[s.ID].Allocation != f.Allocation || !saved.FillRecords[s.ID].FundingDisabled {
				t.Fatal("quantity return was not persisted with irreversible refusal")
			}
			// A clock reorg and a late peer update cannot grant own funding.
			e.clocks[chain.BTC], e.clocks[chain.Blake] = 200, 200
			s.Stage = "late peer funding"
			before := protocol.Digest(*p)
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil || s.ShortFunding != "" || protocol.Digest(*p) != before {
				t.Fatal("returned child regained own funding after clock reorg")
			}
			if err := applyFillRequest(t, e, r, now); err != nil || protocol.Digest(*p) != before {
				t.Fatal("closed exact request allocated again")
			}
		})
	}
}

func TestParentFillRetirementRefusesUnknownOrPreviouslySignedCustody(t *testing.T) {
	for _, change := range []string{"ordinary error", "outage", "imported", "signed", "own txid", "refund bundle"} {
		t.Run(change, func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, chain.BTC)
			r := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s := e.s.Swaps[r.ID]
			e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
			gate := e.gate(s.Terms, "fund-short")
			if !protocol.FundingWindowClosed(gate) {
				t.Fatal("fixture funding window still open")
			}
			switch change {
			case "ordinary error":
				gate = errors.New("clock unavailable")
			case "outage":
				e.chainFresh[chain.BTC] = false
			case "imported":
				e.s.FillRecords[r.ID].ImportedUncertain = true
			case "signed":
				s.ShortFunding = "saved exact bytes"
			case "own txid":
				s.Short.TxID = protocol.Digest("own funding")
			case "refund bundle":
				s.SelfRefunds = []string{"signed refund"}
			}
			before := protocol.Digest(e.s)
			if err := e.retireUnfundedMaker(s, gate); err == nil || protocol.Digest(e.s) != before {
				t.Fatal("uncertain or signed child returned quantity or money")
			}
		})
	}
}

func TestParentFillRetiredMakerKeepsLatePeerObservationsAcrossReload(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, sell)
			e.s.Seen = map[string]string{}
			r := admissionRequest(t, e, maker, 400000)
			peer := nostr.Generate()
			r.Taker = peer.Public().Hex()
			key, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			r.Keys[sell.Other()] = hex.EncodeToString(key.PubKey().SerializeCompressed())
			if err := applyFillRequest(t, e, r, now); err != nil {
				t.Fatal(err)
			}
			s, f := e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
			e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil {
				t.Fatal(err)
			}
			if !f.FundingDisabled || f.Allocation.currentQuantity() != 0 {
				t.Fatal("fixture did not irreversibly return child")
			}
			before := protocol.Digest(*e.s.ParentOrders[f.ParentID])
			pk, err := s.Long.PkScript()
			if err != nil {
				t.Fatal(err)
			}
			funding := wire.NewMsgTx(2)
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
			funding.AddTxOut(wire.NewTxOut(s.Long.Amount, pk))
			raw := contract.Hex(funding)
			body, _ := json.Marshal(fundingMessage{TermsHash: protocol.Digest(s.Terms), Raw: raw})
			msg := transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "long-funded", SwapID: s.ID, Body: body}
			event, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), msg)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.receive(event); err != nil {
				t.Fatal(err)
			}
			if s.LongFunding != raw || s.Long.TxID != funding.TxHash().String() {
				t.Fatal("authenticated funding was not retained")
			}
			refund, err := contract.Spend(s.Long, key, e.scripts[s.Long.Chain], 2000, true, s.Long.RefundHeight, nil, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = contract.VerifySignature(s.Long, refund, true); err != nil {
				t.Fatal(err)
			}
			obs := chain.Observation{Tx: refund, TxID: refund.TxHash().String(), Height: 200, Confirmations: 6}
			all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
			all[s.Long.Chain][chain.OutpointKey(s.Long.TxID, s.Long.Vout)] = obs
			if err = e.advanceSwap(context.Background(), s, all); err != nil {
				t.Fatal(err)
			}
			if s.ShortFunding != "" || s.ShortSent || !f.FundingDisabled || f.Allocation.currentQuantity() != 0 || protocol.Digest(*e.s.ParentOrders[f.ParentID]) != before {
				t.Fatal("late refund changed own authority")
			}
			if s.LongSpend != obs.TxID || s.LongConfirmations != 6 || s.Stage != "aborted; counterparty refunded" {
				t.Fatalf("confirmed authenticated late refund discarded: spend=%q confirmations=%d stage=%q", s.LongSpend, s.LongConfirmations, s.Stage)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			s, f = e.s.Swaps[r.ID], e.s.FillRecords[r.ID]
			if s.LongSpend != obs.TxID || s.LongConfirmations != 6 || !f.FundingDisabled || f.Allocation.currentQuantity() != 0 {
				t.Fatal("reload lost late observation or irreversible refusal")
			}
			e.chainFresh[s.Long.Chain] = false
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil || s.LongSpend != obs.TxID || s.Stage != "aborted; counterparty refunded" {
				t.Fatal("unknown source erased saved positive peer outcome")
			}
			e.chainFresh[s.Long.Chain] = true
			all[s.Long.Chain] = map[string]chain.Observation{}
			e.clocks[chain.BTC], e.clocks[chain.Blake] = 200, 200
			if err := e.advanceSwap(context.Background(), s, all); err != nil || s.LongSpend != "" || s.LongConfirmations != 0 || s.Stage != "own funding disabled; awaiting peer refund" {
				t.Fatal("current contradiction did not reopen peer monitoring")
			}
			if s.ShortFunding != "" || f.Allocation.currentQuantity() != 0 || protocol.Digest(*e.s.ParentOrders[f.ParentID]) != before {
				t.Fatal("reopened monitoring allocated or signed own funding")
			}
			all[s.Long.Chain][chain.OutpointKey(s.Long.TxID, s.Long.Vout)] = obs
			if err := e.advanceSwap(context.Background(), s, all); err != nil || s.Stage != "aborted; counterparty refunded" {
				t.Fatal("fresh exact refund did not restore the peer projection")
			}
			assertRetiredImportPeerRecovery(t, e, s, f, obs, now)
		})
	}
}
