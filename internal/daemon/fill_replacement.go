package daemon

import (
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// planReplacement conserves the exact remaining inventory and each native
// asset's unassigned authorization. Existing child reservations and permanent
// consumption never move to the successor or become a fresh grant.
func (p ParentOrder) planReplacement(next *ParentOrder) (ParentOrder, error) {
	if err := validateParentOrder(p.Offer.ID, &p); err != nil {
		return p, err
	}
	if next == nil || p.RestoreHold || p.Quantities.Closed || p.Quantities.Available == 0 || p.SignedRevision != p.Quantities.Revision || next.Offer.Sell != p.Offer.Sell || next.Quantities.Total != p.Quantities.Available {
		return p, errors.New("replacement must use the exact available quantity and sell asset")
	}
	if err := validateParentOrder(next.Offer.ID, next); err != nil {
		return p, err
	}
	if next.Offer.ID == p.Offer.ID || next.RestoreHold || next.SignedRevision != 0 || next.Quantities.Revision != 1 || next.Quantities.Available != next.Quantities.Total {
		return p, errors.New("replacement requires a fresh initial parent")
	}
	out := p.clone()
	var err error
	out.Quantities, err = out.Quantities.withdrawAvailable()
	if err != nil {
		return p, err
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		out.Fees[id], err = out.Fees[id].transfer(next.Fees[id].Limit)
		if err != nil {
			return p, err
		}
		out.Bounties[id], err = out.Bounties[id].transfer(next.Bounties[id].Limit)
		if err != nil {
			return p, err
		}
	}
	return out, nil
}

// replacementCoins restricts every advisory and execution selection to the
// source's current unassigned outpoints. Missing or conflicting ownership must
// fail rather than allowing selection to borrow a child input or new deposit.
func (e *Engine) replacementCoins(owner string, id chain.ID) ([]chain.UTXO, error) {
	pool, ok := e.s.CoinReservations[owner]
	if !ok || pool.Chain != id || len(pool.Inputs) == 0 || len(pool.Inputs) > 50 {
		return nil, errors.New("replacement has no bounded unassigned input pool")
	}
	known := map[string]chain.UTXO{}
	for _, c := range e.knownCoins(id) {
		key := chain.OutpointKey(c.TxID, c.Vout)
		if _, exists := known[key]; exists {
			return nil, errors.New("duplicate replacement coin observation")
		}
		known[key] = c
	}
	reserved, seen := e.reservedCoins(id, owner), map[string]bool{}
	coins := make([]chain.UTXO, 0, len(pool.Inputs))
	for _, point := range pool.Inputs {
		key := pointKey(point)
		coin, exists := known[key]
		if !protocol.Hex32(point.TxID) || seen[key] || !exists || reserved[key] || coin.Confirmations < e.Config.Network.Confirmations() {
			return nil, errors.New("replacement input changed or belongs to an accepted child")
		}
		seen[key] = true
		coins = append(coins, coin)
	}
	return coins, nil
}
