package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestSendReplacementPersistsVariantsAndRecognizesEarlierConfirmation(t *testing.T) {
	e, b, p := sendFixture(t)
	p.MaxFee = 20000
	broadcasts := 0
	b.broadcast = func(raw string) (string, error) {
		broadcasts++
		var saved State
		if _, err := e.vault.Load(&saved); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, v := range saved.Sends[p.ID].History {
			if v.Raw == raw {
				found = true
			}
		}
		if !found {
			t.Fatal("broadcast before variant persisted")
		}
		return "", context.DeadlineExceeded
	}
	raw, _ := json.Marshal(p)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	original := e.s.Sends[p.ID].TxID
	bump := BumpRequest{ID: p.ID, Kind: "send", ExpectedTxID: original, Fee: 6000}
	result, err := e.bumpSend(context.Background(), bump)
	if err != nil {
		t.Fatal(err)
	}
	if result.TxID == original || result.State != "saved" || result.Error == "" {
		t.Fatal(result)
	}
	before := broadcasts
	if _, err = e.bumpSend(context.Background(), bump); err != nil || broadcasts != before {
		t.Fatal("duplicate bump changed state", err)
	}
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	send := e.s.Sends[p.ID]
	if len(send.History) != 2 {
		t.Fatal("restart lost lineage")
	}
	first, _ := contract.Parse(send.History[0].Raw)
	second, _ := contract.Parse(send.History[1].Raw)
	if first.TxOut[0].Value != second.TxOut[0].Value || string(first.TxOut[0].PkScript) != string(second.TxOut[0].PkScript) || first.TxIn[0].PreviousOutPoint != second.TxIn[0].PreviousOutPoint {
		t.Fatal("replacement changed authorized payment")
	}
	b.transaction = func(_ context.Context, id string) (chain.Transaction, error) {
		if id == original {
			return chain.Transaction{Confirmations: 8}, nil
		}
		return chain.Transaction{}, &chain.RPCError{Code: -5}
	}
	e.advanceSends(context.Background())
	if send.TxID != original || send.Confirmations != 8 || send.Fee != p.Fee {
		t.Fatal("ignored earlier confirming variant", send.public())
	}
	b.transaction = nil
	send.LastAttempt = 0
	e.advanceSends(context.Background())
	if send.TxID != result.TxID || send.Confirmations != 0 || !e.reservedCoins(p.Chain, "")[pointKey(p.Inputs[0])] {
		t.Fatal("deep reorg lost replacement or reservation")
	}
	b.transaction = func(context.Context, string) (chain.Transaction, error) {
		return chain.Transaction{}, context.DeadlineExceeded
	}
	e.advanceSends(context.Background())
	if send.State != "unknown" || !e.publicCoins()[0].Reserved {
		t.Fatal("unknown treated as absent")
	}
}

func TestSendFeeReviewAndReplacementLimits(t *testing.T) {
	for _, kind := range []string{"stale", "size", "cap", "dust", "legacy", "funding"} {
		t.Run(kind, func(t *testing.T) {
			e, b, p := sendFixture(t)
			p.MaxFee = 20000
			b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
			if kind == "stale" || kind == "size" {
				p.Rate = 100000
				p.Timestamp = time.Now().Unix()
				if kind == "stale" {
					p.Timestamp -= 121
				}
				raw, _ := json.Marshal(p)
				if _, err := e.sendCoins(context.Background(), raw); err == nil {
					t.Fatal("accepted invalid fee review")
				}
				return
			}
			if kind == "legacy" {
				p.MaxFee = 0
			}
			raw, _ := json.Marshal(p)
			if _, err := e.sendCoins(context.Background(), raw); err != nil {
				t.Fatal(err)
			}
			fee := int64(20001)
			if kind == "legacy" {
				fee = 6000
			}
			if kind == "dust" {
				e.s.Sends[p.ID].MaxFee = 100000
				fee = 99999
			}
			params, _ := json.Marshal(BumpRequest{ID: p.ID, Kind: "send", Fee: fee, ExpectedTxID: e.s.Sends[p.ID].TxID})
			if kind == "funding" {
				params = []byte(`{"kind":"funding"}`)
			}
			if _, err := e.bumpTransaction(context.Background(), params); err == nil {
				t.Fatal("accepted unsafe acceleration")
			}
		})
	}
}

func TestFeeQuoteManualFallbackSizeDustAndFundingPersistence(t *testing.T) {
	e, _, p := sendFixture(t)
	quote := FeeQuoteRequest{Kind: "send", Chain: p.Chain, Destination: p.Destination, Amount: p.Amount, Inputs: p.Inputs}
	raw, _ := json.Marshal(quote)
	q, err := e.quoteFee(context.Background(), raw)
	if err != nil || q.Error == "" || q.Estimate.State != "unavailable" || q.Fee != 0 {
		t.Fatal("fabricated estimate", q, err)
	}
	quote.Fee = 1500
	raw, _ = json.Marshal(quote)
	q, err = e.quoteFee(context.Background(), raw)
	if err != nil || q.Fee != 1500 || q.Change != 98500 || q.VSize < 140 {
		t.Fatal(q, err)
	}
	quote.Amount = 998000
	raw, _ = json.Marshal(quote)
	if _, err = e.quoteFee(context.Background(), raw); err == nil {
		t.Fatal("dust quote accepted")
	}
	quote.Kind = "funding"
	quote.Amount = 100000
	quote.Inputs = nil
	quote.Fee = 6500
	raw, _ = json.Marshal(quote)
	q, err = e.quoteFee(context.Background(), raw)
	if err != nil || q.Fee != 6500 {
		t.Fatal(q, err)
	}
	result, err := e.Command(context.Background(), Request{Method: "offer.create", Params: []byte(`{"sell":"blake","sell_amount":100000,"buy_amount":200000,"funding_fee":6500,"owner_fee_cap":20000}`)})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	owner := "offer/" + offer.ID
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.reconcileReservations()
	if e.fundingFee(owner) != 6500 || e.s.FundingFees[owner].OwnerFeeCap != 20000 || len(e.s.CoinReservations[owner].Inputs) != 1 {
		t.Fatal("lost fee authorization")
	}
	if e.fundingFee("legacy") != 2000 {
		t.Fatal("legacy fee reinterpreted")
	}
	if strings.Contains(e.s.Offers[offer.ID].Content, "funding_fee") {
		t.Fatal("private funding fee leaked into public offer")
	}
}
