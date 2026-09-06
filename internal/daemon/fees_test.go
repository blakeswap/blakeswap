package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
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
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
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

func TestOwnerLadderRequiresConsentAndPersistsBeforeEscalation(t *testing.T) {
	for _, cap := range []int64{0, 20000} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			e, b, _ := sendFixture(t)
			id := transport.RandomID()
			key, err := e.swapKey(chain.Blake, id)
			if err != nil {
				t.Fatal(err)
			}
			secret := bytes.Repeat([]byte{1}, 32)
			hash := sha256.Sum256(secret)
			pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
			target := contract.HTLC{Chain: chain.Blake, Hash: hex.EncodeToString(hash[:]), ClaimKey: pub, RefundKey: pub, RefundHeight: 1, Amount: 1000000, TxID: strings.Repeat("34", 32)}
			claim, err := contract.Spend(target, key, e.scripts[chain.Blake], 2000, false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			if refundReplaceable(target, true, chain.Observation{Tx: claim}) {
				t.Fatal("pending peer claim allowed refund race")
			}
			refundTx, err := contract.Spend(target, key, e.scripts[chain.Blake], 2000, true, target.RefundHeight, nil, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !refundReplaceable(target, true, chain.Observation{Tx: refundTx}) || refundReplaceable(target, true, chain.Observation{Tx: refundTx, Confirmations: 1}) {
				t.Fatal("refund replacement eligibility incorrect")
			}
			s := &Swap{ID: id, Role: "taker", Short: target, Long: target, OwnerFeeCap: cap, SelfClaim: contract.Hex(claim), Secret: hex.EncodeToString(secret)}
			e.s.Swaps[id] = s
			b.broadcast = func(raw string) (string, error) {
				var saved State
				if _, err := e.vault.Load(&saved); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, v := range saved.Swaps[id].SelfClaims {
					if v == raw {
						found = true
					}
				}
				if !found {
					t.Fatal("owner broadcast before persistence")
				}
				return "", context.DeadlineExceeded
			}
			for i := 0; i < 12; i++ {
				s.ClaimLastAttempt = 0
				_ = e.broadcastOwner(context.Background(), s, chain.Blake, false)
			}
			want := 1
			if cap > 0 {
				want = 3
			}
			if len(s.SelfClaims) != want {
				t.Fatal("unexpected authorized variants", len(s.SelfClaims), cap)
			}
			if cap == 0 && s.ClaimVariant != 0 {
				t.Fatal("legacy claim silently escalated")
			}
			if cap > 0 && s.ClaimVariant != 2 {
				t.Fatal("new claim failed to reach its cap")
			}
			for _, raw := range s.SelfClaims {
				tx, _ := contract.Parse(raw)
				if err := contract.VerifySignature(target, tx, false); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(tx.TxOut[0].PkScript, claim.TxOut[0].PkScript) || target.Amount-tx.TxOut[0].Value > 20000 {
					t.Fatal("owner authority changed")
				}
			}
		})
	}
}

func TestSendReplacementCanConsumeAllChange(t *testing.T) {
	e, b, p := sendFixture(t)
	p.MaxFee = 100000
	b.broadcast = func(raw string) (string, error) {
		tx, err := contract.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return tx.TxHash().String(), nil
	}
	raw, _ := json.Marshal(p)
	sent, err := e.sendCoins(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.bumpSend(context.Background(), BumpRequest{ID: p.ID, Kind: "send", Fee: 100000, ExpectedTxID: sent.TxID})
	if err != nil {
		t.Fatal(err)
	}
	s := e.s.Sends[p.ID]
	tx, err := contract.Parse(s.Raw)
	if err != nil || len(tx.TxOut) != 1 || tx.TxOut[0].Value != p.Amount || s.Change != 0 || result.Fee != 100000 {
		t.Fatal("zero-change replacement changed payment", result, err)
	}
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Sends[p.ID].Change != 0 || len(saved.Sends[p.ID].History) != 2 {
		t.Fatal("zero-change lineage was not durable")
	}
}

type stalledFeeBackend struct {
	*sendBackend
	estimates int
}

func (b *stalledFeeBackend) EstimateFee(ctx context.Context, target uint32) chain.FeeEstimate {
	b.estimates++
	<-ctx.Done()
	return chain.FeeEstimate{Chain: chain.Blake, State: "unavailable", Error: ctx.Err().Error()}
}
func (b *stalledFeeBackend) Output(ctx context.Context, id string, vout uint32) (*chain.TxOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.sendBackend.Output(ctx, id, vout)
}

func TestManualFeeQuoteDoesNotDependOnEstimator(t *testing.T) {
	e, b, p := sendFixture(t)
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend := &stalledFeeBackend{sendBackend: b}
	e.nodes[p.Chain] = backend
	for _, kind := range []string{"send", "funding"} {
		params, _ := json.Marshal(FeeQuoteRequest{Kind: kind, Chain: p.Chain, Destination: p.Destination, Amount: p.Amount, Fee: p.Fee, Inputs: p.Inputs})
		q, err := e.quoteFee(context.Background(), params)
		if err != nil || q.Fee != p.Fee || q.Change != 98500 || backend.estimates != 0 {
			t.Fatal("manual fee depended on unavailable estimator", kind, q, err, backend.estimates)
		}
	}
}

type settlementFeeScanner struct {
	observations map[string]chain.Observation
	err          error
	calls        int
}

func (s *settlementFeeScanner) Scan(context.Context, uint32, []string) (map[string]chain.Observation, error) {
	s.calls++
	return s.observations, s.err
}

func TestManualRefundAccelerationRechecksBothSettlements(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, condition := range []string{"peer_claim", "incoming_claim", "confirmed_refund", "unknown", "reorg_clock", "pending_refund"} {
			t.Run(role+"/"+condition, func(t *testing.T) {
				e, b, _ := sendFixture(t)
				id := transport.RandomID()
				key, err := e.swapKey(chain.Blake, id)
				if err != nil {
					t.Fatal(err)
				}
				secret := bytes.Repeat([]byte{2}, 32)
				hash := sha256.Sum256(secret)
				pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
				own := contract.HTLC{Chain: chain.Blake, Hash: hex.EncodeToString(hash[:]), ClaimKey: pub, RefundKey: pub, RefundHeight: 200, Amount: 1000000, TxID: transport.RandomID()}
				incoming := own
				incoming.Chain = chain.BTC
				incoming.TxID = transport.RandomID()
				if condition == "reorg_clock" {
					own.RefundHeight = 201
				}
				s := &Swap{ID: id, Role: role, Terms: &protocol.Terms{}, Stage: "refunding", Long: own, Short: incoming}
				if role == "maker" {
					s.Long, s.Short = incoming, own
				}
				for _, fee := range protocol.RescueFees {
					tx, err := contract.Spend(own, key, e.scripts[own.Chain], fee, true, own.RefundHeight, nil, 0, nil)
					if err != nil {
						t.Fatal(err)
					}
					s.SelfRefunds = append(s.SelfRefunds, contract.Hex(tx))
				}
				e.s.Swaps[id] = s
				e.heights[own.Chain] = 201 // UI snapshot predates a reorg or new spend.
				ownScan, incomingScan := &settlementFeeScanner{observations: map[string]chain.Observation{}}, &settlementFeeScanner{observations: map[string]chain.Observation{}}
				e.scanners = map[chain.ID]chain.SpendScanner{own.Chain: ownScan, incoming.Chain: incomingScan}
				base, _ := contract.Parse(s.SelfRefunds[0])
				switch condition {
				case "peer_claim", "incoming_claim":
					target, scanner := own, ownScan
					if condition == "incoming_claim" {
						target, scanner = incoming, incomingScan
					}
					claim, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, false, 0, nil, 0, secret)
					if err != nil {
						t.Fatal(err)
					}
					scanner.observations[chain.OutpointKey(target.TxID, target.Vout)] = chain.Observation{Tx: claim}
				case "confirmed_refund":
					ownScan.observations[chain.OutpointKey(own.TxID, own.Vout)] = chain.Observation{Tx: base, Confirmations: 1}
				case "pending_refund":
					ownScan.observations[chain.OutpointKey(own.TxID, own.Vout)] = chain.Observation{Tx: base}
				case "unknown":
					incomingScan.err = context.DeadlineExceeded
				}
				broadcasts := 0
				b.broadcast = func(raw string) (string, error) {
					broadcasts++
					tx, _ := contract.Parse(raw)
					return tx.TxHash().String(), nil
				}
				params, _ := json.Marshal(BumpRequest{ID: id, Kind: "refund", Fee: 6000, ExpectedTxID: base.TxHash().String()})
				_, err = e.bumpTransaction(context.Background(), params)
				if condition == "pending_refund" {
					if err != nil || broadcasts != 1 || s.RefundVariant != 1 || ownScan.calls == 0 || incomingScan.calls == 0 {
						t.Fatal("safe pending refund was not freshly checked", err, broadcasts)
					}
				} else if err == nil || broadcasts != 0 || s.RefundVariant != 0 || s.RefundAttempt != 0 {
					t.Fatal("unsafe refund changed authorization/broadcast", condition, err, broadcasts, s.RefundVariant)
				}
			})
		}
	}
}
