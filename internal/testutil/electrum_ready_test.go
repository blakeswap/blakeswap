package testutil

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/wire"
)

func TestElectrumBridgeIndexesBeforeServing(t *testing.T) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: ^uint32(0)}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		t.Fatal(err)
	}
	blockStarted, releaseBlock := make(chan struct{}, 1), make(chan struct{})
	defer close(releaseBlock)
	var blockReads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "getblockcount":
			result = 0
		case "getblockhash":
			result = "fixture-genesis"
		case "getblock":
			blockReads.Add(1)
			select {
			case blockStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseBlock:
			case <-r.Context().Done():
				return
			}
			result = map[string]any{"tx": []map[string]string{{"txid": tx.TxHash().String(), "hex": hex.EncodeToString(raw.Bytes())}}}
		case "getrawmempool":
			result = []string{}
		default:
			t.Errorf("unexpected method %s", req.Method)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil})
	}))
	t.Cleanup(server.Close)
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("fixture:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rpc, err := chain.New(chain.BTC, server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	type ready struct {
		bridge   *ElectrumBridge
		endpoint string
	}
	returned := make(chan ready, 1)
	go func() { b, endpoint := NewElectrumBridge(t, rpc); returned <- ready{b, endpoint} }()
	select {
	case got := <-returned:
		t.Fatalf("bridge exposed %s before its initial block index completed (%d block reads)", got.endpoint, blockReads.Load())
	case <-blockStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial indexing did not begin")
	}
	select {
	case <-returned:
		t.Fatal("bridge returned while initial block was still unavailable")
	default:
	}
	// Let the real initial block read finish; no production deadline is changed.
	releaseBlock <- struct{}{}
	var got ready
	select {
	case got = <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared bridge did not return")
	}
	if got.endpoint == "" || len(got.bridge.blocks) != 1 {
		t.Fatal("bridge returned without a complete initial index")
	}
	target, _ := json.Marshal(sh([]byte{0x51}))
	history, err := got.bridge.call(context.Background(), "blockchain.scripthash.get_history", []json.RawMessage{target})
	if err != nil {
		t.Fatal(err)
	}
	rows := history.([]map[string]any)
	if len(rows) != 1 || rows[0]["tx_hash"] != tx.TxHash().String() || blockReads.Load() != 1 {
		t.Fatalf("first history lacks prepared rows or reread block bodies: rows=%v reads=%d", rows, blockReads.Load())
	}
}

func TestElectrumBridgePreparationStopsBeforeListening(t *testing.T) {
	for _, mode := range []string{"cancel", "RPC failure"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Method string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				close(started)
				if mode == "cancel" {
					<-r.Context().Done()
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"code": -1, "message": "initial index unavailable"}})
			}))
			defer server.Close()
			cookie := filepath.Join(t.TempDir(), "cookie")
			if err := os.WriteFile(cookie, []byte("fixture:fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			rpc, err := chain.New(chain.BTC, server.URL, cookie)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				b   *ElectrumBridge
				l   net.Listener
				err error
			}
			done := make(chan result, 1)
			go func() { b, l, err := listenPreparedElectrumBridge(ctx, rpc); done <- result{b, l, err} }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("initial index did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case got := <-done:
				if got.l != nil {
					got.l.Close()
				}
				if got.err == nil || got.b != nil || got.l != nil {
					t.Fatalf("failed preparation returned a live fixture: bridge=%v listener=%v error=%v", got.b != nil, got.l != nil, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed preparation did not stop")
			}
		})
	}
}
