package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

// AddressHistorian is optional so a backend without retained wallet history can
// report that limitation rather than silently substituting its current UTXOs.
// Pages are sorted by transaction ID; the caller periodically starts a new pass
// to discover arrivals before its cursor. Returned transactions are verified by
// the selected backend and include spent receipts. No absence proves eviction.
type AddressHistorian interface {
	AddressHistory(context.Context, string, string, int) (AddressHistoryPage, error)
}
type AddressHistoryPage struct {
	Transactions []Transaction
	Next         string
	Complete     bool
	Source       string
	Generation   uint64
}

// Provenance identifies the endpoint without exposing credentials, query tokens
// or private wallet paths in exported activity.
func historySource(kind, endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint))
	return kind + ":" + hex.EncodeToString(digest[:])
}

func historyPageIDs(ids []string, after string, limit int) ([]string, string, error) {
	if limit < 1 || limit > 20 || len(ids) > 10000 {
		return nil, "", errors.New("address history exceeds bounded page or script capacity")
	}
	sort.Strings(ids)
	unique := ids[:0]
	for _, id := range ids {
		if len(id) != 64 {
			return nil, "", errors.New("invalid history transaction ID")
		}
		if _, err := WireOutpoint(id, 0); err != nil {
			return nil, "", err
		}
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	start := sort.SearchStrings(unique, after)
	for start < len(unique) && unique[start] <= after {
		start++
	}
	end := min(start+limit, len(unique))
	next := ""
	if end < len(unique) {
		next = unique[end-1]
	}
	return unique[start:end], next, nil
}
func historyScript(network Network, address string) ([]byte, error) {
	a, err := btcutil.DecodeAddress(address, network.Params())
	if err != nil || !a.IsForNet(network.Params()) {
		return nil, errors.New("wrong history address network")
	}
	return txscript.PayToAddrScript(a)
}
func receivedBy(tx Transaction, script []byte) (bool, error) {
	parsed, err := parseRaw(tx.Hex)
	if err != nil {
		return false, err
	}
	if parsed.TxHash().String() != tx.TxID {
		return false, errors.New("history transaction ID mismatch")
	}
	var total int64
	matches := false
	for _, out := range parsed.TxOut {
		if out.Value < 0 || out.Value > 2100000000000000-total {
			return false, errors.New("invalid history output amount")
		}
		total += out.Value
		matches = matches || bytes.Equal(out.PkScript, script)
	}
	return matches, nil
}
func (r *RPC) AddressHistory(ctx context.Context, address, after string, limit int) (AddressHistoryPage, error) {
	script, err := historyScript(r.Network, address)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	var received []struct {
		Address string
		TxIDs   []string `json:"txids"`
	}
	// Watch-wallet history includes spent outputs and immature coinbase receipts.
	if err = r.Call(ctx, "listreceivedbyaddress", &received, 0, true, true, address, true); err != nil {
		return AddressHistoryPage{}, err
	}
	var ids []string
	for _, entry := range received {
		if entry.Address == address {
			ids = append(ids, entry.TxIDs...)
		}
	}
	page, next, err := historyPageIDs(ids, after, limit)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	result := AddressHistoryPage{Source: historySource("rpc", r.URL), Next: next, Complete: next == ""}
	for _, id := range page {
		tx, err := r.Transaction(ctx, id)
		if err != nil {
			return AddressHistoryPage{}, err
		}
		matches, err := receivedBy(tx, script)
		if err != nil {
			return AddressHistoryPage{}, err
		}
		if !matches {
			return AddressHistoryPage{}, errors.New("wallet history returned an unrelated transaction")
		}
		result.Transactions = append(result.Transactions, tx)
	}
	return result, nil
}
func (e *Electrum) AddressHistory(ctx context.Context, address, after string, limit int) (AddressHistoryPage, error) {
	script, err := historyScript(e.Network, address)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	history, err := e.history(ctx, script)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	heights := map[string]int64{}
	var ids []string
	for _, item := range history {
		if item.Height < -1 || item.Height > int64(^uint32(0)) {
			return AddressHistoryPage{}, errors.New("invalid history height")
		}
		if old, ok := heights[item.TxID]; ok && old != item.Height {
			return AddressHistoryPage{}, errors.New("conflicting history heights")
		}
		heights[item.TxID] = item.Height
		ids = append(ids, item.TxID)
	}
	page, next, err := historyPageIDs(ids, after, limit)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	result := AddressHistoryPage{Source: historySource("electrum", e.endpoint.String()), Next: next, Complete: next == ""}
	for _, id := range page {
		tx, err := e.raw(ctx, id)
		if err != nil {
			return AddressHistoryPage{}, err
		}
		if heights[id] > 0 {
			tx, err = e.inclusion(ctx, tx, uint32(heights[id]))
			if err != nil {
				return AddressHistoryPage{}, err
			}
		}
		matches, err := receivedBy(tx, script)
		if err != nil {
			return AddressHistoryPage{}, err
		}
		if matches {
			result.Transactions = append(result.Transactions, tx)
		} // Script histories also contain spends.
	}
	return result, nil
}

// HistoryObserver supplies provenance for advisory ledger reconciliation. It is
// separate from spending authorization and may report unavailable observations.
type HistoryObserver interface {
	HistoryTransaction(context.Context, string, uint32, string) (HistoryTransaction, error)
}
type HistoryTransaction struct {
	Transaction          Transaction
	Source               string
	Generation           uint64
	PreviousBlockChanged bool
}

func (r *RPC) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (HistoryTransaction, error) {
	result := HistoryTransaction{Source: historySource("rpc", r.URL)}
	tx, err := r.Transaction(ctx, id)
	result.Transaction = tx
	if err != nil && height > 0 && block != "" && ctx.Err() == nil {
		var current string
		if r.Call(ctx, "getblockhash", &current, height) == nil && current != block {
			result.PreviousBlockChanged = true
		}
	}
	return result, err
}
func (e *Electrum) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (HistoryTransaction, error) {
	result := HistoryTransaction{Source: historySource("electrum", e.endpoint.String())}
	tx, err := e.Transaction(ctx, id)
	result.Transaction = tx
	if err != nil && height > 0 && block != "" && ctx.Err() == nil {
		if header, readErr := e.header(ctx, height); readErr == nil {
			if current, hashErr := HeaderHash(header); hashErr == nil && current.String() != block {
				result.PreviousBlockChanged = true
			}
		}
	}
	return result, err
}
