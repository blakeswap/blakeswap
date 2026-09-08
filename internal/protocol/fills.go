package protocol

import (
	"errors"
	"math/big"
)

const (
	FillWhole          = "whole"
	FillPartial        = "partial"
	MinPrincipal int64 = 100000
	MaxPrincipal int64 = 10000000000
)

// FillPolicy is signed parent economics. These bounds concern contract
// quantities; current UTXO selection, fees and private protection are separate
// admission checks and are not assumed to form an interval.
type FillPolicy struct {
	Mode string `json:"fill_mode"`
	Min  int64  `json:"min_fill"`
	Max  int64  `json:"max_fill"`
}

type FillAmounts struct {
	Sell      int64 `json:"sell"`
	Buy       int64 `json:"buy"`
	Remaining int64 `json:"remaining"`
}

// RoundedBuy preserves the maker's minimum B/A rate independently for each
// child. Products can exceed int64 even for supported parent quantities.
func RoundedBuy(total, buy, quantity int64) (int64, error) {
	if total < MinPrincipal || total > MaxPrincipal || buy < MinPrincipal || buy > MaxPrincipal || quantity <= 0 || quantity > total {
		return 0, errors.New("invalid parent or fill quantity")
	}
	n := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(buy))
	n.Add(n, big.NewInt(total-1))
	n.Quo(n, big.NewInt(total))
	if !n.IsInt64() || n.Sign() <= 0 {
		return 0, errors.New("fill price overflows supported amount")
	}
	return n.Int64(), nil
}

// Interval derives the contiguous quantity interval from signed bounds and
// monotone ceil(q*B/A) principal bounds. It deliberately says nothing about
// whether the wallet currently has enough independently spendable inputs.
func (p FillPolicy) Interval(total, buy int64) (int64, int64, error) {
	if _, err := RoundedBuy(total, buy, total); err != nil {
		return 0, 0, err
	}
	if p.Mode != FillWhole && p.Mode != FillPartial {
		return 0, 0, errors.New("explicit whole or partial fill mode required")
	}
	if p.Min < MinPrincipal || p.Min > p.Max || p.Max > total || (p.Mode == FillWhole && (p.Min != total || p.Max != total)) {
		return 0, 0, errors.New("invalid signed fill bounds")
	}
	// ceil(q*B/A) >= L iff q*B > (L-1)*A. The strict inequality
	// requires floor((L-1)*A/B)+1, not ceil(L*A/B).
	minimum := new(big.Int).Mul(big.NewInt(MinPrincipal-1), big.NewInt(total))
	minimum.Quo(minimum, big.NewInt(buy)).Add(minimum, big.NewInt(1))
	maximum := new(big.Int).Mul(big.NewInt(MaxPrincipal), big.NewInt(total))
	maximum.Quo(maximum, big.NewInt(buy))
	m, max := p.Min, p.Max
	if minimum.Cmp(big.NewInt(m)) > 0 {
		if !minimum.IsInt64() {
			return 0, 0, errors.New("minimum fill exceeds supported amount")
		}
		m = minimum.Int64()
	}
	if maximum.Cmp(big.NewInt(max)) < 0 {
		max = maximum.Int64() // bounded above by the already validated p.Max.
	}
	if m > max || !Partitionable(total, m, max) {
		return 0, 0, errors.New("order quantity cannot be partitioned into valid fills")
	}
	return m, max, nil
}

// Partitionable is exact for the quantity interval [minimum, maximum]. It
// uses quotient/remainder to avoid overflow from rounding-up addition.
func Partitionable(amount, minimum, maximum int64) bool {
	if amount < 0 || minimum <= 0 || maximum < minimum {
		return false
	}
	if amount == 0 {
		return true
	}
	least := amount / maximum
	if amount%maximum != 0 {
		least++
	}
	return least <= amount/minimum
}

func (p FillPolicy) Quote(total, buy, available, quantity int64) (FillAmounts, error) {
	m, max, err := p.Interval(total, buy)
	if err != nil {
		return FillAmounts{}, err
	}
	if available < 0 || available > total || quantity < m || quantity > max || quantity > available || !Partitionable(available-quantity, m, max) {
		return FillAmounts{}, errors.New("fill exceeds available bounds or leaves an invalid tail")
	}
	b, err := RoundedBuy(total, buy, quantity)
	if err != nil || b < MinPrincipal || b > MaxPrincipal {
		return FillAmounts{}, errors.New("rounded fill is outside per-leg principal bounds")
	}
	return FillAmounts{Sell: quantity, Buy: b, Remaining: available - quantity}, nil
}

// CheckedBudget applies a per-fill amount to a bounded count without allowing
// aggregate fee/bounty authorization to wrap. Both operands are nonnegative.
func CheckedBudget(perFill, count int64) (int64, error) {
	if perFill < 0 || count < 0 {
		return 0, errors.New("negative fill budget")
	}
	n := new(big.Int).Mul(big.NewInt(perFill), big.NewInt(count))
	if !n.IsInt64() {
		return 0, errors.New("aggregate fill budget overflow")
	}
	return n.Int64(), nil
}
