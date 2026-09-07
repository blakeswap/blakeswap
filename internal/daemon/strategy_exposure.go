package daemon

import (
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// An audit fact, never a budget refund. Archive monitoring must hold new trading
// after a checkpoint contradiction before a missing live swap may use this fact.
// Import preserves the identity but holds its usability until fresh recovery.
type StrategyExposureProof struct {
	SwapID          string   `json:"swap_id"`
	Sell            chain.ID `json:"sell"`
	FundingTxID     string   `json:"funding_txid"`
	PeerFundingTxID string   `json:"peer_funding_txid"`
	SpendTxID       string   `json:"spend_txid"`
	PeerSpendTxID   string   `json:"peer_spend_txid"`
	CheckedAt       int64    `json:"checked_at"`
	Held            bool     `json:"held"`
}

// Called only after the ordinary settlement scanner has positively verified
// both exact spend transactions. A refreshed balance or an old cached stage is
// not equivalent to this observation.
func (e *Engine) recordStrategyExposure(s *Swap) {
	if e.strategyVerifiedSwaps == nil {
		e.strategyVerifiedSwaps = map[string]bool{}
	}
	e.strategyVerifiedSwaps[s.ID] = true
	if s.Role != "maker" {
		return
	}
	o, err := historicalOffer(s.Request.OfferEvent)
	if err != nil || o.Maker != e.identity.Public().Hex() {
		return
	}
	for _, p := range e.s.Automations {
		c := p.Charges[o.ID]
		if c == nil {
			continue
		}
		proof := StrategyExposureProof{SwapID: s.ID, Sell: o.Sell, FundingTxID: s.Short.TxID, PeerFundingTxID: s.Long.TxID, SpendTxID: s.ShortSpend, PeerSpendTxID: s.LongSpend}
		if old := c.ExposureSettled; old != nil {
			copy := *old
			copy.CheckedAt = 0
			if copy == proof {
				return
			}
		}
		proof.CheckedAt = time.Now().Unix()
		c.ExposureSettled = &proof
	}
}
func (e *Engine) reconcileStrategyExposure() {
	for _, p := range e.s.Automations {
		for _, c := range p.Charges {
			if proof := c.ExposureSettled; proof != nil {
				if s := e.s.Swaps[proof.SwapID]; s != nil && (!e.strategyVerifiedSwaps[s.ID] || !strategySettled(s, e.Config.Network.Confirmations())) {
					proof.Held = true
				}
			}
		}
	}
}
func validateStrategyExposure(c *AutomationCharge, sell chain.ID) error {
	if p := c.ExposureSettled; p != nil {
		if p.Sell != sell || !protocol.Hex32(p.SwapID) || !protocol.Hex32(p.FundingTxID) || !protocol.Hex32(p.PeerFundingTxID) || !protocol.Hex32(p.SpendTxID) || !protocol.Hex32(p.PeerSpendTxID) || p.CheckedAt <= 0 {
			return errors.New("invalid strategy exposure settlement proof")
		}
	}
	return nil
}
