package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

type FeeLimits struct {
	Send    int64 `json:"send"`
	Funding int64 `json:"funding"`
	Owner   int64 `json:"owner"`
	Tower   int64 `json:"tower"`
}

// Caps are expressed in each chain's native satoshis, never converted between
// assets. Existing v1 signed settlement ladders retain their original cap.
func feeLimits(id chain.ID) FeeLimits {
	if !id.Valid() {
		return FeeLimits{}
	}
	return FeeLimits{Send: 1000000, Funding: 100000, Owner: 20000, Tower: 20000}
}

type FeeSelection struct {
	OwnerFeeCap int64 `json:"owner_fee_cap"`
	FundingFee  int64 `json:"funding_fee"`
	Rate        int64 `json:"rate_sat_kvb"`
	Timestamp   int64 `json:"fee_timestamp"`
}

type FeeQuoteRequest struct {
	Kind        string         `json:"kind"`
	Chain       chain.ID       `json:"chain"`
	Destination string         `json:"destination"`
	Amount      int64          `json:"amount"`
	Fee         int64          `json:"fee"`
	Target      uint32         `json:"target"`
	Inputs      []CoinOutpoint `json:"inputs"`
}

type FeeQuote struct {
	Estimate chain.FeeEstimate `json:"estimate"`
	Limits   FeeLimits         `json:"limits"`
	Fee      int64             `json:"fee"`
	Amount   int64             `json:"amount"`
	Change   int64             `json:"change"`
	Total    int64             `json:"total"`
	VSize    int64             `json:"vsize"`
	Inputs   []CoinOutpoint    `json:"inputs"`
	Error    string            `json:"error"`
}

func (e *Engine) estimateFee(ctx context.Context, id chain.ID, target uint32) chain.FeeEstimate {
	return estimateBackend(ctx, id, e.nodes[id], target)
}
func estimateBackend(ctx context.Context, id chain.ID, node chain.Backend, target uint32) chain.FeeEstimate {
	if target == 0 {
		target = 6
	}
	if backend, ok := node.(chain.FeeEstimator); ok {
		f := backend.EstimateFee(ctx, target)
		if f.Chain != id {
			return chain.FeeEstimate{Chain: id, State: "unavailable", Error: "estimator returned a different chain"}
		}
		if f.State == "available" && !f.Current(time.Now()) {
			f.State = "stale"
			f.Error = "fee estimate expired; refresh or select a manual fee"
		}
		return f
	}
	return chain.FeeEstimate{Chain: id, Target: target, State: "unavailable", Error: "backend has no fee estimator; select a manual total fee"}
}

func (e *Engine) quoteFee(ctx context.Context, raw json.RawMessage) (FeeQuote, error) {
	var p FeeQuoteRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return FeeQuote{}, err
	}
	if !p.Chain.Valid() || (p.Kind != "send" && p.Kind != "funding") || p.Amount < contract.Dust || p.Amount > contract.MaxMoney || p.Fee < 0 || p.Fee > feeLimits(p.Chain).Send || len(p.Inputs) > 50 {
		return FeeQuote{}, errors.New("invalid fee quote")
	}
	e.mu.Lock()
	if err := CheckCommandNetwork(Request{Method: "fee.quote", Params: raw}, e.Config.Network, false); err != nil {
		e.mu.Unlock()
		return FeeQuote{}, err
	}
	coins := e.knownCoins(p.Chain)
	reserved := e.reservedCoins(p.Chain, "")
	network, node := e.Config.Network, e.nodes[p.Chain]
	changeScript := append([]byte(nil), e.scripts[p.Chain]...)
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	quote := FeeQuote{Estimate: estimateBackend(ctx, p.Chain, node, p.Target), Limits: feeLimits(p.Chain), Amount: p.Amount}
	if p.Fee == 0 && !quote.Estimate.Current(time.Now()) {
		quote.Error = quote.Estimate.Error
		return quote, nil
	}
	var script []byte
	if p.Kind == "funding" {
		script = make([]byte, 34)
	} else {
		a, err := btcutil.DecodeAddress(p.Destination, network.Params())
		if err != nil || !a.IsForNet(network.Params()) {
			return quote, errors.New("invalid destination for network")
		}
		script, err = txscript.PayToAddrScript(a)
		if err != nil {
			return quote, err
		}
		if len(p.Inputs) == 0 {
			return quote, errors.New("select coins before estimating the send")
		}
	}
	wanted := map[string]bool{}
	for _, point := range p.Inputs {
		if wanted[pointKey(point)] {
			return quote, errors.New("duplicate selected coin")
		}
		wanted[pointKey(point)] = true
	}
	for _, coin := range coins {
		point := chain.OutpointKey(coin.TxID, coin.Vout)
		if len(p.Inputs) > 0 && !wanted[point] {
			continue
		}
		if coin.Confirmations < network.Confirmations() || reserved[point] {
			if wanted[point] {
				return quote, errors.New("selected coin is locked or unconfirmed")
			}
			continue
		}
		out, err := node.Output(ctx, coin.TxID, coin.Vout)
		if err != nil {
			return quote, err
		}
		if out == nil || out.Value != coin.Amount || out.Script.Hex != coin.Script || out.Confirmations < network.Confirmations() {
			return quote, errors.New("candidate coins changed; refresh wallet status and review again")
		}
		if _, err := hex.DecodeString(coin.Script); err != nil {
			return quote, err
		}
		quote.Total += int64(coin.Amount)
		if quote.Total > contract.MaxMoney {
			return quote, errors.New("input overflow")
		}
		quote.Inputs = append(quote.Inputs, CoinOutpoint{coin.TxID, coin.Vout})
		quote.VSize, _ = contract.PaymentVSize(len(quote.Inputs), script, changeScript)
		quote.Fee = p.Fee
		if p.Fee == 0 {
			quote.Fee, _ = contract.FeeForVSize(quote.Estimate.Rate, quote.VSize)
		}
		quote.Change = quote.Total - p.Amount - quote.Fee
		if p.Kind == "funding" && len(p.Inputs) == 0 && (quote.Change == 0 || quote.Change >= contract.Dust) {
			break
		}
		if len(quote.Inputs) == 50 {
			break
		}
	}
	if len(quote.Inputs) == 0 || (len(p.Inputs) > 0 && len(quote.Inputs) != len(p.Inputs)) {
		return quote, errors.New("selected inputs changed; refresh coins")
	}
	cap := quote.Limits.Send
	if p.Kind == "funding" {
		cap = quote.Limits.Funding
	}
	if quote.Fee < 1 || quote.Fee > cap {
		return quote, errors.New("quoted fee exceeds this chain's transaction limit; select a lower manual fee")
	}
	if quote.Change < 0 {
		return quote, errors.New("selected funds do not cover amount and fee")
	}
	if quote.Change > 0 && quote.Change < contract.Dust {
		return quote, errors.New("change would be dust; adjust amount or inputs")
	}
	if quote.Change == 0 {
		quote.VSize, _ = contract.PaymentVSize(len(quote.Inputs), script)
	}
	return quote, nil
}

func validateFeeRate(rate, timestamp, fee, vsize int64) error {
	if rate == 0 {
		return nil
	}
	f := chain.FeeEstimate{Rate: rate, Timestamp: timestamp, State: "available"}
	if !f.Current(time.Now()) {
		return errors.New("fee estimate is stale; review a fresh estimate or select a manual total fee")
	}
	minimum, err := contract.FeeForVSize(rate, vsize)
	if err != nil {
		return err
	}
	if fee < minimum {
		return errors.New("input/output size changed; review the fee again")
	}
	return nil
}

func (e *Engine) fundingFee(owner string) int64 {
	if p, ok := e.s.FundingFees[owner]; ok {
		return p.FundingFee
	}
	return protocol.FundingFee // Immutable legacy default, including old obligations.
}

func (e *Engine) selectFundingFee(raw json.RawMessage, owner string, id chain.ID) error {
	var p FeeSelection
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.OwnerFeeCap != 0 && p.OwnerFeeCap != feeLimits(id).Owner {
		return errors.New("owner fee cap must be zero (base only) or 20000 sats")
	}
	if p.FundingFee == 0 {
		p.FundingFee = protocol.FundingFee
	}
	if p.FundingFee < 1 || p.FundingFee > feeLimits(id).Funding {
		return errors.New("funding fee exceeds this chain's limit")
	}
	if p.Rate != 0 {
		if err := validateFeeRate(p.Rate, p.Timestamp, p.FundingFee, 1); err != nil {
			return err
		}
	}
	if e.s.FundingFees == nil {
		e.s.FundingFees = map[string]FeeSelection{}
	}
	e.s.FundingFees[owner] = p
	return nil
}

func (e *Engine) validateFundingReview(owner string, id chain.ID) error {
	p := e.s.FundingFees[owner]
	if p.Rate == 0 {
		return nil
	}
	vsize, err := contract.PaymentVSize(len(e.s.CoinReservations[owner].Inputs), make([]byte, 34), e.scripts[id])
	if err != nil {
		return err
	}
	return validateFeeRate(p.Rate, p.Timestamp, p.FundingFee, vsize)
}
