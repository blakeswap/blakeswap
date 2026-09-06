package daemon

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

type CoinOutpoint struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}
type CoinReservation struct {
	Chain  chain.ID       `json:"chain"`
	Inputs []CoinOutpoint `json:"inputs"`
}

func pointKey(p CoinOutpoint) string { return chain.OutpointKey(p.TxID, p.Vout) }
func terminalSwap(s *Swap) bool {
	switch s.Stage {
	case "completed", "refunded", "rejected", "expired before acceptance", "expired before funding", "expired before maker funding", "aborted; counterparty refunded":
		return true
	}
	return false
}

func (e *Engine) reservedCoins(id chain.ID, except string) map[string]bool {
	reserved := map[string]bool{}
	for owner, r := range e.s.CoinReservations {
		if r.Chain == id && owner != except {
			for _, p := range r.Inputs {
				reserved[pointKey(p)] = true
			}
		}
	}
	reserveRaw := func(raw string) {
		if raw == "" {
			return
		}
		tx, err := contract.Parse(raw)
		if err != nil {
			return
		}
		for _, in := range tx.TxIn {
			reserved[chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)] = true
		}
	}
	for _, s := range e.s.Swaps {
		if s.Role == "maker" && s.Short.Chain == id {
			reserveRaw(s.ShortFunding)
		}
		if s.Role == "taker" && s.Long.Chain == id {
			reserveRaw(s.LongFunding)
		}
	}
	for _, send := range e.s.Sends {
		if send.Chain == id {
			reserveRaw(send.Raw)
		}
	}
	return reserved
}

func (e *Engine) reserveCoins(owner string, id chain.ID, target int64) error {
	if e.s.CoinReservations == nil {
		e.s.CoinReservations = map[string]CoinReservation{}
	}
	reserved := e.reservedCoins(id, owner)
	var selected []CoinOutpoint
	var total int64
	// Retain prior choices so an open order's lock does not jump between coins.
	previous := map[string]bool{}
	for _, p := range e.s.CoinReservations[owner].Inputs {
		previous[pointKey(p)] = true
	}
	coins := e.knownCoins(id)
	sort.SliceStable(coins, func(i, j int) bool {
		return previous[chain.OutpointKey(coins[i].TxID, coins[i].Vout)] && !previous[chain.OutpointKey(coins[j].TxID, coins[j].Vout)]
	})
	for _, coin := range coins {
		if coin.Confirmations < e.Config.Network.Confirmations() || reserved[chain.OutpointKey(coin.TxID, coin.Vout)] {
			continue
		}
		selected = append(selected, CoinOutpoint{coin.TxID, coin.Vout})
		total += int64(coin.Amount)
		if total == target || total >= target+contract.Dust {
			e.s.CoinReservations[owner] = CoinReservation{id, selected}
			return nil
		}
		if len(selected) >= 50 {
			break
		}
	}
	// Existing underfunded orders keep their available coins locked, so deposits
	// cannot be withdrawn out from under an already-advertised obligation.
	e.s.CoinReservations[owner] = CoinReservation{id, selected}
	return errors.New("insufficient unlocked confirmed coins; cancel an open order to release its coins")
}

// Reconcile persisted reservations for legacy wallets and newly confirmed coins.
func (e *Engine) reconcileReservations() {
	type need struct {
		id     chain.ID
		amount int64
	}
	active := map[string]need{}
	for id, event := range e.s.Offers {
		var o protocol.Offer
		if json.Unmarshal([]byte(event.Content), &o) != nil {
			continue
		}
		if o.Status == "open" && o.Expires > time.Now().Unix() {
			active["offer/"+id] = need{o.Sell, o.SellAmount + protocol.FundingFee}
		}
		if o.Status == "reserved" {
			if s := e.s.Swaps[o.Reservation]; s != nil && !terminalSwap(s) && s.ShortFunding == "" {
				active["offer/"+id] = need{o.Sell, o.SellAmount + protocol.FundingFee}
			}
		}
	}
	for id, s := range e.s.Swaps {
		e.expirePendingRequest(s, time.Now().Unix())
		if s.Role != "taker" || terminalSwap(s) || s.LongFunding != "" {
			continue
		}
		var o protocol.Offer
		if json.Unmarshal([]byte(s.Request.OfferEvent.Content), &o) == nil {
			active["swap/"+id] = need{o.Sell.Other(), o.BuyAmount + protocol.FundingFee}
		}
	}
	for owner := range e.s.CoinReservations {
		if _, ok := active[owner]; !ok {
			delete(e.s.CoinReservations, owner)
		}
	}
	owners := make([]string, 0, len(active))
	for owner := range active {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		n := active[owner]
		_ = e.reserveCoins(owner, n.id, n.amount)
	}
}

// Before accepting terms the taker has signed no funding transaction. Expiring
// the request is safe only in that state, and late acceptance cannot revive it.
func (e *Engine) expirePendingRequest(s *Swap, now int64) bool {
	if s.Role != "taker" || s.Terms != nil || s.LongFunding != "" || s.ShortFunding != "" || terminalSwap(s) {
		return false
	}
	var offer protocol.Offer
	if json.Unmarshal([]byte(s.Request.OfferEvent.Content), &offer) != nil || offer.Expires <= 0 || offer.Expires > now {
		return false
	}
	s.Stage = "expired before acceptance"
	delete(e.s.CoinReservations, "swap/"+s.ID)
	raw, _ := json.Marshal(s.Request)
	deliveryID := protocol.Digest([]string{offer.Maker, "request", s.ID, string(raw)})
	delete(e.s.Outbox, deliveryID)
	return true
}
