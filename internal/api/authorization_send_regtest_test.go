package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/testutil"
	"github.com/blakeswap/blakeswap/internal/transport"
	"google.golang.org/protobuf/proto"
)

type nativeSendFault struct {
	refuse   atomic.Bool
	mu       sync.Mutex
	raw      string
	attempts int
}

func nativeSendEndpoint(t *testing.T, rpc *chain.RPC) (daemon.NodeConfig, *nativeSendFault) {
	t.Helper()
	target, err := url.Parse(rpc.URL)
	if err != nil {
		t.Fatal(err)
	}
	fault := &nativeSendFault{}
	fault.refuse.Store(true)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error("synthetic RPC proxy read", err)
			return
		}
		defer clear(body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error("synthetic RPC proxy decode", err)
			return
		}
		if request.Method == "sendrawtransaction" {
			var raw string
			if len(request.Params) != 1 || json.Unmarshal(request.Params[0], &raw) != nil {
				t.Error("synthetic broadcast input malformed")
				return
			}
			fault.mu.Lock()
			fault.attempts++
			if fault.raw == "" {
				fault.raw = raw
			} else if fault.raw != raw {
				t.Error("retry changed saved signed bytes")
			}
			fault.mu.Unlock()
			if fault.refuse.Load() {
				http.Error(w, "injected temporary pre-publication failure", http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := daemon.NodeConfig{URL: server.URL, Cookie: rpc.Cookie}
	if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1" {
		source, err := chain.New(rpc.ID, server.URL, rpc.Cookie)
		if err != nil {
			t.Fatal(err)
		}
		_, endpoint := testutil.NewElectrumBridge(t, source)
		cfg = daemon.NodeConfig{Kind: "electrum", URL: endpoint}
	}
	return cfg, fault
}
func TestRealNativeRevocationRetainsSavedSendRetries(t *testing.T) {
	h, owners := nativeReviewedFixture(t)
	config := h.configs["maker"]
	config.Nodes = map[chain.ID]daemon.NodeConfig{}
	faults := map[chain.ID]*nativeSendFault{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		config.Nodes[id], faults[id] = nativeSendEndpoint(t, h.nodes[id])
	}
	h.configs["maker"] = config
	h.restart("maker")
	requests := map[chain.ID]*pb.SendCoinsRequest{}
	saved := map[chain.ID]*pb.WalletSend{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		// The previous injected broadcast failure deliberately enters endpoint
		// backoff. Each NEW send still needs a complete fresh two-chain view;
		// let normal observation recover before requesting the next approval.
		// Refusal stays enabled and the saved send's 30-second retry is intact.
		h.waitReady("maker")
		var coin *pb.WalletCoin
		for _, c := range h.status("maker").Coins {
			if c.Chain == string(id) && !c.Reserved && c.Confirmations >= 2 {
				coin = c
				break
			}
		}
		if coin == nil {
			t.Fatal("generated native fixture has no confirmed selected coin")
		}
		request := &pb.SendCoinsRequest{Id: transport.RandomID(), Chain: string(id), Destination: h.status("taker").Addresses[string(id)], Amount: 500000, Fee: 6500, MaxFee: 20000, Inputs: []*pb.Outpoint{{Txid: coin.Txid, Vout: coin.Vout}}, ExpectedNetwork: "regtest"}
		if _, err := h.clients["maker"].SendCoins(h.contexts["maker"], request); err == nil {
			t.Fatal("bearer alone authorized a send")
		}
		sent, err := h.clients["maker"].SendCoins(nativeApproved(t, h, "maker", "wallet.send", request), request)
		if err != nil || sent.GetTxid() == "" || sent.Submitted {
			t.Fatal("expected durable signed send before injected broadcast failure", id, sent, err)
		}
		if _, err := h.nodes[id].Transaction(h.ctx, sent.Txid); err == nil {
			t.Fatal("injected refusal published before consent revocation")
		}
		requests[id], saved[id] = request, sent
	}
	// Verify the durable signed inputs are loadable before initial credentials
	// are locked; this restart does not grant any new request or reset its ID.
	h.restart("maker")
	owner := owners["maker"]
	reads := owner.reads.Load()
	owner.locked.Store(true)
	close(owner.done)
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		faults[id].refuse.Store(false)
		retry, err := h.clients["maker"].SendCoins(h.contexts["maker"], requests[id])
		if err != nil || retry.Txid != saved[id].Txid {
			t.Fatal("read-only signed retry required new consent", retry, err)
		}
		changed := proto.Clone(requests[id]).(*pb.SendCoinsRequest)
		changed.Amount++
		if _, err := h.clients["maker"].SendCoins(h.contexts["maker"], changed); err == nil {
			t.Fatal("saved send authorized different terms")
		}
	}
	// The existing 30-second persisted retry interval remains intact. No test
	// mutates LastAttempt or advances a synthetic daemon clock.
	deadline := time.Now().Add(90 * time.Second)
	confirmed := map[chain.ID]bool{}
	for len(confirmed) < 2 && time.Now().Before(deadline) {
		if err := h.engines["maker"].Tick(h.ctx); err != nil {
			t.Log("awaiting injected endpoint recovery:", err)
		}
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			if confirmed[id] {
				continue
			}
			tx, err := h.nodes[id].Transaction(h.ctx, saved[id].Txid)
			if err == nil {
				if tx.Confirmations < 2 {
					h.mine(id, 2)
				}
				retry, err := h.clients["maker"].SendCoins(h.contexts["maker"], requests[id])
				if err != nil {
					t.Fatal(err)
				}
				if retry.Submitted && retry.Confirmations >= 2 {
					confirmed[id] = true
				}
			}
		}
		if len(confirmed) < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if len(confirmed) != 2 {
		t.Fatal("revoked new consent prevented the signed send retry from confirming", h.status("maker").Sends)
	}
	if owner.reads.Load() != reads {
		t.Fatal("saved send retry reacquired a locked credential")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if fee := actualStrategyFee(t, h, id, saved[id].Txid, 500000); fee != 6500 {
			t.Fatal("saved send retry changed actual fee", fee)
		}
		faults[id].mu.Lock()
		attempts := faults[id].attempts
		raw := faults[id].raw
		faults[id].mu.Unlock()
		tx, err := h.nodes[id].Transaction(h.ctx, saved[id].Txid)
		if err != nil {
			t.Fatal(err)
		}
		if attempts < 2 || tx.Hex != raw {
			t.Fatal("node did not confirm the exact previously signed bytes")
		}
		t.Logf("%s signed send %s confirmed unchanged after revoked native consent and locked provider; actual fee6500", id, saved[id].Txid)
	}
}
