package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/testutil"
)

type endpointFault struct {
	setDown           func(bool)
	dropNextBroadcast func()
}

func rpcFault(t *testing.T, endpoint string) (string, endpointFault) {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var down atomic.Bool
	var dropBroadcast atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "injected endpoint outage", http.StatusServiceUnavailable)
			return
		}
		if dropBroadcast.Load() {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct{ Method string }
			_ = json.Unmarshal(body, &request)
			if request.Method == "sendrawtransaction" && dropBroadcast.CompareAndSwap(true, false) {
				accepted := httptest.NewRecorder()
				proxy.ServeHTTP(accepted, r)
				down.Store(true)
				http.Error(w, "injected lost broadcast response", http.StatusBadGateway)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, endpointFault{setDown: down.Store, dropNextBroadcast: func() { dropBroadcast.Store(true) }}
}
func electrumFault(t *testing.T, endpoint string) (string, endpointFault) {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	down := false
	connections := map[net.Conn]bool{}
	var wg sync.WaitGroup
	setDown := func(v bool) {
		mu.Lock()
		defer mu.Unlock()
		down = v
		if v {
			for c := range connections {
				_ = c.Close()
			}
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if down {
				mu.Unlock()
				client.Close()
				continue
			}
			connections[client] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer client.Close()
				defer func() { mu.Lock(); delete(connections, client); mu.Unlock() }()
				remote, err := net.Dial("tcp", target.Host)
				if err != nil {
					return
				}
				defer remote.Close()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(remote, client); remote.Close(); close(done) }()
				_, _ = io.Copy(client, remote)
				client.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); setDown(true); wg.Wait() })
	return "tcp://" + listener.Addr().String(), endpointFault{setDown: setDown}
}
func installEndpointFaults(t *testing.T, h *harness, fallback bool) map[chain.ID]endpointFault {
	t.Helper()
	configs := map[chain.ID]NodeConfig{}
	faults := map[chain.ID]endpointFault{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		cfg := NodeConfig{Kind: "rpc", URL: h.nodes[id].URL, Cookie: h.nodes[id].Cookie}
		var endpoint string
		var fault endpointFault
		if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1" {
			_, cfg.URL = testutil.NewElectrumBridge(t, h.nodes[id])
			cfg.Kind = "electrum"
			cfg.Cookie = ""
			endpoint, fault = electrumFault(t, cfg.URL)
		} else {
			endpoint, fault = rpcFault(t, cfg.URL)
		}
		primary := cfg
		primary.URL = endpoint
		if fallback {
			primary.Fallbacks = []NodeConfig{cfg}
		}
		configs[id] = primary
		faults[id] = fault
	}
	for _, name := range []string{"maker", "taker", "tower"} {
		h.offline(name)
		cfg := h.configs[name]
		cfg.Nodes = configs
		h.configs[name] = cfg
		h.online(name)
		tickUntilConnected(t, h.engines[name])
	}
	return faults
}
func tickUntilConnected(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var err error
	for ctx.Err() == nil {
		err = e.Tick(ctx)
		if err == nil && e.fresh(chain.BTC) && e.fresh(chain.Blake) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("endpoint observation did not recover: %v", err)
}
func tickDegraded(t *testing.T, e *Engine) {
	t.Helper()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := e.Tick(ctx); err == nil {
		t.Fatal("partial failure missing from Tick diagnostic")
	}
	if time.Since(started) > 11*time.Second {
		t.Fatal("unavailable chain monopolized recovery cycle")
	}
}
func TestRealEndpointFailoverSettlement(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 0)
			faults := installEndpointFaults(t, h, true)
			id := h.fundBothFees(sell, 0, 6500, 20000)
			maker := h.swap("maker", id)
			longRaw, shortRaw := maker.LongFunding, maker.ShortFunding
			for _, fault := range faults {
				fault.setDown(true)
			}
			h.online("taker")
			tickUntilConnected(t, h.engines["taker"])
			h.minePending()
			tickUntilConnected(t, h.engines["maker"])
			h.minePending()
			tickUntilConnected(t, h.engines["taker"])
			tickUntilConnected(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if maker.Stage != "completed" || h.swap("taker", id).Stage != "completed" {
				t.Fatal("failover settlement incomplete", maker.Stage, maker.Error, h.swap("taker", id).Stage)
			}
			if maker.LongFunding != longRaw || maker.ShortFunding != shortRaw {
				t.Fatal("endpoint switch changed funding")
			}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				status := h.engines["maker"].Status().Connections[id]
				if !status.Ready || !status.Sources.Endpoints[1].Active || status.Sources.Endpoints[0].Error == "" {
					t.Fatal("missing failover provenance", status)
				}
			}
			t.Logf("failover settled %s long=%s short=%s", id, maker.LongSpend, maker.ShortSpend)
		})
	}
}
func TestRealIsolatedWitnessRecoveryAndFirstRevealHold(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 0)
			faults := installEndpointFaults(t, h, false)
			id := h.fundBothFees(sell, 0, 6500, 20000)
			maker := h.swap("maker", id)
			incoming, own := maker.Long, maker.Short
			// The taker's internally generated secret stays private when either chain
			// is unreachable, despite both funding transactions already being signed.
			faults[incoming.Chain].setDown(true)
			h.online("taker")
			tickDegraded(t, h.engines["taker"])
			if h.swap("taker", id).SecretExposed || h.swap("taker", id).SelfClaim != "" {
				t.Fatal("first revelation escaped partial-readiness gate")
			}
			// Inject the precise durable crash boundary: both funded outputs exist,
			// but a locally prepared claim has never reached a node. Neither reopening
			// nor manually selecting a higher fee may publish it during the outage.
			takerEngine := h.engines["taker"]
			taker := h.swap("taker", id)
			secret, err := hex.DecodeString(taker.Secret)
			if err != nil {
				t.Fatal(err)
			}
			key, err := takerEngine.swapKey(taker.Short.Chain, id)
			if err != nil {
				t.Fatal(err)
			}
			privateClaim, err := contract.Spend(taker.Short, key, takerEngine.scripts[taker.Short.Chain], 2000, false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			taker.SelfClaim, taker.SecretExposed = contract.Hex(privateClaim), true
			if err := takerEngine.save(); err != nil {
				t.Fatal(err)
			}
			h.offline("taker")
			h.online("taker")
			tickDegraded(t, h.engines["taker"])
			params, _ := json.Marshal(BumpRequest{ID: id, Kind: "claim", Fee: 6000, ExpectedTxID: privateClaim.TxHash().String()})
			if _, err := h.engines["taker"].bumpTransaction(h.ctx, params); err == nil {
				t.Fatal("manual acceleration revealed private crash snapshot")
			}
			if _, err := h.nodes[taker.Short.Chain].Transaction(h.ctx, privateClaim.TxHash().String()); !chain.TransactionNotFound(err) {
				t.Fatal("private claim reached node", err)
			}
			if h.swap("taker", id).SecretObserved {
				t.Fatal("private claim was relabeled witnessed")
			}
			faults[incoming.Chain].setDown(false)
			tickUntilConnected(t, h.engines["taker"])
			h.minePending()
			// Maker can learn the actual witness on its outgoing chain while its claim
			// target is unreachable. Persist that knowledge, then reverse the outage.
			faults[incoming.Chain].setDown(true)
			tickDegraded(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if !maker.SecretObserved || maker.SelfClaim != "" {
				t.Fatal("witness not retained without target access", maker.Error)
			}
			h.offline("maker")
			faults[incoming.Chain].setDown(false)
			faults[own.Chain].setDown(true)
			h.online("maker")
			tickDegraded(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if maker.SelfClaim == "" {
				t.Fatal("persisted witnessed secret did not recover isolated claim", maker.Error)
			}
			claim, err := contract.Parse(maker.SelfClaim)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.nodes[incoming.Chain].Transaction(h.ctx, claim.TxHash().String()); err != nil {
				t.Fatal("isolated claim not actually broadcast", err)
			}
			if maker.OwnerFeeCap != 20000 || len(maker.SelfClaims) != 3 {
				t.Fatal("isolated claim lost fee consent or variants")
			}
			currentID, currentFee := settlementVariant(maker.SelfClaims, maker.ClaimVariant, maker.Long, maker.Short, true)
			if currentFee < 20000 {
				params, _ := json.Marshal(BumpRequest{ID: id, Kind: "claim", Fee: 20000, ExpectedTxID: currentID})
				if result, err := h.engines["maker"].bumpTransaction(h.ctx, params); err != nil || result.Error != "" {
					t.Fatal("isolated witnessed claim acceleration failed", result, err)
				}
			}
			h.mine(incoming.Chain, 2)
			tickDegraded(t, h.engines["maker"])
			if !h.swap("maker", id).IncomingClaimSeen {
				t.Fatal("healthy incoming claim not monitored")
			}
			faults[own.Chain].setDown(false)
			tickUntilConnected(t, h.engines["maker"])
			tickUntilConnected(t, h.engines["taker"])
			if h.swap("maker", id).Stage != "completed" || h.swap("taker", id).Stage != "completed" {
				t.Fatal("isolated recovery did not settle")
			}
			t.Logf("isolated %s claim=%s target=%s", id, claim.TxHash().String(), incoming.Chain)
		})
	}
}
func TestRealIsolatedRefundWaitsForPeerObservation(t *testing.T) {
	h := newHarness(t, 0)
	faults := installEndpointFaults(t, h, false)
	id := h.fundBoth(chain.BTC, 0)
	maker := h.swap("maker", id)
	long, short := maker.Long, maker.Short
	h.offline("maker")
	faults[short.Chain].setDown(true)
	h.mine(long.Chain, long.RefundHeight+1-h.height(long.Chain))
	h.online("taker")
	tickDegraded(t, h.engines["taker"])
	out, err := h.nodes[long.Chain].Output(h.ctx, long.TxID, long.Vout)
	if err != nil || out == nil {
		t.Fatal("refund escaped unavailable incoming-chain guard", err)
	}
	if h.swap("taker", id).SecretExposed {
		t.Fatal("late degraded secret revelation")
	}
	faults[short.Chain].setDown(false)
	tickUntilConnected(t, h.engines["taker"])
	h.minePending()
	tickUntilConnected(t, h.engines["taker"])
	if !strings.Contains(h.swap("taker", id).Stage, "refunded") {
		t.Fatal("refund failed after both-chain observation restored", h.swap("taker", id).Error)
	}
	h.mine(short.Chain, short.RefundHeight+1-h.height(short.Chain))
	h.online("maker")
	tickUntilConnected(t, h.engines["maker"])
	h.minePending()
	tickUntilConnected(t, h.engines["maker"])
	if h.swap("maker", id).Stage != "refunded" {
		t.Fatal("counterparty refund failed", h.swap("maker", id).Error)
	}
	t.Logf("refund held safely through outage then confirmed %s long=%s short=%s", id, h.swap("taker", id).LongSpend, h.swap("maker", id).ShortSpend)
}

func TestRealEndpointAmbiguousBroadcastAndReorgRecovery(t *testing.T) {
	h := newHarness(t, 0)
	faults := installEndpointFaults(t, h, true)
	e := h.engines["maker"]
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			// Warm Electrum fixture history can initially select a secondary while
			// its first RPC-backed index build runs. Reopen against the now-warm
			// sources and prove the primary is active before injecting its outage.
			h.offline("maker")
			h.online("maker")
			e = h.engines["maker"]
			tickUntilConnected(t, e)
			if !e.Status().Connections[id].Sources.Endpoints[0].Active {
				t.Fatal("reorg fault fixture did not begin on primary")
			}
			// The HTTP primary publishes to consensus, then loses its response. The
			// secondary must retry the SAME signed transaction, with inputs reserved.
			if faults[id].dropNextBroadcast != nil {
				faults[id].dropNextBroadcast()
				var point CoinOutpoint
				for _, coin := range e.publicCoins() {
					if coin.Chain == id && !coin.Reserved && coin.Confirmations >= 2 {
						point = CoinOutpoint{coin.TxID, coin.Vout}
						break
					}
				}
				if point.TxID == "" {
					t.Fatal("missing funded test input")
				}
				request := SendRequest{ID: strings.Repeat(string(id), 8), Chain: id, Destination: e.addresses[id], Amount: 100000, Fee: 2000, Inputs: []CoinOutpoint{point}, ExpectedNetwork: "regtest"}
				raw, _ := json.Marshal(request)
				result, err := e.sendCoins(h.ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := h.nodes[id].Transaction(h.ctx, result.TxID)
				if err != nil || tx.Hex != e.s.Sends[request.ID].Raw {
					t.Fatal("ambiguous broadcast changed or lost signed transaction", err)
				}
				if !e.reservedCoins(id, "")[pointKey(point)] {
					t.Fatal("ambiguous publication released reserved inputs")
				}
				h.mine(id, 2)
				tickUntilConnected(t, e)
				t.Logf("ambiguous primary publication recovered %s %s", id, result.TxID)
				// Reopen starts again at primary for the following fork observation.
				faults[id].setDown(false)
				h.offline("maker")
				h.online("maker")
				e = h.engines["maker"]
				tickUntilConnected(t, e)
			}
			var tip string
			if err := h.nodes[id].Call(h.ctx, "getbestblockhash", &tip); err != nil {
				t.Fatal(err)
			}
			faults[id].setDown(true)
			if err := h.nodes[id].Call(h.ctx, "invalidateblock", nil, tip); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[id].Call(h.ctx, "reconsiderblock", nil, tip); err != nil {
					t.Error(err)
				}
			}()
			tickDegraded(t, e)
			if e.fresh(id) {
				t.Fatal("failover accepted a conflicting/reorged tip without reconciliation")
			}
			faults[id].setDown(false)
			tickUntilConnected(t, e)
			faults[id].setDown(true)
			tickUntilConnected(t, e)
			if !e.Status().Connections[id].Sources.Endpoints[1].Active {
				t.Fatal("reconciled reorg did not recover secondary")
			}
			faults[id].setDown(false)
		})
	}
}
