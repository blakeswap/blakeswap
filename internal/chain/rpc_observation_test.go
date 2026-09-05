package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func fakeRPC(t *testing.T, handler func(string, []json.RawMessage) (any, *RPCError)) *RPC {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		result, rpcErr := handler(request.Method, request.Params)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": rpcErr})
	}))
	t.Cleanup(server.Close)
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("user:password"), 0600); err != nil {
		t.Fatal(err)
	}
	rpc, err := New(BTC, server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	return rpc
}

func TestObserveHistoryReadinessAndInterruptedRecovery(t *testing.T) {
	var mu sync.Mutex
	known, ready, scanning, incomplete := false, false, false, false
	imports := 0
	rpc := fakeRPC(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "listwallets":
			return []string{"watch"}, nil
		case "getwalletinfo":
			if scanning {
				return map[string]any{"scanning": map[string]any{"progress": .5}}, nil
			}
			return map[string]any{"scanning": false}, nil
		case "listdescriptors":
			list := []any{}
			if known {
				list = append(list, map[string]any{"desc": "addr(deposit)#checksum", "timestamp": 1})
			}
			return map[string]any{"descriptors": list}, nil
		case "getdescriptorinfo":
			return map[string]any{"descriptor": "addr(deposit)#checksum"}, nil
		case "getaddressinfo":
			labels := []string{}
			if ready {
				labels = append(labels, "blakeswap-history-ready-v1")
			}
			return map[string]any{"labels": labels}, nil
		case "importdescriptors":
			imports++
			known = true
			if !strings.Contains(string(params[0]), `"timestamp":0`) {
				t.Error("historical recovery skipped")
			}
			if incomplete {
				return []any{}, nil
			}
			return []any{map[string]any{"success": true}}, nil
		case "setlabel":
			ready = true
			return nil, nil
		default:
			t.Errorf("unexpected %s", method)
			return nil, &RPCError{-1, method}
		}
	})
	observe := func() error { _, err := rpc.Observe(context.Background(), "watch", []string{"deposit"}); return err }
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := imports
	ready = false
	scanning = true
	mu.Unlock()
	if count != 1 {
		t.Fatalf("reconnected wallet rescanned %d times", count)
	}
	if err := observe(); err == nil {
		t.Fatal("in-progress rescan marked ready")
	}
	mu.Lock()
	count = imports
	scanning = false
	incomplete = true
	mu.Unlock()
	if count != 1 {
		t.Fatal("restarted an active scan")
	}
	if err := observe(); err == nil {
		t.Fatal("partial import accepted")
	}
	mu.Lock()
	marked := ready
	incomplete = false
	mu.Unlock()
	if marked {
		t.Fatal("partial history marked ready")
	}
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count = imports
	mu.Unlock()
	if count != 3 {
		t.Fatalf("interrupted import was not recovered exactly once: %d", count)
	}
}

func TestHistoryCallOutlivesOrdinaryTimeoutButHonorsCancellation(t *testing.T) {
	rpc := fakeRPC(t, func(method string, _ []json.RawMessage) (any, *RPCError) {
		time.Sleep(40 * time.Millisecond)
		return true, nil
	})
	rpc.client.Timeout = 5 * time.Millisecond
	var value bool
	if err := rpc.Call(context.Background(), "ordinary", &value); err == nil {
		t.Fatal("ordinary timeout ignored")
	}
	if err := rpc.historyCall(context.Background(), "importdescriptors", &value); err != nil || !value {
		t.Fatal("history inherited short timeout", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rpc.historyCall(ctx, "importdescriptors", &value); !errors.Is(err, context.Canceled) {
		t.Fatal("history ignored shutdown", err)
	}
}

func TestScannerIgnoresUnrelatedMempoolAndTracksSecret(t *testing.T) {
	previous := chainhash.Hash{1}
	tx := wire.NewMsgTx(2)
	secret := bytes.Repeat([]byte{7}, 32)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: previous, Index: 2}, Witness: wire.TxWitness{[]byte("signature"), secret}})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		t.Fatal(err)
	}
	id, op := tx.TxHash().String(), OutpointKey(previous.String(), 2)
	rawCalls, targetedCalls, globalCalls := 0, 0, 0
	rpc := fakeRPC(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		switch method {
		case "getblockcount":
			return 0, nil
		case "gettxspendingprevout":
			targetedCalls++
			var inputs []struct {
				TxID string
				Vout uint32
			}
			if err := json.Unmarshal(params[0], &inputs); err != nil || len(inputs) != 1 || inputs[0].TxID != previous.String() || inputs[0].Vout != 2 {
				t.Error("query did not target watched output", string(params[0]))
			}
			return []any{map[string]any{"txid": previous.String(), "vout": 2, "spendingtxid": id}}, nil
		case "getrawmempool":
			globalCalls++
			pool := make([]string, 10000)
			for i := range pool {
				pool[i] = fmt.Sprintf("%064x", i)
			}
			return pool, nil
		case "getrawtransaction":
			rawCalls++
			var requested string
			_ = json.Unmarshal(params[0], &requested)
			if requested != id {
				return nil, &RPCError{-5, "No such mempool or blockchain transaction"}
			}
			return Transaction{TxID: id, Hex: hex.EncodeToString(encoded.Bytes())}, nil
		default:
			t.Errorf("unexpected %s", method)
			return nil, &RPCError{-1, method}
		}
	})
	scanner := &Scanner{RPC: rpc}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		observations, err := scanner.Scan(ctx, 1, []string{op})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		observed, ok := observations[op]
		if !ok || observed.TxID != id || observed.Confirmations != 0 || !bytes.Equal(observed.Tx.TxIn[0].Witness[1], secret) {
			t.Fatal("lost mempool secret")
		}
	}
	if globalCalls != 0 || rawCalls != 3 || targetedCalls != 3 {
		t.Fatalf("work scales with unrelated mempool: global=%d raw=%d targeted=%d", globalCalls, rawCalls, targetedCalls)
	}
}

type rpcTransportFunc func(*http.Request) (*http.Response, error)

func (f rpcTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRealRPCObservationReconnectSkipsRescan(t *testing.T) {
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("requires external regtest nodes")
	}
	for i, id := range []ID{BTC, Blake} {
		t.Run(string(id), func(t *testing.T) {
			port := fmt.Sprint(19443 + i*10000)
			if configured := os.Getenv("BLAKESWAP_" + strings.ToUpper(string(id)) + "_RPC_PORT"); configured != "" {
				port = configured
			}
			rpc, err := New(id, "http://127.0.0.1:"+port, filepath.Join(root, ".local", string(id), "regtest", ".cookie"))
			if err != nil {
				t.Fatal(err)
			}
			defer rpc.Close()
			ctx := context.Background()
			var address string
			if err = rpc.WithWallet("faucet").Call(ctx, "getnewaddress", &address); err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("test-observe-%d", time.Now().UnixNano())
			if _, err = rpc.Observe(ctx, name, []string{address}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := rpc.Call(ctx, "unloadwallet", nil, name); err != nil {
					t.Error(err)
				}
			})
			// Fail if reopening relies on an unobserved assumption about Core's
			// actual descriptor timestamps or address-label representation.
			rpc.client.Transport = rpcTransportFunc(func(request *http.Request) (*http.Response, error) {
				raw, err := request.GetBody()
				if err != nil {
					return nil, err
				}
				var body struct{ Method string }
				err = json.NewDecoder(raw).Decode(&body)
				raw.Close()
				if err != nil {
					return nil, err
				}
				if body.Method == "importdescriptors" {
					return nil, errors.New("reconnect attempted historical rescan")
				}
				return http.DefaultTransport.RoundTrip(request)
			})
			if _, err = rpc.Observe(ctx, name, []string{address}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
