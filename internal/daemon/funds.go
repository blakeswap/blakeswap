package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/wire"
)

type CoinHold struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Reason      string `json:"reason"`
	Cancellable bool   `json:"cancellable"`
}
type ChainBalance struct {
	TotalConfirmed    int64 `json:"total_confirmed"`
	UnlockedConfirmed int64 `json:"unlocked_confirmed"`
	ReservedConfirmed int64 `json:"reserved_confirmed"`
	Unconfirmed       int64 `json:"unconfirmed"`
	HTLCLocked        int64 `json:"htlc_locked"`
	HTLCAvailable     bool  `json:"htlc_available"`
}

func (e *Engine) coinHolds(id chain.ID) map[string][]CoinHold {
	holds := map[string][]CoinHold{}
	add := func(point string, hold CoinHold) {
		for _, old := range holds[point] {
			if old.Kind == hold.Kind && old.ID == hold.ID {
				return
			}
		}
		holds[point] = append(holds[point], hold)
	}
	for owner, reservation := range e.s.CoinReservations {
		if reservation.Chain != id {
			continue
		}
		kind, activity, _ := strings.Cut(owner, "/")
		hold := CoinHold{Kind: kind, ID: activity, Reason: "Pending swap request or active swap"}
		if kind == "offer" {
			hold.Reason = "Open or reserved order"
			var offer protocol.Offer
			if event, ok := e.s.Offers[activity]; ok && json.Unmarshal([]byte(event.Content), &offer) == nil {
				hold.Cancellable = offer.Status == "open"
				if offer.Status == "reserved" {
					hold.Reason = "Order committed to swap " + offer.Reservation
				}
			}
		}
		for _, point := range reservation.Inputs {
			add(pointKey(point), hold)
		}
	}
	rawHolds := func(raw string, hold CoinHold) {
		tx, err := contract.Parse(raw)
		if err != nil {
			return
		}
		for _, in := range tx.TxIn {
			add(chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index), hold)
		}
	}
	for _, swap := range e.s.Swaps {
		if swap.Role == "maker" && swap.Short.Chain == id {
			rawHolds(swap.ShortFunding, CoinHold{Kind: "swap", ID: swap.ID, Reason: "Signed swap funding; retained until its inputs are spent"})
		}
		if swap.Role == "taker" && swap.Long.Chain == id {
			rawHolds(swap.LongFunding, CoinHold{Kind: "swap", ID: swap.ID, Reason: "Signed swap funding; retained until its inputs are spent"})
		}
	}
	for _, send := range e.s.Sends {
		if send.Chain == id {
			rawHolds(send.Raw, CoinHold{Kind: "send", ID: send.ID, Reason: "Signed outgoing send; retained until its inputs are spent"})
		}
	}
	for point := range holds {
		sort.Slice(holds[point], func(i, j int) bool {
			a, b := holds[point][i], holds[point][j]
			return a.Kind+"/"+a.ID < b.Kind+"/"+b.ID
		})
	}
	return holds
}

func (e *Engine) chainBalances(coins []PublicCoin) map[chain.ID]ChainBalance {
	result := map[chain.ID]ChainBalance{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		result[id] = ChainBalance{HTLCLocked: e.htlcBalances[id], HTLCAvailable: e.htlcAvailable[id]}
	}
	seen := map[string]bool{}
	for _, coin := range coins {
		point := string(coin.Chain) + "/" + chain.OutpointKey(coin.TxID, coin.Vout)
		if seen[point] {
			continue
		}
		seen[point] = true
		b := result[coin.Chain]
		if coin.Confirmations < e.Config.Network.Confirmations() {
			b.Unconfirmed += coin.Amount
		} else {
			b.TotalConfirmed += coin.Amount
			if coin.Reserved {
				b.ReservedConfirmed += coin.Amount
			} else {
				b.UnlockedConfirmed += coin.Amount
			}
		}
		result[coin.Chain] = b
	}
	return result
}

// Contract principal is never inferred from a signed transaction or a missing
// lookup. This small observation budget cannot delay the settlement loop for an
// unbounded history; unavailable observations are explicitly marked unknown.
func (e *Engine) refreshHTLCBalance(ctx context.Context, id chain.ID) {
	if e.htlcBalances == nil {
		e.htlcBalances = map[chain.ID]int64{}
		e.htlcAvailable = map[chain.ID]bool{}
	}
	e.htlcAvailable[id] = false
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	var total int64
	seen := map[string]bool{}
	for _, swap := range e.s.Swaps {
		c := swap.Short
		if swap.Role == "taker" {
			c = swap.Long
		} else if swap.Role != "maker" {
			continue
		}
		if c.Chain != id || c.TxID == "" {
			continue
		}
		point := chain.OutpointKey(c.TxID, c.Vout)
		if seen[point] {
			continue
		}
		seen[point] = true
		out, err := e.nodes[id].Output(ctx, c.TxID, c.Vout)
		if err != nil {
			return
		}
		if out != nil {
			total += int64(out.Value)
		}
	}
	e.htlcBalances[id] = total
	e.htlcAvailable[id] = true
}

type FundsPreflightRequest struct {
	Chain  chain.ID       `json:"chain"`
	Amount int64          `json:"amount"`
	Fee    int64          `json:"fee"`
	Inputs []CoinOutpoint `json:"inputs"`
}
type FundsPreflight struct {
	Network    chain.Network  `json:"network"`
	Wallet     string         `json:"wallet"`
	Inputs     []CoinOutpoint `json:"inputs"`
	State      string         `json:"state"`
	Message    string         `json:"message"`
	Sufficient bool           `json:"sufficient"`
	Total      int64          `json:"total"`
}

// Preflight is advisory: it never signs, reserves, or authorizes spending. Proof
// results are not cached. Each call rereads the candidate outputs and ancestry,
// so a reorg or backend recovery is reevaluated even for identical outpoints.
// Network IO runs without the engine lock and only one proof per wallet runs at
// once. Signing retains its independent conservative ProveBTCExclusive gate.
func (e *Engine) preflightFunds(ctx context.Context, req Request) (FundsPreflight, error) {
	var p FundsPreflightRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return FundsPreflight{}, err
	}
	if err := ctx.Err(); err != nil {
		return FundsPreflight{}, err
	}
	e.mu.Lock()
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return FundsPreflight{}, err
	}
	if err := CheckCommandNetwork(req, e.Config.Network, false); err != nil {
		e.mu.Unlock()
		return FundsPreflight{}, err
	}
	result := FundsPreflight{Network: e.Config.Network, Wallet: e.Config.Name, Inputs: []CoinOutpoint{}, State: "unavailable"}
	if !p.Chain.Valid() || p.Amount < contract.Dust || p.Amount > contract.MaxMoney || p.Fee < 1 || p.Fee > 1000000 || len(p.Inputs) > 50 {
		e.mu.Unlock()
		return result, errors.New("preflight requires a chain, amount, fee and at most 50 inputs")
	}
	coins := e.knownCoins(p.Chain)
	reserved := e.reservedCoins(p.Chain, "")
	network, btc, blake, node := e.Config.Network, e.nodes[chain.BTC], e.nodes[chain.Blake], e.nodes[p.Chain]
	e.mu.Unlock()
	if !e.preflightBusy.CompareAndSwap(false, true) {
		result.State = "checking"
		result.Message = "A wallet funds check is already running; retry shortly."
		return result, nil
	}
	defer e.preflightBusy.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	selected := []chain.UTXO{}
	if len(p.Inputs) > 0 {
		known := map[string]chain.UTXO{}
		for _, coin := range coins {
			known[chain.OutpointKey(coin.TxID, coin.Vout)] = coin
		}
		seen := map[string]bool{}
		for _, point := range p.Inputs {
			key := pointKey(point)
			coin, ok := known[key]
			if !ok || seen[key] || reserved[key] || coin.Confirmations < network.Confirmations() {
				result.Message = "Selected coins are reserved, unconfirmed, duplicated, or no longer available. Refresh coin control."
				return result, nil
			}
			seen[key] = true
			selected = append(selected, coin)
		}
	} else {
		var total int64
		for _, coin := range coins {
			if reserved[chain.OutpointKey(coin.TxID, coin.Vout)] || coin.Confirmations < network.Confirmations() {
				continue
			}
			selected = append(selected, coin)
			total += int64(coin.Amount)
			if total == p.Amount+p.Fee || total >= p.Amount+p.Fee+contract.Dust || len(selected) >= 50 {
				break
			}
		}
	}
	tx := wire.NewMsgTx(2)
	for _, coin := range selected {
		out, err := node.Output(ctx, coin.TxID, coin.Vout)
		if err != nil {
			result.Message = "Chain lookup unavailable. Reconnect the node or indexer and retry."
			return result, nil
		}
		if out == nil || out.Value != coin.Amount || out.Script.Hex != coin.Script || out.Confirmations < network.Confirmations() {
			result.Message = "Candidate coins changed or were spent. Refresh and retry."
			return result, nil
		}
		point, err := chain.WireOutpoint(coin.TxID, coin.Vout)
		if err != nil {
			return result, err
		}
		tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
		result.Inputs = append(result.Inputs, CoinOutpoint{coin.TxID, coin.Vout})
		result.Total += int64(coin.Amount)
	}
	change := result.Total - p.Amount - p.Fee
	result.Sufficient = len(selected) > 0 && (change == 0 || change >= contract.Dust)
	if !result.Sufficient {
		result.Message = "Insufficient unlocked confirmed coins including the fee and non-dust change. Cancel an open order or deposit funds and wait for confirmation."
		return result, nil
	}
	if p.Chain != chain.BTC {
		result.State = "not_applicable"
		result.Message = "Unlocked confirmed candidate inputs cover the amount and fee."
		return result, nil
	}
	err := chain.ProveBTCExclusive(ctx, network, btc, blake, tx)
	switch {
	case err == nil:
		result.State = "proven"
		result.Message = "This candidate input set passes BTC replay protection. Funds and ancestry are checked again before funding."
	case errors.Is(err, chain.ErrReplayUnsafe):
		result.State = "not_proven"
		result.Message = "BTC ancestry is not proven exclusive within the bounded verifier. Use independently split BTC descended from a post-fork BTC coinbase, or select a set containing a proven exclusive input."
	default:
		result.Message = "BTC ancestry check unavailable. Check both chain connections and retry; these coins have not been classified as unsafe."
	}
	return result, nil
}
