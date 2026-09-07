package testutil

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// A shallow fork must not turn a bounded wallet refresh into a genesis scan.
// The fake node counts block reads; actual RPC/Electrum matrices separately
// validate the same rollback and settlement paths against both consensus nodes.
func TestElectrumBridgeRetainsCanonicalPrefix(t *testing.T) {
	type transaction struct {
		TxID string `json:"txid"`
		Hex  string `json:"hex"`
	}
	type block struct {
		Tx []transaction `json:"tx"`
	}
	makeTx := func(n uint32, spend *transaction, script byte) transaction {
		t.Helper()
		tx := wire.NewMsgTx(2)
		tx.LockTime = n
		point := wire.OutPoint{Index: ^uint32(0)}
		if spend != nil {
			hash, err := chainhash.NewHashFromStr(spend.TxID)
			if err != nil {
				t.Fatal(err)
			}
			point = wire.OutPoint{Hash: *hash, Index: 0}
		}
		tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
		tx.AddTxOut(wire.NewTxOut(1000, []byte{script}))
		var raw bytes.Buffer
		if err := tx.Serialize(&raw); err != nil {
			t.Fatal(err)
		}
		return transaction{tx.TxHash().String(), hex.EncodeToString(raw.Bytes())}
	}
	var mu sync.Mutex
	canonical := make([]string, 101)
	blocks := map[string]block{}
	transactions := map[string]transaction{}
	for h := range canonical {
		canonical[h] = fmt.Sprintf("block-%d", h)
		tx := makeTx(uint32(h), nil, 0x52)
		blocks[canonical[h]] = block{[]transaction{tx}}
		transactions[tx.TxID] = tx
	}
	deposit := makeTx(1000, nil, 0x51)
	orphan := makeTx(1001, &deposit, 0x52)
	blocks[canonical[98]] = block{[]transaction{deposit}}
	blocks[canonical[100]] = block{[]transaction{orphan}}
	transactions[deposit.TxID], transactions[orphan.TxID] = deposit, orphan
	var pool []string
	var blockReads []string
	var hashReads []int
	failHash := -1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var req struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		var rpcErr *chain.RPCError
		switch req.Method {
		case "getblockcount":
			result = len(canonical) - 1
		case "getblockhash":
			var h int
			_ = json.Unmarshal(req.Params[0], &h)
			hashReads = append(hashReads, h)
			if h == failHash || h < 0 || h >= len(canonical) {
				rpcErr = &chain.RPCError{Code: -8, Message: "hash unavailable"}
			} else {
				result = canonical[h]
			}
		case "getblock":
			var hash string
			_ = json.Unmarshal(req.Params[0], &hash)
			blockReads = append(blockReads, hash)
			result = blocks[hash]
		case "getrawmempool":
			result = pool
		case "getrawtransaction":
			var id string
			_ = json.Unmarshal(req.Params[0], &id)
			result = transactions[id].Hex
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": rpcErr})
	}))
	defer server.Close()
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("fixture:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rpc, err := chain.New(chain.Blake, server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	b := &ElectrumBridge{rpc: rpc, txs: map[string]indexedTx{}}
	ctx := context.Background()
	target, _ := json.Marshal(sh([]byte{0x51}))
	query := func(method string) []map[string]any {
		t.Helper()
		result, err := b.call(ctx, method, []json.RawMessage{target})
		if err != nil {
			t.Fatal(err)
		}
		return result.([]map[string]any)
	}
	assertHistory := func(want map[string]uint32) {
		t.Helper()
		got := map[string]uint32{}
		for _, item := range query("blockchain.scripthash.get_history") {
			got[item["tx_hash"].(string)] = item["height"].(uint32)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("history %v, want %v", got, want)
		}
	}
	trace := func() ([]string, []int) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), blockReads...), append([]int(nil), hashReads...)
	}
	assertHistory(map[string]uint32{deposit.TxID: 98, orphan.TxID: 100})
	readBlocks, readHashes := trace()
	if len(readBlocks) != 101 || len(query("blockchain.scripthash.listunspent")) != 0 {
		t.Fatal("initial block index or spend missing")
	}
	mu.Lock()
	canonical = canonical[:100]
	pool = []string{orphan.TxID}
	blockReads, hashReads = nil, nil
	mu.Unlock()
	assertHistory(map[string]uint32{deposit.TxID: 98, orphan.TxID: 0})
	readBlocks, readHashes = trace()
	if len(readBlocks) != 0 || !reflect.DeepEqual(readHashes, []int{99}) || len(b.blocks) != 100 {
		t.Fatalf("short rollback rebuilt prefix: blocks=%v hashes=%v", readBlocks, readHashes)
	}
	if len(query("blockchain.scripthash.listunspent")) != 0 {
		t.Fatal("orphan transaction returned to mempool must still spend deposit")
	}
	mu.Lock()
	pool = nil
	mu.Unlock()
	assertHistory(map[string]uint32{deposit.TxID: 98})
	if coins := query("blockchain.scripthash.listunspent"); len(coins) != 1 || coins[0]["tx_hash"] != deposit.TxID {
		t.Fatal("evicted orphan did not restore canonical deposit", coins)
	}
	mu.Lock()
	canonical = append(canonical, "replacement-100")
	blocks["replacement-100"] = block{[]transaction{orphan}}
	blockReads = nil
	mu.Unlock()
	assertHistory(map[string]uint32{deposit.TxID: 98, orphan.TxID: 100})
	readBlocks, _ = trace()
	if !reflect.DeepEqual(readBlocks, []string{"replacement-100"}) {
		t.Fatal("reconfirmation reread prefix", readBlocks)
	}
	// A same-height fork requires comparing hashes, not merely truncating to
	// the new height. Failed ancestor lookup must not silently trust old data.
	fresh := makeTx(1002, nil, 0x52)
	mu.Lock()
	canonical[100] = "fork-100"
	blocks["fork-100"] = block{[]transaction{fresh}}
	failHash = 99
	blockReads, hashReads = nil, nil
	mu.Unlock()
	if _, err = b.call(ctx, "blockchain.scripthash.get_history", []json.RawMessage{target}); err == nil {
		t.Fatal("failed ancestor lookup accepted stale history")
	}
	readBlocks, _ = trace()
	if len(readBlocks) != 0 || b.blocks[100].hash != "replacement-100" || b.txs[orphan.TxID].height != 100 {
		t.Fatal("failed ancestor lookup partially changed the index")
	}
	mu.Lock()
	failHash = -1
	blockReads, hashReads = nil, nil
	mu.Unlock()
	assertHistory(map[string]uint32{deposit.TxID: 98})
	readBlocks, readHashes = trace()
	if !reflect.DeepEqual(readBlocks, []string{"fork-100"}) || !reflect.DeepEqual(readHashes, []int{100, 99, 100}) {
		t.Fatalf("same-height retry rebuilt prefix: blocks=%v hashes=%v", readBlocks, readHashes)
	}
	if _, ok := b.txs[orphan.TxID]; ok || len(query("blockchain.scripthash.listunspent")) != 1 {
		t.Fatal("replaced confirmed spender remained in index")
	}
}
