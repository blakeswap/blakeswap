package protocol

import (
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"time"
)

const TimeLockThreshold uint32 = 500000000
const LongSeconds uint32 = 4 * 24 * 3600
const ShortSeconds uint32 = 2 * 24 * 3600
const TakeoverSeconds uint32 = 24 * 3600
const RevealSeconds uint32 = 12 * 3600
const MaxClockSkew uint32 = 2 * 3600

func RefundDelay(n chain.Network) uint32 {
	if n.Normalized() == chain.Regtest {
		return RefundGrace
	}
	return 6 * 3600
}
func publicClocks(clocks map[chain.ID]uint32) error {
	a, b := clocks[chain.BTC], clocks[chain.Blake]
	if a < TimeLockThreshold || b < TimeLockThreshold {
		return errors.New("both chain median times are required")
	}
	if a > b {
		a, b = b, a
	}
	if b-a > MaxClockSkew {
		return errors.New("chain clocks differ by more than two hours; new funding and revelation stopped")
	}
	return nil
}
func (t Terms) timeGate(phase string, clocks map[chain.ID]uint32) error {
	return t.timeGateAt(phase, clocks, time.Now().Unix())
}
func (t Terms) timeGateAt(phase string, clocks map[chain.ID]uint32, now int64) error {
	if err := publicClocks(clocks); err != nil {
		return err
	}
	for _, clock := range clocks {
		if int64(clock) < now-6*3600 || int64(clock) > now+2*3600 {
			return errors.New("chain median time is stale or too far in the future")
		}
	}
	l, s := clocks[t.Long.Chain], clocks[t.Short.Chain]
	start := t.Long.RefundHeight - LongSeconds
	if uint64(start) > uint64(max(l, s))+uint64(MaxClockSkew) {
		return errors.New("swap deadlines start too far in the future")
	}
	if l >= t.Long.RefundHeight || s >= t.Short.RefundHeight {
		return errors.New("refund deadline reached")
	}
	longLeft, shortLeft := t.Long.RefundHeight-l, t.Short.RefundHeight-s
	switch phase {
	case "fund-long":
		if longLeft < LongSeconds-2*3600 || shortLeft < ShortSeconds-2*3600 {
			return errors.New("acceptance too old to fund")
		}
	case "fund-short":
		if longLeft < 3*24*3600 || shortLeft < 24*3600 || uint64(l)+2*3600 > uint64(t.RevealBefore) {
			return errors.New("insufficient funding safety margin")
		}
	case "reveal":
		if longLeft < 2*24*3600 || shortLeft < 12*3600 || l >= t.RevealBefore {
			return errors.New("secret reveal cutoff reached; wait for refunds")
		}
	default:
		return errors.New("unknown safety gate")
	}
	return nil
}
