package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

type PublicCoin struct {
	Chain         chain.ID   `json:"chain"`
	TxID          string     `json:"txid"`
	Vout          uint32     `json:"vout"`
	Amount        int64      `json:"amount"`
	Address       string     `json:"address"`
	Confirmations int        `json:"confirmations"`
	Reserved      bool       `json:"reserved"`
	Holds         []CoinHold `json:"holds"`
}
type PublicSend struct {
	ID            string   `json:"id"`
	Chain         chain.ID `json:"chain"`
	TxID          string   `json:"txid"`
	Destination   string   `json:"destination"`
	Amount        int64    `json:"amount"`
	Fee           int64    `json:"fee"`
	Change        int64    `json:"change"`
	Confirmations int      `json:"confirmations"`
	Error         string   `json:"error"`
	Submitted     bool     `json:"submitted"`
}
type WalletSend struct {
	PublicSend
	Raw         string `json:"raw"`
	Digest      string `json:"digest"`
	LastAttempt int64  `json:"last_attempt"`
}
type SendRequest struct {
	ID              string         `json:"id"`
	Chain           chain.ID       `json:"chain"`
	Destination     string         `json:"destination"`
	Amount          int64          `json:"amount"`
	Fee             int64          `json:"fee"`
	Inputs          []CoinOutpoint `json:"inputs"`
	ExpectedNetwork string         `json:"expected_network"`
}

func (e *Engine) publicCoins() []PublicCoin {
	result := []PublicCoin{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		holds := e.coinHolds(id)
		for _, entry := range e.receiveBook[id] {
			for _, coin := range e.walletCoins[id][hex.EncodeToString(entry.script)] {
				owners := holds[chain.OutpointKey(coin.TxID, coin.Vout)]
				result = append(result, PublicCoin{Chain: id, TxID: coin.TxID, Vout: coin.Vout, Amount: int64(coin.Amount), Address: entry.address, Confirmations: coin.Confirmations, Reserved: len(owners) > 0, Holds: owners})
			}
		}
	}
	return result
}
func (e *Engine) sendCoins(ctx context.Context, raw json.RawMessage) (PublicSend, error) {
	var p SendRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return PublicSend{}, err
	}
	if !p.Chain.Valid() || len(p.ID) < 16 || len(p.ID) > 64 || strings.TrimSpace(p.ID) != p.ID || p.Amount < contract.Dust || p.Amount > contract.MaxMoney || p.Fee < 1 || p.Fee > 1000000 || len(p.Inputs) < 1 || len(p.Inputs) > 50 {
		return PublicSend{}, errors.New("send requires a request ID, chain, destination, at least 600 sats, a fee of 1–1,000,000 sats, and 1–50 selected coins")
	}
	digest := protocol.Digest(p)
	if previous := e.s.Sends[p.ID]; previous != nil {
		if previous.Digest != digest {
			return PublicSend{}, errors.New("send request ID already used with different details")
		}
		return previous.PublicSend, nil
	}
	if len(e.s.Sends) >= 1000 {
		return PublicSend{}, errors.New("send history capacity reached")
	}
	address, err := btcutil.DecodeAddress(p.Destination, e.Config.Network.Params())
	if err != nil || !address.IsForNet(e.Config.Network.Params()) {
		return PublicSend{}, errors.New("destination is not a valid address for the selected network")
	}
	destination, err := txscript.PayToAddrScript(address)
	if err != nil {
		return PublicSend{}, err
	}
	if err := e.refresh(ctx); err != nil {
		return PublicSend{}, err
	}
	reserved := e.reservedCoins(p.Chain, "")
	known := map[string]chain.UTXO{}
	for _, coin := range e.knownCoins(p.Chain) {
		known[chain.OutpointKey(coin.TxID, coin.Vout)] = coin
	}
	keys := map[string]*btcec.PrivateKey{}
	for _, entry := range e.receiveBook[p.Chain] {
		keys[hex.EncodeToString(entry.script)] = entry.key
	}
	var coins []chain.UTXO
	var total int64
	seen := map[string]bool{}
	for _, input := range p.Inputs {
		point := pointKey(input)
		if reserved[point] {
			return PublicSend{}, errors.New("selected coin is locked by an open order, trade, or pending send; cancel its open order before sending")
		}
		coin, ok := known[point]
		if !ok || seen[point] || coin.Confirmations < e.Config.Network.Confirmations() {
			return PublicSend{}, errors.New("selected coin is unavailable, unconfirmed, or duplicated; refresh coin control")
		}
		seen[point] = true
		out, err := e.nodes[p.Chain].Output(ctx, input.TxID, input.Vout)
		if err != nil {
			return PublicSend{}, err
		}
		if out == nil || out.Value != coin.Amount || out.Script.Hex != coin.Script || out.Confirmations < e.Config.Network.Confirmations() {
			return PublicSend{}, errors.New("selected coin changed or was spent; refresh coin control")
		}
		coins = append(coins, coin)
		total += int64(coin.Amount)
	}
	tx, err := contract.PayWithKeys(p.Chain, p.Amount, destination, coins, keys, e.scripts[p.Chain], p.Fee)
	if err != nil {
		return PublicSend{}, err
	}
	if p.Chain == chain.BTC {
		if err := chain.ProveBTCExclusive(ctx, e.Config.Network, e.nodes[chain.BTC], e.nodes[chain.Blake], tx); err != nil {
			return PublicSend{}, err
		}
	}
	send := &WalletSend{PublicSend: PublicSend{ID: p.ID, Chain: p.Chain, TxID: tx.TxHash().String(), Destination: p.Destination, Amount: p.Amount, Fee: p.Fee, Change: total - p.Amount - p.Fee}, Raw: contract.Hex(tx), Digest: digest}
	if e.s.Sends == nil {
		e.s.Sends = map[string]*WalletSend{}
	}
	e.s.Sends[p.ID] = send
	// Persist the exact signed transaction and reserve its inputs before any
	// broadcast. Ambiguous network failures retry this transaction, never a new one.
	if err := e.save(); err != nil {
		return PublicSend{}, err
	}
	e.advanceSend(ctx, send)
	if err := e.save(); err != nil {
		return PublicSend{}, err
	}
	return send.PublicSend, nil
}
func (e *Engine) advanceSend(ctx context.Context, send *WalletSend) {
	t, err := e.nodes[send.Chain].Transaction(ctx, send.TxID)
	if err == nil {
		send.Confirmations = t.Confirmations
		send.Submitted = true
		send.Error = ""
		return
	}
	if !chain.TransactionNotFound(err) {
		send.Error = err.Error()
		return
	}
	send.Confirmations = 0
	if time.Now().Unix()-send.LastAttempt < 30 {
		return
	}
	send.LastAttempt = time.Now().Unix()
	if _, err := e.nodes[send.Chain].Broadcast(ctx, send.Raw); err != nil {
		send.Error = fmt.Sprintf("Send saved; broadcast will retry: %v", err)
		return
	}
	send.Submitted = true
	send.Error = ""
}
func (e *Engine) advanceSends(ctx context.Context) {
	ids := make([]string, 0, len(e.s.Sends))
	for id, send := range e.s.Sends {
		if send.Confirmations < 6 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return
	}
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > e.sendCursor }) % len(ids)
	// Transfers have a separate budget, preserving time for swaps and rescues.
	sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for i := 0; i < len(ids); i++ {
		if sendCtx.Err() != nil {
			return
		}
		id := ids[(start+i)%len(ids)]
		e.sendCursor = id // Advance even when this lookup uses the remaining budget.
		e.advanceSend(sendCtx, e.s.Sends[id])
	}
}
