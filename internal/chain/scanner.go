package chain

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"sort"
	"strings"
)

// Observation is a current-chain spend, not an irrevocable settlement verdict.
type Observation struct {
	TxID          string
	Tx            *wire.MsgTx
	Height        uint32
	Confirmations int
}
type Scanner struct {
	RPC       *RPC
	cursor    uint32
	tip       string
	interest  string
	confirmed map[string]Observation
}

func OutpointKey(txid string, vout uint32) string { return fmt.Sprintf("%s:%d", txid, vout) }
func parseRaw(raw string) (*wire.MsgTx, error) {
	b, e := hex.DecodeString(raw)
	if e != nil {
		return nil, e
	}
	tx := wire.NewMsgTx(2)
	reader := strings.NewReader(string(b))
	e = tx.Deserialize(reader)
	if e == nil && reader.Len() != 0 {
		return nil, fmt.Errorf("trailing transaction bytes")
	}
	return tx, e
}
func (s *Scanner) Scan(ctx context.Context, start uint32, outpoints []string) (map[string]Observation, error) {
	result := map[string]Observation{}
	if len(outpoints) == 0 {
		return result, nil
	}
	sort.Strings(outpoints)
	interest := strings.Join(outpoints, ",")
	set := map[string]bool{}
	for _, op := range outpoints {
		set[op] = true
	}
	height, e := s.RPC.Height(ctx)
	if e != nil {
		return nil, e
	}
	if start < 1 {
		start = 1
	}
	reset := s.confirmed == nil || s.interest != interest || s.cursor > height
	if !reset && s.cursor > 0 {
		var hash string
		if e = s.RPC.Call(ctx, "getblockhash", &hash, s.cursor); e != nil {
			return nil, e
		}
		reset = hash != s.tip
	}
	if reset {
		s.cursor = start - 1
		s.tip = ""
		s.confirmed = map[string]Observation{}
		s.interest = interest
	}
	for n := s.cursor + 1; n <= height; n++ {
		var hash string
		if e = s.RPC.Call(ctx, "getblockhash", &hash, n); e != nil {
			return nil, e
		}
		var block struct {
			Tx []struct {
				TxID string `json:"txid"`
				Vin  []struct {
					TxID string `json:"txid"`
					Vout uint32 `json:"vout"`
				}
			}
		}
		if e = s.RPC.Call(ctx, "getblock", &block, hash, 2); e != nil {
			return nil, e
		}
		for _, tx := range block.Tx {
			for _, in := range tx.Vin {
				key := OutpointKey(in.TxID, in.Vout)
				if !set[key] {
					continue
				}
				raw, e := s.RPC.Transaction(ctx, tx.TxID)
				if e != nil {
					return nil, e
				}
				parsed, e := parseRaw(raw.Hex)
				if e != nil {
					return nil, e
				}
				s.confirmed[key] = Observation{tx.TxID, parsed, n, int(height - n + 1)}
			}
		}
		s.cursor = n
		s.tip = hash
	}
	for key, obs := range s.confirmed {
		obs.Confirmations = int(height - obs.Height + 1)
		result[key] = obs
	}
	var pool []string
	if e = s.RPC.Call(ctx, "getrawmempool", &pool); e != nil {
		return nil, e
	}
	for _, id := range pool {
		raw, e := s.RPC.Transaction(ctx, id)
		if e != nil {
			continue
		}
		tx, e := parseRaw(raw.Hex)
		if e != nil {
			return nil, e
		}
		for _, in := range tx.TxIn {
			key := OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)
			if set[key] {
				result[key] = Observation{id, tx, 0, 0}
			}
		}
	}
	// A hash check at the end prevents accepting a snapshot straddling a reorg.
	if s.cursor > 0 {
		var hash string
		if e = s.RPC.Call(ctx, "getblockhash", &hash, s.cursor); e != nil {
			return nil, e
		}
		if hash != s.tip {
			s.confirmed = nil
			return nil, fmt.Errorf("chain changed during scan; retry")
		}
	}
	return result, nil
}

func WireOutpoint(txid string, vout uint32) (wire.OutPoint, error) {
	h, e := chainhash.NewHashFromStr(txid)
	if e != nil {
		return wire.OutPoint{}, e
	}
	return wire.OutPoint{Hash: *h, Index: vout}, nil
}
