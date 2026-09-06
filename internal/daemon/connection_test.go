package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
)

func TestOpenReportsEachChainOnlyAfterWalletData(t *testing.T) {
	for _, rejectWalletData := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "wallet-data-error"}[rejectWalletData], func(t *testing.T) {
			var mu sync.Mutex
			var events []string
			record := func(event string) { mu.Lock(); defer mu.Unlock(); events = append(events, event) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Method string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				id := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
				var result any = map[string]any{}
				switch request.Method {
				case "getblockchaininfo":
					result = map[string]any{"chain": "regtest", "blocks": 100, "bestblockhash": strings.Repeat("1", 64)}
				case "getblockhash":
					result = chain.Regtest.Genesis()
				case "getblockheader":
					size := 80
					if id == "blake" {
						size = 164
					}
					result = strings.Repeat("00", size)
				case "getdeploymentinfo":
					result = map[string]any{"blake2b": map[string]any{"active": true, "height": 1}}
				case "getblockcount":
					result = 100
				case "listwallets":
					result = []string{}
				case "listwalletdir":
					result = map[string]any{"wallets": []string{}}
				case "createwallet", "setlabel":
				case "getwalletinfo":
					result = map[string]any{"scanning": false}
				case "listdescriptors":
					result = map[string]any{"descriptors": []string{}}
				case "getdescriptorinfo":
					result = map[string]any{"descriptor": "test-descriptor"}
				case "getaddressinfo":
					result = map[string]any{"labels": []string{}}
				case "importdescriptors":
					result = []map[string]bool{{"success": true}}
				case "listunspent":
					record("wallet-" + id)
					if rejectWalletData {
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": -1, "message": "wallet data unavailable"}})
						return
					}
					result = []any{}
				default:
					t.Errorf("unexpected RPC %s", request.Method)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
			}))
			defer server.Close()
			root := t.TempDir()
			password, cookie := filepath.Join(root, "password"), filepath.Join(root, "cookie")
			for path, value := range map[string]string{password: "connection-test-password", cookie: "test:test"} {
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{Mode: "trader", Network: chain.Regtest, DataDir: root, PasswordFile: password, Relays: []string{"ws://127.0.0.1:1"}, Nodes: map[chain.ID]NodeConfig{}}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				cfg.Nodes[id] = NodeConfig{Kind: "rpc", URL: server.URL + "/" + string(id), Cookie: cookie}
			}
			cfg.ChainReady = func(id chain.ID, height uint32) {
				if height != 100 {
					t.Errorf("incorrect ready height: %d", height)
				}
				record("ready-" + string(id))
			}
			e, err := Open(context.Background(), cfg)
			if e != nil {
				defer e.Close()
			}
			if rejectWalletData != (err != nil) {
				t.Fatalf("unexpected startup result: %v", err)
			}
			want := []string{"wallet-btc", "ready-btc", "wallet-blake", "ready-blake"}
			if rejectWalletData {
				want = []string{"wallet-btc"}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("readiness events: %v; want %v", events, want)
			}
		})
	}
}
