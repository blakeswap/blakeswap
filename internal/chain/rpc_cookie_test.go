package chain

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegtestCookieDiscoveryDistinguishesStoppedNodesAndUpdatesRegistration(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "nodes.json")
	t.Setenv("BLAKESWAP_REGTEST_REGISTRY", registry)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) }))
	defer server.Close()
	rpc, _ := New(BTC, server.URL, "")
	if _, err := rpc.cookiePath(ctx); err == nil || !strings.Contains(err.Error(), "endpoint is reachable") {
		t.Fatal("missing registration confused with stopped node", err)
	}
	write := func(url, path string) {
		raw, _ := json.Marshal(map[ID]localNode{BTC: {URL: url, Cookie: path}})
		if err := os.WriteFile(registry, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "first/.cookie"), filepath.Join(t.TempDir(), "new checkout/.cookie")} {
		write(server.URL, path)
		if got, err := rpc.cookiePath(ctx); err != nil || got != path {
			t.Fatal("did not resolve current registration", got, err)
		}
	}
	write(server.URL+"/different", "/incorrect/.cookie")
	if _, err := rpc.cookiePath(ctx); err == nil {
		t.Fatal("borrowed a different endpoint's cookie")
	}
	rpc.Cookie = "/explicit/.cookie"
	if got, err := rpc.cookiePath(ctx); err != nil || got != rpc.Cookie {
		t.Fatal("overrode explicit path", err)
	}
	rpc.Cookie = ""
	rpc.Network = Mainnet
	if _, err := rpc.cookiePath(ctx); err == nil {
		t.Fatal("discovered credentials on a public network")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	listener.Close()
	rpc, _ = New(Blake, endpoint, "")
	if _, err := rpc.cookiePath(ctx); err == nil || !strings.Contains(err.Error(), "not reachable") || !strings.Contains(err.Error(), "make regtest-blake") {
		t.Fatal("incorrect stopped-node error", err)
	}
}

func TestRPCConfirmedReceiptsIncludeSpentAndImmatureCoinbase(t *testing.T) {
	rpc := fakeRPC(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		if method != "listreceivedbyaddress" || len(params) != 5 || string(params[0]) != "1" || string(params[4]) != "true" {
			t.Fatal("wrong receipt query", method, params)
		}
		return []map[string]any{{"address": "receive", "amount": 0.1}}, nil
	})
	if used, err := rpc.ConfirmedReceived(context.Background(), "receive"); err != nil || !used {
		t.Fatal("confirmed receipt missed", err)
	}
}
