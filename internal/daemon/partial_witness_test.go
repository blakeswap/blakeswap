package daemon

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/wire"
)

func partialRPCScanner(t *testing.T, incoming contract.HTLC, claim *wire.MsgTx, fault string) (chain.SpendScanner, *atomic.Bool) {
	t.Helper()
	var delivered atomic.Bool
	endReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "getblockcount":
			result = 102
		case "getblockhash":
			var height uint32
			_ = json.Unmarshal(req.Params[0], &height)
			if height == 101 && fault == "deadline" {
				<-r.Context().Done()
				return
			}
			if height == 101 && fault == "transport" {
				http.Error(w, "injected later block error", 503)
				return
			}
			if height == 102 {
				endReads++
				if endReads > 1 && fault == "reorg" {
					height++
				}
			}
			result = fmt.Sprintf("%064d", height)
		case "getblock":
			if fault == "mempool-spender" {
				result = map[string]any{"tx": []any{}}
				break
			}
			result = map[string]any{"tx": []any{map[string]any{"txid": claim.TxHash().String(), "vin": []any{map[string]any{"txid": incoming.TxID, "vout": incoming.Vout}}}}}
		case "getrawtransaction":
			var id string
			_ = json.Unmarshal(req.Params[0], &id)
			if id != claim.TxHash().String() {
				http.Error(w, "later mempool spender disappeared", 503)
				return
			}
			delivered.Store(true)
			result = chain.Transaction{TxID: claim.TxHash().String(), Hex: contract.Hex(claim), Confirmations: 3}
		case "gettxspendingprevout":
			if fault == "mempool" {
				http.Error(w, "injected mempool read error", 503)
				return
			}
			if fault == "mempool-spender" {
				result = []any{map[string]any{"txid": incoming.TxID, "vout": incoming.Vout, "spendingtxid": claim.TxHash().String()}, map[string]any{"txid": incoming.TxID, "vout": incoming.Vout, "spendingtxid": fmt.Sprintf("%064d", 42)}}
			} else {
				result = []any{}
			}
		default:
			t.Errorf("unexpected RPC %s", req.Method)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": 1})
	}))
	t.Cleanup(server.Close)
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("test:private"), 0600); err != nil {
		t.Fatal(err)
	}
	rpc, err := chain.New(incoming.Chain, server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	return &chain.Scanner{RPC: rpc}, &delivered
}

func partialElectrumScanner(t *testing.T, incoming contract.HTLC, funding string, claim *wire.MsgTx, fault string) (chain.SpendScanner, *atomic.Bool) {
	t.Helper()
	if incoming.Chain != chain.BTC {
		t.Fatal("fixture requires BTC observation")
	}
	header := make([]byte, 80)
	binary.LittleEndian.PutUint32(header, 1)
	binary.LittleEndian.PutUint32(header[68:], 1700000000)
	binary.LittleEndian.PutUint32(header[72:], 0x207fffff)
	for nonce := uint32(0); ; nonce++ {
		binary.LittleEndian.PutUint32(header[76:], nonce)
		hash, err := chain.HeaderHash(header)
		if err != nil {
			t.Fatal(err)
		}
		if blockchain.HashToBig(&hash).Cmp(blockchain.CompactToBig(0x207fffff)) <= 0 {
			break
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var delivered atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		decoder := json.NewDecoder(bufio.NewReader(conn))
		headerReads := 0
		for {
			var req struct {
				ID     uint64
				Method string
				Params []json.RawMessage
			}
			if decoder.Decode(&req) != nil {
				return
			}
			var result any
			var replyError any
			switch req.Method {
			case "blockchain.headers.subscribe":
				result = map[string]any{"height": 102, "hex": hex.EncodeToString(header)}
			case "blockchain.block.header":
				headerReads++
				result = hex.EncodeToString(header)
				if fault == "reorg" && headerReads > 2 {
					result = "invalid changed header"
				}
			case "blockchain.transaction.get":
				var id string
				_ = json.Unmarshal(req.Params[0], &id)
				switch id {
				case incoming.TxID:
					result = funding
				case claim.TxHash().String():
					result = contract.Hex(claim)
					delivered.Store(true)
				default:
					replyError = map[string]any{"code": -32603, "message": "injected later history lookup failure"}
				}
			case "blockchain.scripthash.get_history":
				height := 0
				if fault == "inclusion" {
					height = 100
				}
				history := []any{map[string]any{"tx_hash": claim.TxHash().String(), "height": height}}
				if fault == "later-history" {
					history = append(history, map[string]any{"tx_hash": fmt.Sprintf("%064d", 42), "height": 0})
				}
				result = history
			case "blockchain.transaction.get_merkle":
				replyError = map[string]any{"code": -32603, "message": "injected inclusion failure"}
			default:
				t.Errorf("unexpected Electrum call %s", req.Method)
				return
			}
			if json.NewEncoder(conn).Encode(map[string]any{"id": req.ID, "result": result, "error": replyError}) != nil {
				return
			}
		}
	}()
	e, err := chain.NewElectrum(chain.Regtest, chain.BTC, "tcp://"+listener.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(); _ = listener.Close(); <-done })
	return e, &delivered
}

func partialWitnessScenarios(t *testing.T, run func(*testing.T, string, string)) {
	for _, backend := range []string{"rpc", "electrum"} {
		faults := []string{"deadline", "transport", "mempool", "mempool-spender", "reorg"}
		if backend == "electrum" {
			faults = []string{"inclusion", "later-history", "reorg"}
		}
		for _, fault := range faults {
			t.Run(backend+"/"+fault, func(t *testing.T) { run(t, backend, fault) })
		}
	}
}
func partialWitnessScanner(t *testing.T, backend, fault string, c contract.HTLC, funding string, claim *wire.MsgTx) (chain.SpendScanner, *atomic.Bool) {
	if backend == "electrum" {
		return partialElectrumScanner(t, c, funding, claim, fault)
	}
	return partialRPCScanner(t, c, claim, fault)
}

func TestIsolatedIncompleteScanRetainsWitnessAcrossRestart(t *testing.T) {
	partialWitnessScenarios(t, func(t *testing.T, backend, fault string) {
		for _, role := range []string{"maker", "taker"} {
			t.Run(role, func(t *testing.T) {
				sell := chain.BTC
				if role == "maker" {
					sell = chain.Blake
				}
				e, swap, b, secret := isolatedFixtureSell(t, role, sell)
				incoming, own, funding := swap.Short, swap.Long, swap.ShortFunding
				if role == "maker" {
					incoming, own, funding = swap.Long, swap.Short, swap.LongFunding
				}
				key, _ := e.swapKey(incoming.Chain, swap.ID)
				claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
				if err != nil {
					t.Fatal(err)
				}
				scanner, delivered := partialWitnessScanner(t, backend, fault, incoming, funding, claim)
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
				e.scanners[incoming.Chain] = scanner
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				all, err := e.scan(ctx)
				cancel()
				if err == nil || all[incoming.Chain] != nil || !delivered.Load() {
					t.Fatal("fixture did not parse witness before incomplete scan", err, delivered.Load())
				}
				var saved State
				if _, err := e.vault.Load(&saved); err != nil {
					t.Fatal(err)
				}
				e.s = saved
				swap = e.s.Swaps[swap.ID]
				e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
				e.nodes[own.Chain] = &recoveryClockBackend{Backend: b}
				e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
				refundErr := e.checkRefundAcceleration(context.Background(), swap, own)
				if !swap.SecretObserved || !swap.IncomingClaimSeen || refundErr == nil {
					t.Fatalf("parsed witness lost across incomplete scan/restart: observed=%v incomingClaimSeen=%v refundGate=%v", swap.SecretObserved, swap.IncomingClaimSeen, refundErr)
				}
			})
		}
	})
}

func TestIsolatedTowerIncompleteScanRetainsWitnessAcrossRestart(t *testing.T) {
	partialWitnessScenarios(t, func(t *testing.T, backend, fault string) {
		e, s, b, secret := isolatedFixture(t, "maker")
		tower := e.ownTower()
		s.Protection = &tower
		target, observe := s.Long, s.Short
		job, err := e.makeJob(s, target, "claim", &observe, s.Terms.Takeover)
		if err != nil {
			t.Fatal(err)
		}
		e.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job}}
		e.s.Swaps = map[string]*Swap{}
		if err := e.save(); err != nil {
			t.Fatal(err)
		}
		key, _ := e.swapKey(observe.Chain, s.ID)
		claim, err := contract.Spend(observe, key, e.scripts[observe.Chain], 2000, false, 0, nil, 0, secret)
		if err != nil {
			t.Fatal(err)
		}
		scanner, delivered := partialWitnessScanner(t, backend, fault, observe, s.ShortFunding, claim)
		e.chainFresh[target.Chain], e.chainFresh[observe.Chain] = false, true
		e.towerScanners = map[chain.ID]chain.SpendScanner{observe.Chain: scanner}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		all, err := e.scanTower(ctx)
		cancel()
		if err == nil || all[observe.Chain] != nil || !delivered.Load() {
			t.Fatal("partial tower scan accepted or witness never parsed", err)
		}
		var saved State
		if _, err := e.vault.Load(&saved); err != nil {
			t.Fatal(err)
		}
		if saved.TowerJobs[job.ID].Secret != hex.EncodeToString(secret) {
			t.Fatal("partial witness not durable")
		}
		e.s = saved
		e.chainFresh[target.Chain], e.chainFresh[observe.Chain] = true, false
		broadcasts := 0
		b.broadcast = func(raw string) (string, error) {
			broadcasts++
			tx, err := contract.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := contract.ExtractSecret(target, tx); !ok || tx.TxOut[1].Value != protocol.Bounty(target.Amount, job.BPS) {
				t.Fatal("restored claim authorization changed")
			}
			return tx.TxHash().String(), nil
		}
		if err := e.advanceTower(context.Background(), all); err != nil || broadcasts != 0 {
			t.Fatal("partial scan authorized claim", err)
		}
		if err := e.advanceTower(context.Background(), map[chain.ID]map[string]chain.Observation{target.Chain: {}}); err != nil || broadcasts != 1 {
			t.Fatal("complete target failed recovery after witness disappeared", err, broadcasts)
		}
	})
}

func TestIsolatedWitnessDurabilityFailureStopsPublication(t *testing.T) {
	e, s, b, secret := isolatedFixture(t, "maker")
	incoming := s.Long
	key, _ := e.swapKey(incoming.Chain, s.ID)
	claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	scanner, _ := partialRPCScanner(t, incoming, claim, "transport")
	e.scanners[incoming.Chain] = scanner
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	all, err := e.scan(context.Background())
	if err == nil || e.fatal == nil || all[incoming.Chain] != nil {
		t.Fatal("failed witness persistence accepted", err)
	}
	b.broadcast = func(string) (string, error) { t.Fatal("published after durability failure"); return "", nil }
	if err := e.broadcast(context.Background(), incoming.Chain, contract.Hex(claim), false); err == nil {
		t.Fatal("fatal durability state bypassed")
	}
}
