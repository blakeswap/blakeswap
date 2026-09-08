package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestTradeConfirmationAcceptsReorderedSameInputs(t *testing.T) {
	for _, kind := range []string{"maker", "taker"} {
		t.Run(kind, func(t *testing.T) {
			e, request := tradeFixture(t, kind)
			backend := e.nodes[chain.Blake].(*sendBackend)
			first, second := backend.coins[0], backend.coins[0]
			amount := request.SellAmount
			if kind == "taker" {
				amount = 200000
			}
			first.Amount, second.Amount = chain.Coins(amount/2+2000), chain.Coins(amount/2+2000)
			second.TxID = strings.Repeat("34", 32)
			backend.coins = []chain.UTXO{first, second}
			if err := e.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			quote := requestQuote(t, e, request)
			if len(quote.Funds.Inputs) != 2 {
				t.Fatal("fixture must require both reviewed inputs")
			}
			backend.coins = []chain.UTXO{second, first}
			confirm := confirmation(quote)
			result := confirmQuote(t, e, confirm)
			if result.State != "accepted" || result.Error != "" {
				t.Fatal("unchanged input set was rejected after backend reordered it", result)
			}
			owner := "offer/" + result.ID
			if kind == "taker" {
				owner = "swap/" + result.ID
			}
			actual := e.s.CoinReservations[owner].Inputs
			if len(actual) != 2 || actual[0] != quote.Funds.Inputs[1] || actual[1] != quote.Funds.Inputs[0] {
				t.Fatal("fixture did not reserve the same inputs in changed backend order", actual)
			}
			digest := protocol.Digest(e.s.TradeReceipts[confirm.RequestID])
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			if retry := confirmQuote(t, e, confirm); retry != result || protocol.Digest(e.s.TradeReceipts[confirm.RequestID]) != digest {
				t.Fatal("reordered input acceptance changed on persisted retry", retry)
			}
		})
	}
}

func TestTradeInputSetBindingRejectsChangedOrDuplicateInputs(t *testing.T) {
	a, b, c := CoinOutpoint{TxID: strings.Repeat("11", 32)}, CoinOutpoint{TxID: strings.Repeat("22", 32)}, CoinOutpoint{TxID: strings.Repeat("33", 32)}
	for _, tc := range []struct {
		name             string
		actual, reviewed []CoinOutpoint
		chain            chain.ID
		accepted         bool
	}{
		{"same", []CoinOutpoint{a, b}, []CoinOutpoint{a, b}, chain.BTC, true},
		{"reordered", []CoinOutpoint{b, a}, []CoinOutpoint{a, b}, chain.BTC, true},
		{"missing", []CoinOutpoint{a}, []CoinOutpoint{a, b}, chain.BTC, false},
		{"additional", []CoinOutpoint{a, b, c}, []CoinOutpoint{a, b}, chain.BTC, false},
		{"substituted", []CoinOutpoint{a, c}, []CoinOutpoint{a, b}, chain.BTC, false},
		{"different_output", []CoinOutpoint{a, {TxID: b.TxID, Vout: 1}}, []CoinOutpoint{a, b}, chain.BTC, false},
		{"duplicate_actual", []CoinOutpoint{a, a}, []CoinOutpoint{a, b}, chain.BTC, false},
		{"duplicate_review", []CoinOutpoint{a, b}, []CoinOutpoint{a, a}, chain.BTC, false},
		{"matching_duplicates", []CoinOutpoint{a, a}, []CoinOutpoint{a, a}, chain.BTC, false},
		{"empty", nil, nil, chain.BTC, false},
		{"different_chain", []CoinOutpoint{a, b}, []CoinOutpoint{a, b}, chain.Blake, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{s: State{CoinReservations: map[string]CoinReservation{"offer/review": {Chain: tc.chain, Inputs: tc.actual}}}}
			receipt := &TradeReceipt{Snapshot: TradeQuoteSnapshot{Quote: TradeQuote{PaidChain: chain.BTC, Funds: FundsPreflight{Inputs: tc.reviewed}}}}
			before := protocol.Digest([]any{e.s.CoinReservations, receipt})
			if err := e.validateTradeInputs("offer/review", receipt); (err == nil) != tc.accepted {
				t.Fatalf("binding accepted=%v: %v", tc.accepted, err)
			}
			if protocol.Digest([]any{e.s.CoinReservations, receipt}) != before {
				t.Fatal("binding check mutated recorded order or receipt")
			}
		})
	}
}
