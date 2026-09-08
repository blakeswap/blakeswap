package daemon

import "errors"

func (e *Engine) retainedFillRecord(id string) (*FillRecord, error) {
	child := e.s.FillRecords[id]
	if child == nil {
		var cold FillRecord
		found, err := e.archivedValue("fill_records", id, &cold)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("fill allocation evidence is unavailable")
		}
		child = &cold
	}
	if err := validateFillRecord(id, child); err != nil {
		return nil, err
	}
	return child, nil
}

// A current-format child's fee is its own authorization, never a parent fee
// inferred from identity or a legacy default. Read failures remain failures.
func (e *Engine) retainedSwapFee(s *Swap) (FeeSelection, error) {
	if s == nil {
		return FeeSelection{}, errors.New("child core is unavailable")
	}
	owner := "swap/" + s.ID
	selection, found := e.s.FundingFees[owner]
	if !found {
		var err error
		found, err = e.archivedValue("funding_fees", owner, &selection)
		if err != nil {
			return FeeSelection{}, err
		}
	}
	if !found || selection.FundingFee < 1 || selection.FundingFee > 100000 || (selection.OwnerFeeCap != 0 && selection.OwnerFeeCap != 20000) {
		return FeeSelection{}, errors.New("exact retained child funding fee is unavailable")
	}
	if s.Role == "maker" {
		child, err := e.retainedFillRecord(s.ID)
		if err != nil {
			return FeeSelection{}, err
		}
		if selection != child.FundingPolicy {
			return FeeSelection{}, errors.New("child fee companion conflicts with its saved authorization")
		}
	}
	return selection, nil
}

// retainedParentOrder is a point read from the current authenticated query or
// live vault boundary. It does not promote publication or spending authority.
func (e *Engine) retainedParentOrder(id string) (*ParentOrder, error) {
	p := e.s.ParentOrders[id]
	if p == nil {
		var cold ParentOrder
		found, err := e.archivedValue("parent_orders", id, &cold)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("retained parent accounting is unavailable")
		}
		p = &cold
	}
	if err := validateParentOrder(id, p); err != nil {
		return nil, err
	}
	if p.Offer.Network.Normalized() != e.Config.Network || p.Offer.Maker != e.identity.Public().Hex() {
		return nil, errors.New("retained parent belongs to another wallet or network")
	}
	return p, nil
}
