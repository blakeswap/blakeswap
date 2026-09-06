package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFailoverIncrementalScanKeepsHealthyEndpointAvailable(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, progress := range []bool{false, true} {
			t.Run(fmt.Sprintf("local=%t/progress=%t", local, progress), func(t *testing.T) {
				var blocked atomic.Bool
				blocked.Store(true)
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
						result = 5
					case "getblockhash":
						var height uint32
						_ = json.Unmarshal(req.Params[0], &height)
						stop := uint32(1)
						if progress {
							stop = 3
						}
						if height == stop && blocked.CompareAndSwap(true, false) {
							<-r.Context().Done()
							return
						}
						result = fmt.Sprintf("%064d", height)
					case "getblock":
						result = map[string]any{"tx": []any{}}
					case "gettxspendingprevout":
						result = []any{}
					default:
						t.Errorf("unexpected RPC %s", req.Method)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": 1})
				}))
				defer server.Close()
				cookie := filepath.Join(t.TempDir(), "cookie")
				if err := os.WriteFile(cookie, []byte("test:private"), 0600); err != nil {
					t.Fatal(err)
				}
				rpc, err := New(BTC, server.URL, cookie)
				if err != nil {
					t.Fatal(err)
				}
				p := testPool(rpc)
				p.id = BTC
				p.active = 0
				p.generation = 1
				p.entries[0].validated = true
				p.attemptBudget = 200 * time.Millisecond
				scanner := &Scanner{RPC: rpc, cursor: 100, interest: "previous interest", confirmed: map[string]Observation{}}
				var scan SpendScanner = &failoverScanner{pool: p, scanners: []SpendScanner{scanner}}
				if local {
					p.entries[0].scanner = scanner
					scan = p
				}
				outer, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				observations, err := scan.Scan(outer, 1, []string{fmt.Sprintf("%064d:0", 10)})
				if err == nil || observations != nil || outer.Err() != nil {
					t.Fatal("partial scan accepted or outer deadline consumed", err)
				}
				status := p.Status()
				if !progress {
					if status.Endpoints[0].RetryAfter == 0 || status.Endpoints[0].Error == "" {
						t.Fatal("stalled scan lost failure/backoff", err, status)
					}
					return
				}
				if scanner.cursor != 2 || status.Endpoints[0].RetryAfter != 0 || status.Endpoints[0].Error != "" || p.Generation() != 1 {
					t.Fatal("progressing scan poisoned endpoint", err, status, scanner.cursor)
				}
				if height, err := p.Height(context.Background()); err != nil || height != 5 {
					t.Fatal("tower catch-up blocked wallet refresh", height, err)
				}
				observations, err = scan.Scan(context.Background(), 1, []string{fmt.Sprintf("%064d:0", 10)})
				if err != nil || observations == nil || scanner.cursor != 5 || p.Generation() != 1 {
					t.Fatal("retained scan did not finish on same source", err, scanner.cursor)
				}
			})
		}
	}
}
