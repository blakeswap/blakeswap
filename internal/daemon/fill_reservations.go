package daemon

import (
	"errors"
	"math"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// fillReservationCandidate transfers whole known-confirmed outpoints from one
// parent's unassigned pool. It never borrows a sibling's coin or treats the
// unconfirmed change of an earlier child as new independent funds. The caller
// commits both returned owners with the child's allocation before acceptance.
func (e *Engine) fillReservationCandidate(parentID string, id chain.ID, target int64) (CoinReservation, CoinReservation, error) {
	pool, ok := e.s.CoinReservations["offer/"+parentID]
	if !ok || pool.Chain != id || target <= 0 {
		return CoinReservation{}, pool, errors.New("parent has no matching unassigned input pool")
	}
	unique := map[string]bool{}
	for _, point := range pool.Inputs {
		key := pointKey(point)
		if !protocol.Hex32(point.TxID) || unique[key] {
			return CoinReservation{}, pool, errors.New("invalid or duplicate parent input ownership")
		}
		unique[key] = true
	}
	reserved := e.reservedCoins(id, "offer/"+parentID)
	known := map[string]chain.UTXO{}
	for _, coin := range e.knownCoins(id) {
		key := chain.OutpointKey(coin.TxID, coin.Vout)
		if _, duplicate := known[key]; duplicate {
			return CoinReservation{}, pool, errors.New("duplicate wallet outpoint observation")
		}
		known[key] = coin
	}
	child := CoinReservation{Chain: id}
	selected := map[string]bool{}
	var total int64
	for _, point := range pool.Inputs {
		key := pointKey(point)
		coin, exists := known[key]
		if selected[key] {
			return CoinReservation{}, pool, errors.New("duplicate parent input ownership")
		}
		if !exists || coin.Confirmations < e.Config.Network.Confirmations() || reserved[key] {
			continue
		}
		value := int64(coin.Amount)
		if value <= 0 || value > contract.MaxMoney || value > math.MaxInt64-total {
			return CoinReservation{}, pool, errors.New("invalid parent input value")
		}
		selected[key] = true
		child.Inputs = append(child.Inputs, point)
		total += value
		if total == target || (total > target && total-target >= contract.Dust) {
			remaining := CoinReservation{Chain: id}
			for _, original := range pool.Inputs {
				if !selected[pointKey(original)] {
					remaining.Inputs = append(remaining.Inputs, original)
				}
			}
			return child, remaining, nil
		}
		if len(child.Inputs) >= 50 {
			break
		}
	}
	return CoinReservation{}, pool, errors.New("no independent confirmed parent inputs for this fill; wait for confirmed change or deposit")
}

// assignedFundingCoins is exact ownership, not a preference for coin selection.
// Missing or changed assigned inputs never authorize substitution from another
// child's pool or an unrelated deposit after the acceptance was committed.
func (e *Engine) assignedFundingCoins(id chain.ID, owner string) ([]chain.UTXO, error) {
	coins := e.knownCoins(id)
	if owner == "" {
		return coins, nil
	}
	reservation, ok := e.s.CoinReservations[owner]
	if !ok || reservation.Chain != id || len(reservation.Inputs) == 0 || len(reservation.Inputs) > 50 {
		return nil, errors.New("funding has no valid exact input assignment")
	}
	known := map[string]chain.UTXO{}
	for _, coin := range coins {
		key := chain.OutpointKey(coin.TxID, coin.Vout)
		if _, duplicate := known[key]; duplicate {
			return nil, errors.New("duplicate wallet outpoint observation")
		}
		known[key] = coin
	}
	var assigned []chain.UTXO
	seen := map[string]bool{}
	for _, point := range reservation.Inputs {
		key := pointKey(point)
		coin, found := known[key]
		if !protocol.Hex32(point.TxID) || seen[key] || !found {
			return nil, errors.New("assigned funding input is missing or duplicated")
		}
		seen[key] = true
		assigned = append(assigned, coin)
	}
	return assigned, nil
}
