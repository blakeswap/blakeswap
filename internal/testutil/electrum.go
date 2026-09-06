// Package testutil contains loopback-only integration fixtures. It is never
// imported by the daemon or app. The Electrum fixture indexes real regtest RPC
// blocks so signatures, scripts, mempool rules, and reorgs remain node-validated.
package testutil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"

	"crypto/sha256"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

type indexedTx struct {
	raw    string
	tx     *wire.MsgTx
	height uint32
}
type indexedBlock struct {
	hash string
	ids  []string
}
type ElectrumBridge struct {
	mu     sync.Mutex
	rpc    *chain.RPC
	blocks []indexedBlock
	txs    map[string]indexedTx
	// Transform injects adversarial replies in tests. Set before connecting clients.
	Transform func(string, any) any
}

func NewElectrumBridge(t *testing.T, rpc *chain.RPC) (*ElectrumBridge, string) {
	t.Helper()
	b := &ElectrumBridge{rpc: rpc, txs: map[string]indexedTx{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				scanner := bufio.NewScanner(conn)
				scanner.Buffer(make([]byte, 8192), 4<<20)
				for scanner.Scan() {
					var req struct {
						ID     uint64
						Method string
						Params []json.RawMessage
					}
					if json.Unmarshal(scanner.Bytes(), &req) != nil {
						return
					}
					b.mu.Lock()
					result, err := b.call(ctx, req.Method, req.Params)
					if err == nil && b.Transform != nil {
						result = b.Transform(req.Method, result)
					}
					b.mu.Unlock()
					reply := map[string]any{"id": req.ID, "jsonrpc": "2.0", "result": result}
					if err != nil {
						reply["error"] = map[string]any{"code": 1, "message": err.Error()}
					}
					if json.NewEncoder(conn).Encode(reply) != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); wg.Wait() })
	return b, "tcp://" + listener.Addr().String()
}
func (b *ElectrumBridge) sync(ctx context.Context) error {
	height, err := b.rpc.Height(ctx)
	if err != nil {
		return err
	}
	if len(b.blocks) > 0 {
		last := uint32(len(b.blocks) - 1)
		var hash string
		if height < last {
			b.blocks = nil
			b.txs = map[string]indexedTx{}
		} else if err = b.rpc.Call(ctx, "getblockhash", &hash, last); err != nil {
			return err
		} else if b.blocks[last].hash != hash {
			b.blocks = nil
			b.txs = map[string]indexedTx{}
		}
	}
	for h := uint32(len(b.blocks)); h <= height; h++ {
		var hash string
		if err = b.rpc.Call(ctx, "getblockhash", &hash, h); err != nil {
			return err
		}
		var block struct {
			Tx []struct {
				TxID string `json:"txid"`
				Hex  string `json:"hex"`
			}
		}
		if err = b.rpc.Call(ctx, "getblock", &block, hash, 2); err != nil {
			return err
		}
		item := indexedBlock{hash: hash}
		for _, tx := range block.Tx {
			if err = b.put(tx.TxID, tx.Hex, h); err != nil {
				return err
			}
			item.ids = append(item.ids, tx.TxID)
		}
		b.blocks = append(b.blocks, item)
	}
	return nil
}
func (b *ElectrumBridge) put(id, raw string, h uint32) error {
	buf, err := hex.DecodeString(raw)
	if err != nil {
		return err
	}
	tx := wire.NewMsgTx(2)
	if err = tx.Deserialize(bytes.NewReader(buf)); err != nil {
		return err
	}
	b.txs[id] = indexedTx{raw, tx, h}
	return nil
}
func sh(script []byte) string {
	h := sha256.Sum256(script)
	for i := 0; i < 16; i++ {
		h[i], h[31-i] = h[31-i], h[i]
	}
	return hex.EncodeToString(h[:])
}
func (b *ElectrumBridge) call(ctx context.Context, method string, params []json.RawMessage) (any, error) {
	str := func(i int) string {
		var s string
		if i < len(params) {
			_ = json.Unmarshal(params[i], &s)
		}
		return s
	}
	num := func(i int) uint32 {
		var n uint32
		if i < len(params) {
			_ = json.Unmarshal(params[i], &n)
		}
		return n
	}
	switch method {
	case "blockchain.estimatefee":
		var reply struct {
			Rate   json.Number `json:"feerate"`
			Errors []string    `json:"errors"`
		}
		if err := b.rpc.Call(ctx, "estimatesmartfee", &reply, num(0), "CONSERVATIVE"); err != nil {
			return nil, err
		}
		if len(reply.Errors) > 0 || reply.Rate == "" {
			return -1, nil
		}
		return reply.Rate, nil
	case "blockchain.block.headers":
		height, err := b.rpc.Height(ctx)
		if err != nil {
			return nil, err
		}
		var raw string
		var count uint32
		for h := num(0); h <= height && count < num(1); h++ {
			n, _ := json.Marshal(h)
			header, err := b.call(ctx, "blockchain.block.header", []json.RawMessage{n})
			if err != nil {
				return nil, err
			}
			raw += header.(string)
			count++
		}
		return map[string]any{"count": count, "hex": raw, "max": 2016}, nil
	case "server.features":
		return map[string]any{"genesis_hash": chain.Regtest.Genesis(), "hash_function": "sha256"}, nil
	case "blockchain.block.header":
		var hash, raw string
		if err := b.rpc.Call(ctx, "getblockhash", &hash, num(0)); err != nil {
			return nil, err
		}
		err := b.rpc.Call(ctx, "getblockheader", &raw, hash, false)
		return raw, err
	case "blockchain.headers.subscribe":
		height, err := b.rpc.Height(ctx)
		if err != nil {
			return nil, err
		}
		n, _ := json.Marshal(height)
		raw, err := b.call(ctx, "blockchain.block.header", []json.RawMessage{n})
		return map[string]any{"height": height, "hex": raw}, err
	case "blockchain.transaction.get":
		var raw string
		err := b.rpc.Call(ctx, "getrawtransaction", &raw, str(0), false)
		if chain.TransactionNotFound(err) {
			return nil, fmt.Errorf("transaction not found")
		}
		return raw, err
	case "blockchain.transaction.broadcast":
		return b.rpc.Broadcast(ctx, str(0))
	}
	if err := b.sync(ctx); err != nil {
		return nil, err
	}
	if method == "blockchain.transaction.id_from_pos" {
		h, pos := num(0), num(1)
		if int(h) >= len(b.blocks) || int(pos) >= len(b.blocks[h].ids) {
			return nil, fmt.Errorf("invalid block position")
		}
		return b.blocks[h].ids[pos], nil
	}
	if method == "blockchain.transaction.get_merkle" {
		h := num(1)
		if int(h) >= len(b.blocks) {
			return nil, fmt.Errorf("unknown block")
		}
		ids := b.blocks[h].ids
		pos := -1
		var hashes []chainhash.Hash
		for i, id := range ids {
			hash, err := chainhash.NewHashFromStr(id)
			if err != nil {
				return nil, err
			}
			hashes = append(hashes, *hash)
			if id == str(0) {
				pos = i
			}
		}
		if pos < 0 {
			return nil, fmt.Errorf("transaction not in block")
		}
		p := pos
		proof := []string{}
		for len(hashes) > 1 {
			if len(hashes)%2 != 0 {
				hashes = append(hashes, hashes[len(hashes)-1])
			}
			proof = append(proof, hashes[p^1].String())
			var next []chainhash.Hash
			for i := 0; i < len(hashes); i += 2 {
				pair := append(append([]byte{}, hashes[i][:]...), hashes[i+1][:]...)
				next = append(next, chainhash.DoubleHashH(pair))
			}
			hashes = next
			p /= 2
		}
		return map[string]any{"block_height": h, "pos": pos, "merkle": proof}, nil
	}
	// Refresh mempool without retaining evicted transactions.
	for id, tx := range b.txs {
		if tx.height == 0 && id != b.blocks[0].ids[0] {
			delete(b.txs, id)
		}
	}
	var pool []string
	if err := b.rpc.Call(ctx, "getrawmempool", &pool); err != nil {
		return nil, err
	}
	for _, id := range pool {
		var raw string
		if err := b.rpc.Call(ctx, "getrawtransaction", &raw, id, false); err != nil {
			return nil, err
		}
		if err := b.put(id, raw, 0); err != nil {
			return nil, err
		}
	}
	target := str(0)
	history := []map[string]any{}
	coins := []map[string]any{}
	spent := map[string]bool{}
	for _, item := range b.txs {
		for _, in := range item.tx.TxIn {
			spent[in.PreviousOutPoint.String()] = true
		}
	}
	for id, item := range b.txs {
		touched := false
		for i, out := range item.tx.TxOut {
			if sh(out.PkScript) == target {
				touched = true
				if !spent[chain.OutpointKey(id, uint32(i))] {
					coins = append(coins, map[string]any{"tx_hash": id, "tx_pos": i, "height": item.height, "value": out.Value})
				}
			}
		}
		for _, in := range item.tx.TxIn {
			previous, ok := b.txs[in.PreviousOutPoint.Hash.String()]
			if ok && int(in.PreviousOutPoint.Index) < len(previous.tx.TxOut) && sh(previous.tx.TxOut[in.PreviousOutPoint.Index].PkScript) == target {
				touched = true
			}
		}
		if touched {
			history = append(history, map[string]any{"tx_hash": id, "height": item.height})
		}
	}
	switch method {
	case "blockchain.scripthash.get_history":
		return history, nil
	case "blockchain.scripthash.listunspent":
		return coins, nil
	}
	return nil, fmt.Errorf("unsupported fixture method %s", method)
}
