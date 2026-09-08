// Private-node acceptance. Runs only when the explicit fixture environment is set.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// Disconnecting/reconsidering 144 Blake blocks exceeds the ordinary 15s wallet
// RPC deadline on the private fixture. Only these test administration operations
// get a separate budget; wallet observation and settlement deadlines stay intact.
func archiveFixtureBlockCommand(t *testing.T, node *chain.RPC, method, hash string) error {
	t.Helper()
	if method != "invalidateblock" && method != "reconsiderblock" {
		return fmt.Errorf("unsupported fixture operation %s", method)
	}
	started := time.Now()
	defer func() { t.Logf("%s fixture %s block=%s elapsed=%s", node.ID, method, hash, time.Since(started)) }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": []string{hash}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, node.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	cookie, err := os.ReadFile(node.Cookie)
	if err != nil {
		return err
	}
	defer clear(cookie)
	user, password, ok := strings.Cut(strings.TrimSpace(string(cookie)), ":")
	if !ok {
		return fmt.Errorf("invalid private fixture cookie")
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var reply struct {
		Error *chain.RPCError `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return fmt.Errorf("fixture %s HTTP %d: invalid response", method, resp.StatusCode)
	}
	if reply.Error != nil {
		return reply.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fixture %s HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// Use a fresh private destination: an old source may already own cold records,
// and overwriting only its active checkpoint would not model the real installer.
func restoreArchivedPortableFresh(t *testing.T, h *harness, name, path string) {
	t.Helper()
	h.offline(name)
	var archive recoveryArchive
	if err := storage.ReadPortable(h.ctx, path, []byte(testPortablePassword), &archive); err != nil {
		t.Fatal(err)
	}
	if archive.FormatVersion != 1 || len(archive.Wallets) != 1 {
		t.Fatal("invalid isolated archive")
	}
	state := archive.Wallets[0].Networks[chain.Regtest]
	if err := PrepareRecovery(state, archive.CreatedAt, false); err != nil {
		t.Fatal(err)
	}
	cfg := h.configs[name]
	cfg.DataDir = t.TempDir()
	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	vault, err := storage.Open(filepath.Join(cfg.DataDir, "state.db"), bytes.TrimSpace(password))
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(state); err != nil {
		_ = vault.Close()
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	h.configs[name] = cfg
	h.online(name)
	if h.engines[name].Status().Recovery.State != "recovering" {
		t.Fatal("restored archive opened before positive reconciliation")
	}
}

func TestRealArchiveBoundaryPaymentReorgAndPortableRecovery(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			h := newHarness(t, 0)
			e := h.engines["maker"]
			var inputs []CoinOutpoint
			for _, coin := range e.knownCoins(id) {
				inputs = append(inputs, CoinOutpoint{coin.TxID, coin.Vout})
			}
			request := SendRequest{ID: transport.RandomID(), Chain: id, Destination: h.engines["taker"].addresses[id], Amount: 1000000, Fee: 1, MaxFee: 20000, Inputs: inputs, ExpectedNetwork: "regtest"}
			raw, _ := json.Marshal(request)
			original, err := e.sendCoins(h.ctx, raw)
			if err != nil || original.Submitted {
				t.Fatal("expected preserved below-relay signed variant", err)
			}
			tickUntilConnected(t, e)
			bumped, err := e.bumpSend(h.ctx, BumpRequest{ID: request.ID, Kind: "send", Fee: 6500, ExpectedTxID: original.TxID})
			if err != nil || bumped.Error != "" {
				t.Fatal(err, bumped.Error)
			}
			expected := append([]SignedVariant(nil), e.s.Sends[request.ID].History...)
			expectedRaw := e.s.Sends[request.ID].Raw
			before := snapshotRecoveryArchive(t, h, "maker")
			h.mine(id, archiveSettlementDepth)
			tickUntilConnected(t, e)
			if e.s.Sends[request.ID] != nil || e.s.Capacity.Archived.Kinds["sends"] != 1 {
				t.Fatal("deep positive payment did not cross archive boundary")
			}
			after := snapshotRecoveryArchive(t, h, "maker")
			assertLineage := func(e *Engine) {
				state, err := e.BackupSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				complete, err := CompleteState(state)
				if err != nil {
					t.Fatal(err)
				}
				send := complete.Sends[request.ID]
				if send == nil || send.Raw != expectedRaw || send.MaxFee != 20000 || len(send.History) != len(expected) {
					t.Fatal("archive lost signed payment authority")
				}
				for i, v := range expected {
					if send.History[i].Raw != v.Raw || send.History[i].TxID != v.TxID || send.History[i].Fee != v.Fee {
						t.Fatal("signed variant bytes or fee changed")
					}
				}
			}
			for _, backup := range []string{before, after} {
				restoreArchivedPortableFresh(t, h, "maker", backup)
				e = h.engines["maker"]
				tickUntilConnected(t, e)
				if e.Status().Recovery.State != "ready" {
					t.Fatal("restored payment lacks positive readiness", e.Status().Recovery.State)
				}
				assertLineage(e)
				if _, err := e.sendCoins(h.ctx, raw); err != nil {
					t.Fatal("exact send retry failed after portable compaction", err)
				}
			}
			// The last restored snapshot must itself be cold before the actual fork.
			if e.s.Sends[request.ID] != nil || e.s.Capacity.Archived.Kinds["recovery_sends"] != 1 {
				t.Fatal("restored source/origin did not archive together")
			}
			record, err := h.nodes[id].Transaction(h.ctx, bumped.TxID)
			if err != nil || record.BlockHash == "" {
				t.Fatal("payment lacks actual confirmed block", err)
			}
			// Register exact-block cleanup before the request: a client timeout can
			// occur after the node has already applied the invalidation.
			defer func() {
				if err := archiveFixtureBlockCommand(t, h.nodes[id], "reconsiderblock", record.BlockHash); err != nil {
					t.Error(err)
					return
				}
				var header struct{ Confirmations int }
				if err := h.nodes[id].Call(h.ctx, "getblockheader", &header, record.BlockHash, true); err != nil || header.Confirmations < 1 {
					t.Errorf("fixture cleanup did not restore exact block %s: confirmations=%d error=%v", record.BlockHash, header.Confirmations, err)
				}
			}()
			if err = archiveFixtureBlockCommand(t, h.nodes[id], "invalidateblock", record.BlockHash); err != nil {
				t.Fatal(err)
			}
			tickUntilConnected(t, e)
			if e.CanChangeNetwork() == nil || e.s.Capacity == nil || !e.s.Capacity.Invalidated["send/"+request.ID] {
				t.Fatal("known archive-crossing reorg did not hold monitoring")
			}
			if e.s.Sends[request.ID] == nil || e.s.Recovery == nil || !e.s.Recovery.Sends[request.ID] {
				t.Fatal("reactivation lost payment or restore-origin gate")
			}
			assertLineage(e)
			h.offline("maker")
			if err := CheckStoredNetwork(h.configs["maker"]); err == nil {
				t.Fatal("saved offline guard lost archive reorg hold")
			}
			h.online("maker")
			e = h.engines["maker"]
			if e.CanChangeNetwork() == nil {
				t.Fatal("reopen released unconfirmed payment obligation")
			}
			tickUntilConnected(t, e)
			if e.s.Sends[request.ID] == nil || e.s.Sends[request.ID].Confirmations != 0 || e.CanChangeNetwork() == nil {
				t.Fatal("mempool evidence released reorg hold")
			}
			assertLineage(e)
			// Deep block disconnection need not re-add its old transactions to the
			// mempool. Permit the normal 30s exact retry before mining, and prove
			// actual admission instead of mistaking an orphan's count=0 for it.
			retryCtx, cancelRetry := context.WithTimeout(h.ctx, 45*time.Second)
			defer cancelRetry()
			for {
				if err := e.Tick(retryCtx); err != nil {
					t.Fatal("retry observation failed", err)
				}
				if e.s.Sends[request.ID] == nil || e.s.Sends[request.ID].Confirmations != 0 || !e.s.Capacity.Invalidated["send/"+request.ID] || e.CanChangeNetwork() == nil {
					t.Fatal("unconfirmed automatic retry cleared the archive/network obligation")
				}
				err := h.nodes[id].Call(retryCtx, "getmempoolentry", nil, bumped.TxID)
				if err == nil {
					break
				}
				var rpcErr *chain.RPCError
				if !errors.As(err, &rpcErr) || rpcErr.Code != -5 {
					t.Fatal("cannot verify exact retry in the fixture mempool", err)
				}
				select {
				case <-retryCtx.Done():
					t.Fatal("ordinary signed retry did not reach the actual mempool", retryCtx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
			cancelRetry()
			assertLineage(e)
			h.mine(id, uint32(chain.Regtest.Confirmations()))
			confirmed, err := h.nodes[id].Transaction(h.ctx, bumped.TxID)
			if err != nil || confirmed.Confirmations != chain.Regtest.Confirmations() {
				t.Fatal("fixture did not positively reconfirm the exact payment twice", confirmed.Confirmations, err)
			}
			tickUntilConnected(t, e)
			if e.s.Capacity.Invalidated["send/"+request.ID] {
				t.Fatal("positive current confirmation did not clear archive invalidation")
			}
			if got := e.Status().Recovery.State; got != "ready" {
				t.Fatal("positive current confirmation did not restore recovery readiness", got)
			}
			if e.s.Sends[request.ID].Confirmations != chain.Regtest.Confirmations() || e.CanChangeNetwork() == nil {
				t.Fatal("two-confirmation payment bypassed the six-confirmation network guard")
			}
			h.mine(id, uint32(6-chain.Regtest.Confirmations()))
			confirmed, err = h.nodes[id].Transaction(h.ctx, bumped.TxID)
			if err != nil || confirmed.Confirmations != 6 {
				t.Fatal("fixture did not positively confirm the exact payment six times", confirmed.Confirmations, err)
			}
			tickUntilConnected(t, e)
			if got := e.s.Sends[request.ID].Confirmations; got != 6 {
				t.Fatal("payment lacks six positive confirmations", got)
			}
			if err := e.CanChangeNetwork(); err != nil {
				t.Fatal("six-confirmation payment did not release network guard", err)
			}
			assertLineage(e)
			t.Logf("%s retained both signed variants across before/after portable recovery and deep archive-boundary reorg", id)
		})
	}
}
