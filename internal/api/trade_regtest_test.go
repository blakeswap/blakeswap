package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/relay"
	"github.com/blakeswap/blakeswap/internal/testutil"
	"github.com/blakeswap/blakeswap/internal/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type reviewedTradeFixture struct {
	t        *testing.T
	ctx      context.Context
	engines  map[string]*daemon.Engine
	configs  map[string]daemon.Config
	nodes    map[chain.ID]*chain.RPC
	clients  map[string]pb.DaemonServiceClient
	contexts map[string]context.Context
}

func newReviewedTradeFixture(t *testing.T) *reviewedTradeFixture {
	t.Helper()
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("set BLAKESWAP_REGTEST for actual BTC and Blake regtest nodes")
	}
	h := &reviewedTradeFixture{t: t, ctx: context.Background(), engines: map[string]*daemon.Engine{}, configs: map[string]daemon.Config{}, nodes: map[chain.ID]*chain.RPC{}, clients: map[string]pb.DaemonServiceClient{}, contexts: map[string]context.Context{}}
	nodes := map[chain.ID]daemon.NodeConfig{}
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		port := 19443 + i*10000
		if value := os.Getenv("BLAKESWAP_" + strings.ToUpper(string(id)) + "_RPC_PORT"); value != "" {
			var err error
			port, err = strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		cfg := daemon.NodeConfig{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Cookie: filepath.Join(root, ".local", string(id), "regtest", ".cookie")}
		nodes[id] = cfg
		rpc, err := chain.New(id, cfg.URL, cfg.Cookie)
		if err != nil {
			t.Fatal(err)
		}
		h.nodes[id] = rpc
	}
	r, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r)
	t.Cleanup(func() { server.Close(); r.Close() })
	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")
	for _, name := range []string{"maker", "taker"} {
		dir := t.TempDir()
		password := filepath.Join(dir, "password")
		if err := os.WriteFile(password, []byte(transport.RandomID()), 0600); err != nil {
			t.Fatal(err)
		}
		cfg := daemon.Config{Name: name, Mode: "trader", Network: chain.Regtest, DataDir: dir, PasswordFile: password, Relays: []string{relayURL}, Nodes: nodes}
		h.configs[name] = cfg
		engine, err := daemon.Open(h.ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		h.engines[name] = engine
		wallet := "blakeswap-" + engine.Status().PubKey[:20]
		t.Cleanup(func() {
			if e := h.engines[name]; e != nil {
				e.Close()
			}
			for _, rpc := range h.nodes {
				if err := rpc.Call(h.ctx, "unloadwallet", nil, wallet); err != nil {
					t.Error(err)
				}
			}
		})
		socketDir, err := os.MkdirTemp("", "bs-trade-api-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(socketDir) })
		apiServer, err := Listen(h.ctx, filepath.Join(socketDir, "rpc.sock"), &Service{Command: func(ctx context.Context, request daemon.Request) (any, error) {
			return h.engines[name].Command(ctx, request)
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { apiServer.Close() })
		conn, err := grpc.NewClient("unix://"+apiServer.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		h.clients[name] = pb.NewDaemonServiceClient(conn)
		h.contexts[name] = metadata.AppendToOutgoingContext(h.ctx, "authorization", "Bearer "+apiServer.Endpoint.Token)
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			if _, err := h.clients[name].Faucet(h.contexts[name], &pb.FaucetRequest{Chain: string(id), Amount: 100000000}); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.mine(chain.BTC, 2)
	h.mine(chain.Blake, 2)
	h.tick()
	if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1" {
		indexers := map[chain.ID]daemon.NodeConfig{}
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			_, url := testutil.NewElectrumBridge(t, h.nodes[id])
			indexers[id] = daemon.NodeConfig{Kind: "electrum", URL: url}
		}
		for _, name := range []string{"maker", "taker"} {
			cfg := h.configs[name]
			cfg.Nodes = indexers
			h.configs[name] = cfg
			h.restart(name)
		}
	}
	return h
}
func (h *reviewedTradeFixture) restart(name string) {
	h.t.Helper()
	if err := h.engines[name].Close(); err != nil {
		h.t.Fatal(err)
	}
	h.engines[name] = nil
	e, err := daemon.Open(h.ctx, h.configs[name])
	if err != nil {
		h.t.Fatal(err)
	}
	h.engines[name] = e
}
func (h *reviewedTradeFixture) status(name string) *pb.Status {
	h.t.Helper()
	s, err := h.clients[name].GetStatus(h.contexts[name], &emptypb.Empty{})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}
func (h *reviewedTradeFixture) tick() {
	h.t.Helper()
	for _, name := range []string{"maker", "taker"} {
		if err := h.engines[name].Tick(h.ctx); err != nil {
			h.t.Fatal(name, err)
		}
	}
}
func (h *reviewedTradeFixture) mine(id chain.ID, n uint32) {
	h.t.Helper()
	var addr string
	if err := h.nodes[id].WithWallet("faucet").Call(h.ctx, "getnewaddress", &addr); err != nil {
		h.t.Fatal(err)
	}
	if err := h.nodes[id].Call(h.ctx, "generatetoaddress", nil, n, addr); err != nil {
		h.t.Fatal(err)
	}
}
func (h *reviewedTradeFixture) minePending() {
	h.t.Helper()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		var pool []string
		if err := h.nodes[id].Call(h.ctx, "getrawmempool", &pool); err != nil {
			h.t.Fatal(err)
		}
		if len(pool) > 0 {
			h.mine(id, 2)
		}
	}
}
func (h *reviewedTradeFixture) quote(name string, req *pb.TradeQuoteRequest) *pb.TradeQuote {
	h.t.Helper()
	req.ExpectedWallet = name
	req.ExpectedNetwork = "regtest"
	q, err := h.clients[name].QuoteTrade(h.contexts[name], req)
	if err != nil || !q.GetReady() {
		h.t.Fatal("quote", q, err)
	}
	return q
}
func (h *reviewedTradeFixture) confirm(name string, q *pb.TradeQuote) (string, *pb.ConfirmTradeRequest) {
	h.t.Helper()
	request := &pb.ConfirmTradeRequest{Token: q.Token, Revision: q.Revision, RequestId: transport.RandomID(), ExpectedWallet: name, ExpectedNetwork: "regtest"}
	got, err := h.clients[name].ConfirmTrade(h.contexts[name], request)
	if err != nil || got.GetState() != "accepted" {
		h.t.Fatal(got, err)
	}
	h.restart(name)
	retry, err := h.clients[name].ConfirmTrade(h.contexts[name], request)
	if err != nil || !proto.Equal(got, retry) {
		h.t.Fatal("restart retry", retry, err)
	}
	return got.Id, request
}

func TestRealReviewedSwapThroughTypedAPI(t *testing.T) {
	h := newReviewedTradeFixture(t)
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			before := h.status("maker")
			mq := h.quote("maker", &pb.TradeQuoteRequest{Kind: "maker", Sell: string(sell), SellAmount: 1000000, BuyAmount: 2000000, FundingFee: 6500, OwnerFeeCap: 20000})
			if !proto.Equal(before, h.status("maker")) {
				t.Fatal("quote/cancel mutated public wallet state")
			}
			if mq.PaidChain != string(sell) || mq.ReceivedChain != string(sell.Other()) || mq.PaidTotal != 1006500 || mq.Timing.FirstRevealer != "taker" {
				t.Fatal("maker economics", mq)
			}
			offerID, mrequest := h.confirm("maker", mq)
			h.tick()
			var order *pb.Offer
			for _, o := range h.status("taker").Orders {
				if o.Id == offerID {
					order = o
				}
			}
			if order == nil {
				t.Fatal("confirmed maker offer not relayed")
			}
			tq := h.quote("taker", &pb.TradeQuoteRequest{Kind: "taker", Maker: order.Maker, Id: order.Id, Sell: order.Sell, SellAmount: order.SellAmount, BuyAmount: order.BuyAmount, FundingFee: 6500, OwnerFeeCap: 20000})
			if tq.PaidChain != string(sell.Other()) || tq.ReceivedChain != string(sell) || tq.PaidTotal != 2006500 || tq.OfferEventId == "" {
				t.Fatal("taker orientation/binding", tq)
			}
			swapID, trequest := h.confirm("taker", tq)
			complete := func() bool {
				for _, name := range []string{"maker", "taker"} {
					found := false
					for _, s := range h.status(name).Swaps {
						if s.Id == swapID {
							found = s.Stage == "completed"
						}
					}
					if !found {
						return false
					}
				}
				return true
			}
			for i := 0; i < 40 && !complete(); i++ {
				h.tick()
				h.minePending()
			}
			if !complete() {
				t.Fatal("reviewed swap did not complete", h.status("maker").Swaps, h.status("taker").Swaps)
			}
			for _, name := range []string{"maker", "taker"} {
				q := mq
				req := mrequest
				if name == "taker" {
					q = tq
					req = trequest
				}
				var swap *pb.Swap
				for _, s := range h.status(name).Swaps {
					if s.Id == swapID {
						swap = s
					}
				}
				if swap == nil || swap.FundingFee != q.Fees.FundingFee || swap.OwnerFeeCap != q.Fees.OwnerFeeCap || swap.TowerPaid != 0 {
					t.Fatal("authorized policy lost", swap)
				}
				incoming := swap.Long
				spend := swap.LongSpend
				if name == "taker" {
					incoming = swap.Short
					spend = swap.ShortSpend
				}
				record, err := h.nodes[chain.ID(incoming.Chain)].Transaction(h.ctx, spend)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := contract.Parse(record.Hex)
				if err != nil {
					t.Fatal(err)
				}
				outcome := q.Outcomes[0]
				if outcome.Kind != "owner_claim" || len(tx.TxOut) != 1 || tx.TxOut[0].Value < outcome.NetMin || tx.TxOut[0].Value > outcome.NetMax {
					t.Fatal("actual receipt outside reviewed bounds", outcome, tx)
				}
				// The signed order has now become unavailable. The original request remains
				// accepted through the public API after another restart; changed terms fail.
				h.restart(name)
				result, err := h.clients[name].ConfirmTrade(h.contexts[name], req)
				if err != nil || result.GetState() != "accepted" {
					t.Fatal("completed receipt retry", result, err)
				}
				changed := proto.Clone(req).(*pb.ConfirmTradeRequest)
				changed.Revision = transport.RandomID()
				if _, err := h.clients[name].ConfirmTrade(h.contexts[name], changed); err == nil {
					t.Fatal("changed confirmation reused request identity")
				}
				t.Logf("%s %s request=%s swap=%s net=%d %s sats within [%d,%d]", sell, name, req.RequestId, swapID, tx.TxOut[0].Value, incoming.Chain, outcome.NetMin, outcome.NetMax)
			}
		})
	}
}
