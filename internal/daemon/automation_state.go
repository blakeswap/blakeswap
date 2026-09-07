package daemon

import (
	"errors"

	"github.com/blakeswap/blakeswap/internal/contract"
)

// ValidateAutomationState checks external durable records before import or
// daemon initialization. Invalid records are rejected rather than discarded:
// silently repairing accounting could erase an obligation or spending charge.
func ValidateAutomationState(s *State) error {
	if s == nil {
		return errors.New("invalid automation state")
	}
	for id, p := range s.Automations {
		if p == nil || id == "" || p.Config.ID != id || p.Charges == nil {
			return errors.New("invalid automation policy record")
		}
		var volume, btc, blake int64
		for id, c := range p.Charges {
			if c == nil || id == "" || c.OfferID != id {
				return errors.New("invalid automation charge record")
			}
			if c.State != "reserved" && c.State != "committed" && c.State != "released" {
				return errors.New("invalid automation charge state")
			}
			if c.Volume <= 0 || c.Volume > contract.MaxMoney || c.BTCFees < 0 || c.BTCFees > contract.MaxMoney || c.BlakeFees < 0 || c.BlakeFees > contract.MaxMoney {
				return errors.New("invalid automation charge amounts")
			}
			if c.State == "released" {
				continue
			}
			if c.Volume > contract.MaxMoney-volume || c.BTCFees > contract.MaxMoney-btc || c.BlakeFees > contract.MaxMoney-blake {
				return errors.New("automation charges exceed safe accounting bounds")
			}
			volume += c.Volume
			btc += c.BTCFees
			blake += c.BlakeFees
		}
	}
	return validateStrategyState(s)
}
