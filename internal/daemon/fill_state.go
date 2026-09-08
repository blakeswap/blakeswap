package daemon

import (
	"errors"
	"maps"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// ParentOrder retains immutable economics and monetary authorization separately
// from the changing signed public availability. Child history is indexed by
// FillRecords; this aggregate never embeds a lifetime array of child IDs.
type ParentOrder struct {
	Offer          protocol.Offer          `json:"offer"`
	Economics      string                  `json:"economics"`
	Quantities     QuantityLedger          `json:"quantities"`
	Fees           map[chain.ID]FillBudget `json:"fees"`
	Bounties       map[chain.ID]FillBudget `json:"bounties"`
	FundingPolicy  FeeSelection            `json:"funding_policy"`
	RestoreHold    bool                    `json:"restore_hold"`
	SignedRevision uint64                  `json:"signed_revision"`
	LastSignedAt   int64                   `json:"last_signed_at"`
}

// FillRecord is maker-owned allocation, not a guessed projection of a taker's
// remote parent. Every admitted child retains its request identity after return
// or settlement. Consumed fee authorization is never credited by a reorg/refund.
type FillRecord struct {
	ID                string             `json:"id"`
	ParentID          string             `json:"parent_id"`
	ParentMaker       string             `json:"parent_maker"`
	ParentRevision    uint64             `json:"parent_revision"`
	RequestDigest     string             `json:"request_digest"`
	Allocation        FillAllocation     `json:"allocation"`
	BuyAmount         int64              `json:"buy_amount"`
	Fees              map[chain.ID]int64 `json:"fees"`
	Bounties          map[chain.ID]int64 `json:"bounties"`
	FundingPolicy     FeeSelection       `json:"funding_policy"`
	Inputs            []CoinOutpoint     `json:"inputs"`
	ImportedUncertain bool               `json:"imported_uncertain"`
	FundingDisabled   bool               `json:"funding_disabled"`
}

func validateFillLimits(limits map[chain.ID]int64) error {
	if len(limits) != 2 {
		return errors.New("explicit btc and blake authorization limits required")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		limit, ok := limits[id]
		if !ok || limit < 0 {
			return errors.New("invalid per-asset fill authorization limit")
		}
	}
	return nil
}

func newParentOrder(offer protocol.Offer, policy FeeSelection, authorization FillOrderFields, now int64) (*ParentOrder, error) {
	if err := offer.Validate(now); err != nil {
		return nil, err
	}
	if offer.FillPolicy != authorization.FillPolicy || offer.Revision != 1 || offer.Available != offer.SellAmount || offer.Status != "open" {
		return nil, errors.New("new parent must explicitly authorize its full initial quantity")
	}
	if err := validateFillLimits(authorization.FeeBudgets); err != nil {
		return nil, err
	}
	if err := validateFillLimits(authorization.BountyBudgets); err != nil {
		return nil, err
	}
	if policy.FundingFee <= 0 || policy.FundingFee > feeLimits(offer.Sell).Funding || (policy.OwnerFeeCap != 0 && policy.OwnerFeeCap != 20000) {
		return nil, errors.New("invalid child funding or settlement policy")
	}
	minimum, _, err := offer.FillPolicy.Interval(offer.SellAmount, offer.BuyAmount)
	if err != nil {
		return nil, err
	}
	minimumBuy, err := protocol.RoundedBuy(offer.SellAmount, offer.BuyAmount, minimum)
	if err != nil {
		return nil, err
	}
	if err := protocol.ValidateRescueAmounts(offer.TowerBPS, minimum, minimumBuy); err != nil {
		return nil, errors.New("minimum fill is uneconomic for this private protection; review larger bounds")
	}
	ledger, err := newQuantityLedger(offer.SellAmount)
	if err != nil {
		return nil, err
	}
	parent := &ParentOrder{Offer: offer, Economics: offer.EconomicsDigest(), Quantities: ledger, FundingPolicy: policy, Fees: map[chain.ID]FillBudget{}, Bounties: map[chain.ID]FillBudget{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		parent.Fees[id] = FillBudget{Limit: authorization.FeeBudgets[id]}
		parent.Bounties[id] = FillBudget{Limit: authorization.BountyBudgets[id]}
	}
	return parent, nil
}

func (p ParentOrder) clone() ParentOrder {
	p.Fees = maps.Clone(p.Fees)
	p.Bounties = maps.Clone(p.Bounties)
	return p
}

func (p ParentOrder) fundingReserve(available int64) (int64, error) {
	minimum, _, err := p.Offer.FillPolicy.Interval(p.Offer.SellAmount, p.Offer.BuyAmount)
	if err != nil {
		return 0, err
	}
	if available < 0 || available > p.Quantities.Total {
		return 0, errors.New("invalid available funding quantity")
	}
	return protocol.CheckedBudget(p.FundingPolicy.FundingFee, available/minimum)
}

// reserveFill is a preparation step. Its caller checks exact signed event,
// current chain/protection proofs and new child identity, assigns actual disjoint
// inputs, then saves parent/child/terms/input and acceptance together before ACK.
func (p ParentOrder) reserveFill(request protocol.Request) (ParentOrder, *FillRecord, error) {
	if p.RestoreHold || p.Quantities.Closed || request.Revision != p.Quantities.Revision {
		return p, nil, errors.New("parent is held, closed or changed; refresh the reviewed revision")
	}
	offer, err := request.Validate(int64(request.OfferEvent.CreatedAt))
	if err != nil {
		return p, nil, err
	}
	if offer.Maker != p.Offer.Maker || offer.ID != p.Offer.ID || offer.EconomicsDigest() != p.Economics || offer.Available != p.Quantities.Available {
		return p, nil, errors.New("request does not match parent economics and available quantity")
	}
	amounts, err := p.Offer.FillPolicy.Quote(p.Offer.SellAmount, p.Offer.BuyAmount, p.Quantities.Available, request.Quantity)
	if err != nil {
		return p, nil, err
	}
	if err := protocol.ValidateRescueAmounts(p.Offer.TowerBPS, amounts.Sell, amounts.Buy); err != nil {
		return p, nil, err
	}
	ledger, allocation, err := p.Quantities.reserveAllocation(amounts.Sell)
	if err != nil {
		return p, nil, err
	}
	ownerMaximum := int64(2000)
	if p.FundingPolicy.OwnerFeeCap > 0 {
		ownerMaximum = p.FundingPolicy.OwnerFeeCap
	}
	if p.Offer.TowerBPS > 0 {
		ownerMaximum = max(ownerMaximum, protocol.RescueFees[len(protocol.RescueFees)-1])
	}
	// Own funding plus one own refund, and one incoming claim, are per-asset
	// authorization bounds. Refund and claim variants replace rather than add.
	// Existing authorized refund ladders include all rescue fee tiers, even
	// when automatic owner claim escalation is disabled. Reserve their ceiling.
	refundMaximum := max(ownerMaximum, protocol.RescueFees[len(protocol.RescueFees)-1])
	fees := map[chain.ID]int64{p.Offer.Sell: p.FundingPolicy.FundingFee + refundMaximum, p.Offer.Sell.Other(): ownerMaximum}
	bounties := map[chain.ID]int64{p.Offer.Sell: protocol.Bounty(amounts.Sell, p.Offer.TowerBPS), p.Offer.Sell.Other(): protocol.Bounty(amounts.Buy, p.Offer.TowerBPS)}
	next := p.clone()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		fee, err := next.Fees[id].reserve(fees[id])
		if err != nil {
			return p, nil, err
		}
		bounty, err := next.Bounties[id].reserve(bounties[id])
		if err != nil {
			return p, nil, err
		}
		next.Fees[id], next.Bounties[id] = fee, bounty
	}
	next.Quantities = ledger
	child := &FillRecord{ID: request.ID, ParentID: offer.ID, ParentMaker: offer.Maker, ParentRevision: request.Revision, RequestDigest: protocol.Digest(request), Allocation: allocation, BuyAmount: amounts.Buy, Fees: fees, Bounties: bounties, FundingPolicy: p.FundingPolicy}
	return next, child, nil
}

// transitionFill does not infer chain proof. The caller supplies a positively
// established transition and persists its result with the exact signed/evidence
// change. Never-funded returns additionally require an irreversible local
// funding refusal; imported uncertainty is not proof that nothing was signed.
func (p ParentOrder) transitionFill(child FillRecord, to FillDisposition, disableFunding bool) (ParentOrder, FillRecord, error) {
	if child.ParentID != p.Offer.ID || child.ParentMaker != p.Offer.Maker {
		return p, child, errors.New("fill belongs to another parent")
	}
	if child.Allocation.Disposition == to {
		return p, child, nil
	}
	returning := child.Allocation.Disposition == FillReserved && (to == FillRetired || to == FillReleased)
	if returning && (!disableFunding || child.ImportedUncertain || child.Allocation.EverCommitted) {
		return p, child, errors.New("unfunded return lacks irreversible local funding refusal")
	}
	ledger, allocation, err := p.Quantities.transitionAllocation(child.Allocation, to)
	if err != nil {
		return p, child, err
	}
	next := p.clone()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		fee, bounty := next.Fees[id], next.Bounties[id]
		if returning {
			fee, err = fee.returnReserved(child.Fees[id])
			if err == nil {
				bounty, err = bounty.returnReserved(child.Bounties[id])
			}
		} else if !child.Allocation.EverCommitted && allocation.EverCommitted {
			fee, err = fee.consume(child.Fees[id])
			if err == nil {
				bounty, err = bounty.consume(child.Bounties[id])
			}
		}
		if err != nil {
			return p, child, err
		}
		next.Fees[id], next.Bounties[id] = fee, bounty
	}
	next.Quantities, child.Allocation = ledger, allocation
	child.FundingDisabled = child.FundingDisabled || returning
	return next, child, nil
}

func validateParentOrder(id string, p *ParentOrder) error {
	if p == nil || id != p.Offer.ID || !protocol.Hex32(id) || p.Economics != p.Offer.EconomicsDigest() || p.Quantities.Total != p.Offer.SellAmount || p.SignedRevision > p.Quantities.Revision || p.LastSignedAt < 0 {
		return errors.New("invalid retained parent identity or revision")
	}
	if err := p.Offer.Validate(p.Offer.Expires - 1); err != nil {
		return err
	}
	if err := p.Quantities.validate(); err != nil {
		return err
	}
	if len(p.Fees) != 2 || len(p.Bounties) != 2 {
		return errors.New("retained parent lacks per-asset authorization")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		fee, feeOK := p.Fees[id]
		bounty, bountyOK := p.Bounties[id]
		if !feeOK || !bountyOK {
			return errors.New("retained parent has invalid authorization assets")
		}
		if err := fee.validate(); err != nil {
			return err
		}
		if err := bounty.validate(); err != nil {
			return err
		}
	}
	if p.FundingPolicy.FundingFee < 1 || p.FundingPolicy.FundingFee > feeLimits(p.Offer.Sell).Funding || (p.FundingPolicy.OwnerFeeCap != 0 && p.FundingPolicy.OwnerFeeCap != 20000) {
		return errors.New("retained parent has invalid fee policy")
	}
	return nil
}

func validateFillRecord(id string, f *FillRecord) error {
	if f == nil || f.ID != id || !protocol.Hex32(id) || !protocol.Hex32(f.ParentID) || !protocol.Hex32(f.ParentMaker) || !protocol.Hex32(f.RequestDigest) || f.ParentRevision == 0 || f.BuyAmount < protocol.MinPrincipal || f.BuyAmount > protocol.MaxPrincipal {
		return errors.New("invalid retained child allocation identity")
	}
	if err := f.Allocation.validate(); err != nil {
		return err
	}
	if err := validateFillLimits(f.Fees); err != nil {
		return err
	}
	if err := validateFillLimits(f.Bounties); err != nil {
		return err
	}
	if f.Allocation.Disposition == FillRetired && !f.FundingDisabled {
		return errors.New("retired child lacks irreversible funding refusal")
	}
	if f.FundingPolicy.FundingFee < 1 || f.FundingPolicy.FundingFee > 100000 || (f.FundingPolicy.OwnerFeeCap != 0 && f.FundingPolicy.OwnerFeeCap != 20000) {
		return errors.New("retained child has invalid fee policy")
	}
	if len(f.Inputs) == 0 || len(f.Inputs) > 50 {
		return errors.New("retained child lacks bounded actual input ownership")
	}
	seen := map[string]bool{}
	for _, input := range f.Inputs {
		if !protocol.Hex32(input.TxID) || seen[pointKey(input)] {
			return errors.New("invalid or duplicate child input")
		}
		seen[pointKey(input)] = true
	}
	return nil
}
