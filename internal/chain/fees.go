package chain

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FeeEstimator is optional so a backend without an estimator remains usable
// with an explicitly selected total fee. Rates always belong to this chain.
type FeeEstimator interface {
	EstimateFee(context.Context, uint32) FeeEstimate
}

type FeeEstimate struct {
	RequestedTarget uint32 `json:"requested_target"`
	Chain           ID     `json:"chain"`
	Rate            int64  `json:"rate_sat_kvb"`
	Target          uint32 `json:"target"`
	Timestamp       int64  `json:"timestamp"`
	Source          string `json:"source"`
	State           string `json:"state"`
	Error           string `json:"error"`
}

const FeeEstimateLifetime = 120 * time.Second

func (f FeeEstimate) Current(now time.Time) bool {
	return f.State == "available" && f.Rate > 0 && f.Timestamp <= now.Unix() && now.Unix()-f.Timestamp <= int64(FeeEstimateLifetime/time.Second)
}

// Both estimatesmartfee and Electrum blockchain.estimatefee return native
// coins per 1000 virtual bytes. Parse decimals directly; never round via float.
var feeDecimal = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?([eE][+-]?[0-9]{1,2})?$`)

func feeRate(value json.Number) (int64, error) {
	if !feeDecimal.MatchString(value.String()) {
		return 0, errors.New("invalid fee estimate")
	}
	if parts := strings.FieldsFunc(value.String(), func(r rune) bool { return r == 'e' || r == 'E' }); len(parts) == 2 {
		exponent, err := strconv.Atoi(parts[1])
		if err != nil || exponent < -12 || exponent > 12 {
			return 0, errors.New("fee estimate exponent out of bounds")
		}
	}
	r, ok := new(big.Rat).SetString(value.String())
	if !ok || r.Sign() <= 0 {
		return 0, errors.New("fee estimation unavailable; select a manual fee")
	}
	r.Mul(r, big.NewRat(100000000, 1))
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > 1000000000 {
		return 0, errors.New("fee estimate exceeds supported rate")
	}
	return q.Int64(), nil
}

func estimate(id ID, source string, target uint32, call func(*json.Number) error) FeeEstimate {
	f := FeeEstimate{Chain: id, Source: source, Target: target, RequestedTarget: target, Timestamp: time.Now().Unix(), State: "unavailable"}
	if target < 1 || target > 1008 {
		f.Error = "target must be 1–1008 blocks"
		return f
	}
	var n json.Number
	err := call(&n)
	if err == nil {
		f.Rate, err = feeRate(n)
	}
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.State = "available"
	return f
}

func (r *RPC) EstimateFee(ctx context.Context, target uint32) FeeEstimate {
	var actual uint32
	f := estimate(r.ID, "rpc:estimatesmartfee", target, func(n *json.Number) error {
		var reply struct {
			Blocks  uint32      `json:"blocks"`
			FeeRate json.Number `json:"feerate"`
			Errors  []string    `json:"errors"`
		}
		if err := r.Call(ctx, "estimatesmartfee", &reply, target, "CONSERVATIVE"); err != nil {
			return err
		}
		if len(reply.Errors) > 0 {
			return errors.New(strings.Join(reply.Errors, "; "))
		}
		actual = reply.Blocks
		*n = reply.FeeRate
		return nil
	})
	if actual > 0 {
		f.Target = actual
	}
	return f
}

func (e *Electrum) EstimateFee(ctx context.Context, target uint32) FeeEstimate {
	return estimate(e.ID, "electrum:blockchain.estimatefee", target, func(n *json.Number) error {
		return e.Call(ctx, "blockchain.estimatefee", n, target)
	})
}
