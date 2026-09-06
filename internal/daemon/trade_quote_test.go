package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func tradeFixture(t *testing.T, kind string) (*Engine, TradeQuoteRequest) {
	t.Helper()
	e, _, _ := sendFixture(t)
	e.Config.Name = "alice"
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := TradeQuoteRequest{Kind: kind, ExpectedWallet: "alice", ExpectedNetwork: "regtest", Sell: chain.Blake, SellAmount: 100000, BuyAmount: 200000, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}}
	if kind == "taker" {
		maker := nostr.Generate()
		offer := protocol.Offer{ID: transport.RandomID(), Network: chain.Regtest, Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 3600, Status: "open"}
		content, _ := offer.PublicJSON()
		event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(content)}
		if err := transport.Sign(&event, maker); err != nil {
			t.Fatal(err)
		}
		e.s.Book[offer.Maker+":"+offer.ID] = event
		p.Maker, p.ID, p.Sell = offer.Maker, offer.ID, offer.Sell
	}
	return e, p
}
func requestQuote(t *testing.T, e *Engine, p TradeQuoteRequest) TradeQuote {
	t.Helper()
	raw, _ := json.Marshal(p)
	q, err := e.quoteTrade(context.Background(), raw)
	if err != nil || !q.Ready || q.Error != "" {
		t.Fatal("quote not ready", q, err)
	}
	return q
}
func confirmation(q TradeQuote) ConfirmTradeRequest {
	return ConfirmTradeRequest{Token: q.Token, Revision: q.Revision, RequestID: transport.RandomID(), ExpectedWallet: q.Wallet, ExpectedNetwork: string(q.Network)}
}
func confirmQuote(t *testing.T, e *Engine, p ConfirmTradeRequest) ConfirmTradeResult {
	t.Helper()
	raw, _ := json.Marshal(p)
	result, err := e.confirmTrade(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTradeQuoteIsReadOnlyAndConfirmationSurvivesExpiryRestart(t *testing.T) {
	for _, kind := range []string{"maker", "taker"} {
		t.Run(kind, func(t *testing.T) {
			e, p := tradeFixture(t, kind)
			before, _ := json.Marshal(e.s)
			q := requestQuote(t, e, p)
			after, _ := json.Marshal(e.s)
			if !bytes.Equal(before, after) || len(e.s.CoinReservations) != 0 || len(e.s.Outbox) != 0 {
				t.Fatal("reading/cancelling quote mutated wallet")
			}
			if q.PaidChain != chain.Blake || q.ReceivedChain != chain.BTC || q.PaidTotal != q.PaidPrincipal+2000 || q.Fees.OwnerFeeCap != 20000 || len(q.Outcomes) != 2 {
				t.Fatal("wrong orientation/economics", q)
			}
			request := confirmation(q)
			got := confirmQuote(t, e, request)
			if got.State != "accepted" || got.ID != request.RequestID {
				t.Fatal(got)
			}
			// Model elapsed time without a two-minute sleep: the stored receipt's
			// quote is expired before closing/reloading the encrypted snapshot.
			e.s.TradeReceipts[request.RequestID].Snapshot.Quote.Expires = time.Now().Unix() - 1
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			e.tradeQuotes = nil
			e.tradeConfirming = nil
			// The original ephemeral quote has disappeared, and its public order may
			// have changed: only the exact persisted confirmation may return success.
			e.s.Book = map[string]nostr.Event{}
			again := confirmQuote(t, e, request)
			if again != got {
				t.Fatal("retry lost original authorization", again, got)
			}
			if kind == "maker" && len(e.s.Offers) != 1 || kind == "taker" && len(e.s.Swaps) != 1 {
				t.Fatal("duplicate local trade")
			}
			request.Revision = transport.RandomID()
			raw, _ := json.Marshal(request)
			if _, err := e.confirmTrade(context.Background(), raw); err == nil {
				t.Fatal("changed terms reused confirmation ID")
			}
		})
	}
}

func TestTradeConfirmationRejectsStaleBindingsWithoutTradeMutation(t *testing.T) {
	for _, change := range []string{"wallet", "network", "event", "revision", "expiry", "inputs"} {
		t.Run(change, func(t *testing.T) {
			e, p := tradeFixture(t, "taker")
			q := requestQuote(t, e, p)
			request := confirmation(q)
			switch change {
			case "wallet":
				request.ExpectedWallet = "bob"
			case "network":
				request.ExpectedNetwork = "mainnet"
			case "event":
				delete(e.s.Book, p.Maker+":"+p.ID)
			case "revision":
				request.Revision = transport.RandomID()
			case "expiry":
				s := e.tradeQuotes[q.Token]
				s.Quote.Expires = time.Now().Unix() - 1
				e.tradeQuotes[q.Token] = s
			case "inputs":
				if err := e.reserveCoins("offer/another", chain.Blake, 100000); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(request)
			got, err := e.confirmTrade(context.Background(), raw)
			if err == nil && got.State != "rejected" {
				t.Fatal("stale confirmation accepted", got)
			}
			if len(e.s.Swaps) != 0 || len(e.s.Offers) != 0 || len(e.s.Outbox) != 0 {
				t.Fatal("stale confirmation started a trade")
			}
			if err == nil {
				// A definitive rejection also remains definitive if the underlying state
				// recovers while a delayed duplicate of this same request arrives.
				if retry := confirmQuote(t, e, request); retry != got {
					t.Fatal("rejected ID changed meaning")
				}
			}
		})
	}
}

func TestTradeConfirmationDuplicateConcurrentRequestsCreateOneIntent(t *testing.T) {
	e, p := tradeFixture(t, "taker")
	q := requestQuote(t, e, p)
	request := confirmation(q)
	raw, _ := json.Marshal(request)
	var wg sync.WaitGroup
	results := make(chan ConfirmTradeResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.confirmTrade(context.Background(), raw)
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for r := range results {
		if r.ID != request.RequestID || (r.State != "pending" && r.State != "accepted") {
			t.Fatal(r)
		}
	}
	if len(e.s.Swaps) != 1 || len(e.s.TradeReceipts) != 1 {
		t.Fatal("concurrent duplicate created multiple intents")
	}
}

func TestTradeTimingAndOutcomeBoundsFromEachWalletPerspective(t *testing.T) {
	for _, network := range []chain.Network{chain.Regtest, chain.Mainnet, chain.Testnet} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			e, p := tradeFixture(t, "maker")
			e.Config.Network = network
			p.ExpectedNetwork = string(network)
			p.Sell = sell
			s, err := e.tradeSnapshot(p, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			q := s.Quote
			if q.PaidChain != sell || q.ReceivedChain != sell.Other() || q.RateNumerator != 2 || q.RateDenominator != 1 || q.RateDisplay != "2.00000000" {
				t.Fatal(q)
			}
			if q.Outcomes[0].NetMin != 180000 || q.Outcomes[0].NetMax != 198000 || q.Outcomes[1].NetMin != 80000 || q.Outcomes[1].NetMax != 98000 {
				t.Fatal("wrong net bounds", q.Outcomes)
			}
			if network == chain.Regtest {
				if q.Timing.Unit != "blocks" || q.Timing.OwnRefund != 48 || q.Timing.IncomingRefund != 96 {
					t.Fatal(q.Timing)
				}
			} else if q.Timing.Unit != "seconds" || q.Timing.OwnRefund != 2*24*3600 || q.Timing.IncomingRefund != 4*24*3600 {
				t.Fatal(q.Timing)
			}
			if q.Timing.FirstRevealer != "taker" || q.Timing.Confirmations != network.Confirmations() {
				t.Fatal(q.Timing)
			}
		}
	}
}

func TestTradeQuoteRequiresExplicitFeeReviewAndMatchingOrderAmounts(t *testing.T) {
	e, p := tradeFixture(t, "taker")
	for _, mutate := range []func(*TradeQuoteRequest){func(p *TradeQuoteRequest) { p.BuyAmount++ }, func(p *TradeQuoteRequest) { p.FundingFee = 0 }, func(p *TradeQuoteRequest) { p.OwnerFeeCap = 20001 }, func(p *TradeQuoteRequest) { p.TowerBPS = 50; p.TowerPubKey = "" }, func(p *TradeQuoteRequest) { p.Rate = 1000; p.Timestamp = time.Now().Unix() - 121 }} {
		changed := p
		mutate(&changed)
		raw, _ := json.Marshal(changed)
		if q, err := e.quoteTrade(context.Background(), raw); err == nil && q.Ready {
			t.Fatal("invalid policy produced actionable quote", q)
		}
	}
}

func TestTradePendingConfirmationResumesAfterCancellationAndRestart(t *testing.T) {
	e, p := tradeFixture(t, "taker")
	q := requestQuote(t, e, p)
	request := confirmation(q)
	original := e.nodes[chain.Blake]
	point := q.Funds.Inputs[0]
	out, err := original.Output(context.Background(), point.TxID, point.Vout)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fundsBackend{Backend: original, outputs: map[string]*chain.TxOut{pointKey(point): out}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e.nodes[chain.Blake] = backend
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	raw, _ := json.Marshal(request)
	go func() { _, err := e.confirmTrade(ctx, raw); done <- err }()
	select {
	case <-backend.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmation did not reach preflight")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.TradeReceipts[request.RequestID].Result.State != "pending" || len(saved.Swaps) != 0 || len(saved.CoinReservations) != 0 {
		t.Fatal("authorization was not durable before IO")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation ignored")
	}
	e.s = saved
	e.tradeQuotes = nil
	e.tradeConfirming = nil
	e.nodes[chain.Blake] = original
	result := confirmQuote(t, e, request)
	if result.State != "accepted" || result.ID != request.RequestID || len(e.s.Swaps) != 1 {
		t.Fatal("lost pending request identity", result)
	}
}

func TestTradeQuoteBindsProviderProofAndMatchesConstructedOutcomes(t *testing.T) {
	for _, kind := range []string{"maker", "taker"} {
		t.Run(kind, func(t *testing.T) {
			e, p := tradeFixture(t, kind)
			// Use a signed maker draft or a freshly signed public order at economically
			// valid tower amounts; owner outcome amounts remain on their native chain.
			if kind == "maker" {
				p.SellAmount = 600001
				p.BuyAmount = 800001
			} else {
				p.SellAmount = 100000
				p.BuyAmount = 200000
			}
			provider, _, _ := sendFixture(t)
			provider.Config.Name = "Private provider"
			provider.Config.RescueFeeBPS = 125
			if err := provider.advertiseTower(); err != nil {
				t.Fatal(err)
			}
			event := provider.s.Towers[provider.identity.Public().Hex()]
			e.ingestTower(event)
			p.TowerBPS = 125
			p.TowerPubKey = provider.identity.Public().Hex()
			q := requestQuote(t, e, p)
			want := 3
			if kind == "maker" {
				want = 4
			}
			if len(q.Outcomes) != want || q.Provider.Event == "" {
				t.Fatal("wrong provider coverage", q)
			}
			for _, outcome := range q.Outcomes {
				key, err := e.swapKey(outcome.Chain, transport.RandomID())
				if err != nil {
					t.Fatal(err)
				}
				secret := bytes.Repeat([]byte{7}, 32)
				hash := sha256.Sum256(secret)
				pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
				target := contract.HTLC{Chain: outcome.Chain, TxID: transport.RandomID(), Amount: outcome.Principal, Hash: hex.EncodeToString(hash[:]), ClaimKey: pub, RefundKey: pub, RefundHeight: 200}
				var towerScript []byte
				if strings.HasPrefix(outcome.Kind, "tower_") {
					towerScript, _ = hex.DecodeString(q.Provider.Scripts[outcome.Chain])
				}
				for _, fee := range []int64{outcome.FeeMin, outcome.FeeMax} {
					refund := strings.HasSuffix(outcome.Kind, "refund")
					lock := uint32(0)
					if refund {
						lock = target.RefundHeight
					}
					tx, err := contract.Spend(target, key, e.scripts[outcome.Chain], fee, refund, lock, towerScript, outcome.Bounty, secret)
					if err != nil {
						t.Fatal(err)
					}
					wantNet := outcome.NetMax
					if fee == outcome.FeeMax {
						wantNet = outcome.NetMin
					}
					if tx.TxOut[0].Value != wantNet {
						t.Fatal("quote does not reconcile constructed payout", outcome)
					}
					if len(towerScript) > 0 && tx.TxOut[1].Value != outcome.Bounty {
						t.Fatal("tower bounty changed")
					}
				}
			}
			// An updated signed proof requires review even when its percentage is equal.
			provider.Config.Name = "Changed provider proof"
			provider.s.Towers = nil
			if err := provider.advertiseTower(); err != nil {
				t.Fatal(err)
			}
			e.s.Towers[p.TowerPubKey] = provider.s.Towers[p.TowerPubKey]
			result := confirmQuote(t, e, confirmation(q))
			if result.State != "rejected" || !strings.Contains(result.Error, "proof changed") || len(e.s.Swaps) != 0 || len(e.s.Offers) != 0 {
				t.Fatal("stale proof committed", result)
			}
		})
	}
}

func TestTradeQuoteCannotAuthorizeAnotherRequestID(t *testing.T) {
	e, p := tradeFixture(t, "maker")
	q := requestQuote(t, e, p)
	original := confirmation(q)
	if got := confirmQuote(t, e, original); got.State != "accepted" {
		t.Fatal(got)
	}
	// Even when the funding reservation can be released later, this review has
	// already authorized exactly one request. A new trade needs a new review.
	e.tradeQuotes = nil
	raw, _ := json.Marshal(confirmation(q))
	if result, err := e.confirmTrade(context.Background(), raw); err != nil || result.State != "rejected" {
		t.Fatal("one quote authorized multiple request identities", result, err)
	}
	if len(e.s.Offers) != 1 || len(e.s.TradeReceipts) != 2 {
		t.Fatal("duplicate authorization mutated trade state")
	}
}
