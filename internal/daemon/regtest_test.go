package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/relay"
	"github.com/blakeswap/blakeswap/internal/testutil"
	"github.com/blakeswap/blakeswap/internal/transport"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type harness struct {
	t       *testing.T
	ctx     context.Context
	engines map[string]*Engine
	configs map[string]Config
	nodes   map[chain.ID]*chain.RPC
}

func newHarness(t *testing.T, bps int64) *harness {
	t.Helper()
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("set BLAKESWAP_REGTEST to the project root")
	}
	h := &harness{t: t, ctx: context.Background(), engines: map[string]*Engine{}, configs: map[string]Config{}, nodes: map[chain.ID]*chain.RPC{}}
	nodes := map[chain.ID]NodeConfig{}
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		port := 19443 + i*10000
		if configured := os.Getenv("BLAKESWAP_" + strings.ToUpper(string(id)) + "_RPC_PORT"); configured != "" {
			var err error
			port, err = strconv.Atoi(configured)
			if err != nil {
				t.Fatal(err)
			}
		}
		cfg := NodeConfig{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Cookie: filepath.Join(root, ".local", string(id), "regtest", ".cookie")}
		nodes[id] = cfg
		r, e := chain.New(id, cfg.URL, cfg.Cookie)
		if e != nil {
			t.Fatal(e)
		}
		h.nodes[id] = r
	}
	var urls []string
	for i := 0; i < 2; i++ {
		r, e := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
		if e != nil {
			t.Fatal(e)
		}
		server := httptest.NewServer(r)
		t.Cleanup(func() { server.Close(); r.Close() })
		urls = append(urls, "ws"+strings.TrimPrefix(server.URL, "http"))
	}
	tower := TowerConfig{BPS: 50}
	for _, name := range []string{"tower", "maker", "taker"} {
		dir := t.TempDir()
		password := filepath.Join(dir, "pass")
		if e := os.WriteFile(password, []byte(transport.RandomID()), 0600); e != nil {
			t.Fatal(e)
		}
		mode := "trader"
		if name == "tower" {
			mode = "tower"
		}
		cfg := Config{Name: name, Mode: mode, DataDir: dir, PasswordFile: password, Socket: filepath.Join(dir, "daemon.sock"), Relays: urls, Nodes: nodes, Tower: tower}
		h.configs[name] = cfg
		e, err := Open(h.ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		h.engines[name] = e
		watchWallet := "blakeswap-" + e.Status().PubKey[:20]
		t.Cleanup(func() {
			for _, node := range h.nodes {
				if err := node.Call(context.Background(), "unloadwallet", nil, watchWallet); err != nil {
					t.Error("unload fixture wallet", err)
				}
			}
		})
		if name == "tower" {
			tower = e.Config.Tower
		}
	}
	t.Cleanup(func() {
		for _, e := range h.engines {
			if e != nil {
				e.Close()
			}
		}
	})
	for _, name := range []string{"maker", "taker"} {
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			h.command(name, "regtest.faucet", map[string]any{"chain": id, "amount": 100000000})
		}
	}
	h.mine(chain.BTC, 2)
	h.mine(chain.Blake, 2)
	h.tick("maker", "taker", "tower")
	if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1" {
		indexers := map[chain.ID]NodeConfig{}
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			_, endpoint := testutil.NewElectrumBridge(t, h.nodes[id])
			indexers[id] = NodeConfig{Kind: "electrum", URL: endpoint}
		}
		for _, name := range []string{"maker", "taker", "tower"} {
			h.offline(name)
			cfg := h.configs[name]
			cfg.Nodes = indexers
			h.configs[name] = cfg
			h.online(name)
		}
		// Open intentionally permits partial readiness. A new bridge may still
		// be indexing historical blocks after its first bounded request. Trading
		// fixtures require a successful complete observation of BOTH chains;
		// establish it before any offer/funding assertion, preserving backoff.
		for _, name := range []string{"maker", "taker", "tower"} {
			e := h.engines[name]
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				if !e.fresh(id) {
					endpoint := e.nodes[id].(*chain.Failover).Status().Endpoints[0]
					t.Logf("%s %s cold Electrum startup awaiting readiness: %s; endpoint error=%s retry_after=%d", name, id, e.chainErrors[id], endpoint.Error, endpoint.RetryAfter)
				}
			}
			tickUntilConnected(t, e)
		}
	}
	return h
}
func (h *harness) command(name, method string, params any) any {
	h.t.Helper()
	raw, _ := json.Marshal(params)
	result, e := h.engines[name].Command(h.ctx, Request{Method: method, Params: raw})
	if e != nil {
		h.t.Fatalf("%s %s: %v", name, method, e)
	}
	return result
}
func (h *harness) tick(names ...string) {
	h.t.Helper()
	for _, name := range names {
		e := h.engines[name]
		if e == nil {
			h.t.Fatal("ticking offline daemon", name)
		}
		for _, d := range e.s.Outbox {
			d.LastAttempt = 0
		}
		for _, j := range e.s.TowerJobs {
			j.LastAttempt = 0
		}
		if err := e.Tick(h.ctx); err != nil {
			h.t.Fatalf("%s tick: %v", name, err)
		}
	}
}
func (h *harness) offline(name string) {
	h.t.Helper()
	if e := h.engines[name]; e != nil {
		if err := e.Close(); err != nil {
			h.t.Fatal(err)
		}
		h.engines[name] = nil
	}
}
func (h *harness) online(name string) {
	h.t.Helper()
	if h.engines[name] != nil {
		return
	}
	e, err := Open(h.ctx, h.configs[name])
	if err != nil {
		h.t.Fatal(err)
	}
	h.engines[name] = e
}
func (h *harness) mine(id chain.ID, n uint32) {
	h.t.Helper()
	if n == 0 {
		return
	}
	var addr string
	if e := h.nodes[id].WithWallet("faucet").Call(h.ctx, "getnewaddress", &addr); e != nil {
		h.t.Fatal(e)
	}
	if e := h.nodes[id].Call(h.ctx, "generatetoaddress", nil, n, addr); e != nil {
		h.t.Fatal(e)
	}
}
func (h *harness) minePending() {
	h.t.Helper()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		var pool []string
		if e := h.nodes[id].Call(h.ctx, "getrawmempool", &pool); e != nil {
			h.t.Fatal(e)
		}
		if len(pool) > 0 {
			h.mine(id, 2)
		}
	}
}
func (h *harness) height(id chain.ID) uint32 {
	h.t.Helper()
	n, e := h.nodes[id].Height(h.ctx)
	if e != nil {
		h.t.Fatal(e)
	}
	return n
}
func (h *harness) swap(name, id string) *Swap {
	h.t.Helper()
	s := h.engines[name].s.Swaps[id]
	if s == nil {
		h.t.Fatalf("%s has no swap %s; error=%s", name, id, h.engines[name].lastError)
	}
	return s
}
func (h *harness) until(label string, fn func() bool, step func()) {
	h.t.Helper()
	for i := 0; i < 30; i++ {
		if fn() {
			return
		}
		step()
	}
	for name, e := range h.engines {
		if e == nil {
			continue
		}
		status := e.Status()
		h.t.Log(name, status.LastError, status.Swaps)
	}
	h.t.Fatal("did not reach", label)
}

// Each phase runs with the other trader stopped. A new Engine is opened from its
// encrypted database at every handoff; the local relay is the only mailbox.
func (h *harness) fundBoth(sell chain.ID, bps int64) string {
	return h.fundBothFees(sell, bps, 2000, 0)
}
func (h *harness) fundBothFees(sell chain.ID, bps, fee, ownerCap int64) string {
	h.t.Helper()
	o := h.command("maker", "offer.create", map[string]any{"sell": sell, "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": bps, "funding_fee": fee, "owner_fee_cap": ownerCap}).(protocol.Offer)
	// Power loss before the first relay publish must preserve the signed offer.
	h.offline("maker")
	h.online("maker")
	h.tick("maker")
	h.offline("maker")
	h.tick("taker")
	result := h.command("taker", "swap.take", map[string]any{"maker": o.Maker, "id": o.ID, "tower_bps": bps, "funding_fee": fee, "owner_fee_cap": ownerCap}).(map[string]string)
	id := result["id"]
	// The taker's request is likewise saved before any network transmission.
	h.offline("taker")
	h.online("taker")
	h.tick("taker")
	h.offline("taker")
	h.online("maker")
	h.tick("maker")
	h.offline("maker")
	h.online("taker")
	h.until("long funding", func() bool { return h.swap("taker", id).LongSent }, func() { h.tick("taker", "tower") })
	h.minePending()
	h.offline("taker")
	h.online("maker")
	h.until("short funding", func() bool { return h.swap("maker", id).ShortSent }, func() { h.tick("maker", "tower") })
	h.minePending()
	return id
}

func TestRealAsyncSwapRecoveryAndBounties(t *testing.T) {
	scenarios := []struct {
		name string
		sell chain.ID
		bps  int64
	}{{"self-claim", chain.BTC, 50}, {"offline-maker-tower-takeover", chain.Blake, 50}, {"offline-both-refund", chain.BTC, 50}, {"no-tower-reverse-direction", chain.Blake, 0}, {"late-taker-self-refund", chain.BTC, 0}}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			h := newHarness(t, scenario.bps)
			id := h.fundBoth(scenario.sell, scenario.bps)
			maker := h.swap("maker", id)
			terms := *maker.Terms
			short, long := maker.Short, maker.Long
			h.offline("maker")
			if scenario.name == "late-taker-self-refund" {
				h.mine(long.Chain, long.RefundHeight-h.height(long.Chain))
				h.online("taker")
				h.tick("taker")
				if h.swap("taker", id).SecretExposed {
					t.Fatal("late taker revealed a secret")
				}
				h.minePending()
				h.tick("taker")
				if h.swap("taker", id).Stage != "refunded" {
					t.Fatal("reveal cutoff blocked self refund", h.swap("taker", id).Stage, h.swap("taker", id).Error)
				}
				h.mine(short.Chain, short.RefundHeight-h.height(short.Chain))
				h.online("maker")
				h.tick("maker")
				h.minePending()
				h.tick("maker", "taker")
				if h.swap("maker", id).Stage != "refunded" {
					t.Fatal("maker refund failed")
				}
				return
			}
			if scenario.name == "offline-both-refund" {
				for _, job := range h.engines["tower"].s.TowerJobs {
					if job.Secret != "" {
						t.Fatal("tower got a preimage before revelation")
					}
				}
				h.mine(short.Chain, short.RefundHeight+protocol.RefundGrace-h.height(short.Chain))
				h.tick("tower")
				h.minePending()
				h.mine(long.Chain, long.RefundHeight+protocol.RefundGrace-h.height(long.Chain))
				h.tick("tower")
				h.minePending()
				h.online("maker")
				h.tick("maker")
				h.online("taker")
				h.tick("taker")
				if h.swap("maker", id).Stage != "refunded" || h.swap("taker", id).Stage != "refunded" {
					t.Fatal("refund recovery failed", h.swap("maker", id).Stage, h.swap("taker", id).Stage)
				}
				t.Log("both parties refunded while offline; bounty outputs confirmed")
				return
			}
			h.online("taker")
			h.tick("taker")
			taker := h.swap("taker", id)
			if !taker.SecretExposed || taker.SelfClaim == "" {
				t.Fatal("taker did not reveal after confirmed funding", taker.Error)
			}
			h.tick("tower")
			for _, job := range h.engines["tower"].s.TowerJobs {
				if job.Job.Kind == "claim" {
					if job.Secret == "" {
						t.Fatal("tower missed mempool preimage")
					}
					if job.Broadcast != "" {
						t.Fatal("tower broadcast before takeover")
					}
				}
			}
			h.offline("tower")
			h.online("tower")
			h.minePending()
			h.offline("taker")
			if scenario.name == "offline-maker-tower-takeover" {
				if n := h.height(long.Chain); n < terms.Takeover-1 {
					h.mine(long.Chain, terms.Takeover-1-n)
				}
				h.tick("tower")
				for _, job := range h.engines["tower"].s.TowerJobs {
					if job.Job.Kind == "claim" && job.Broadcast != "" {
						t.Fatal("one-block-early tower broadcast")
					}
				}
				h.mine(long.Chain, 1)
				h.tick("tower")
				h.minePending()
			} else {
				h.online("maker")
				h.tick("maker")
				h.minePending()
				h.offline("maker")
			}
			h.online("maker")
			h.tick("maker")
			h.offline("maker")
			h.online("taker")
			h.tick("taker")
			if h.swap("taker", id).Stage != "completed" {
				t.Fatal("taker did not complete", h.swap("taker", id).Stage, h.swap("taker", id).Error)
			}
			h.online("maker")
			h.tick("maker")
			maker = h.swap("maker", id)
			if maker.Stage != "completed" {
				t.Fatal("maker did not complete", maker.Stage, maker.Error)
			}
			if scenario.name == "offline-maker-tower-takeover" {
				if maker.TowerPaid != protocol.Bounty(long.Amount, scenario.bps) {
					t.Fatal("incorrect confirmed fallback fee", maker.TowerPaid)
				}
			} else if maker.TowerPaid != 0 {
				t.Fatal("owner claim charged bounty")
			}
			for _, c := range []contract.HTLC{long, short} {
				out, e := h.nodes[c.Chain].Output(h.ctx, c.TxID, c.Vout)
				if e != nil || out != nil {
					t.Fatal("contract not consumed exactly once", e)
				}
			}
			t.Logf("completed %s long=%s short=%s bounty=%d", id, maker.LongSpend, maker.ShortSpend, maker.TowerPaid)
			// A claim reorg demotes completion. Seeing its secret cannot be undone.
			if scenario.name == "self-claim" {
				tx, e := h.nodes[long.Chain].Transaction(h.ctx, maker.LongSpend)
				if e != nil {
					t.Fatal(e)
				}
				if e = h.nodes[long.Chain].Call(h.ctx, "invalidateblock", nil, tx.BlockHash); e != nil {
					t.Fatal(e)
				}
				h.tick("maker", "taker")
				if !h.swap("maker", id).SecretExposed || h.swap("maker", id).Stage == "completed" {
					t.Fatal("reorg did not demote settlement while preserving secret")
				}
				h.minePending()
				h.tick("maker", "taker")
				if h.swap("maker", id).Stage != "completed" {
					t.Fatal("reorg recovery failed")
				}
			}
		})
	}
}
