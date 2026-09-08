package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// The real daemon and wallet worker run against disposable HTTP observation
// fixtures. This checks scheduling/IO availability, not real-chain settlement.
func snapshotWorkerEngine(t *testing.T, m *Manager) *atomic.Uint64 {
	t.Helper()
	var reads atomic.Uint64
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("snapshot:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	nodes := map[chain.ID]daemon.NodeConfig{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
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
			case "getblockchaininfo":
				result = map[string]any{"chain": "regtest", "blocks": 200, "bestblockhash": fmt.Sprintf("%064d", 200), "mediantime": time.Now().Unix()}
			case "getblockcount":
				reads.Add(1)
				result = 200
			case "getblockhash":
				var height int
				_ = json.Unmarshal(req.Params[0], &height)
				result = fmt.Sprintf("%064d", height)
				if height == 0 {
					result = chain.Regtest.Genesis()
				}
			case "getblockheader":
				size := 160
				if id == chain.Blake {
					size = 328
				}
				result = strings.Repeat("0", size)
			case "getdeploymentinfo":
				result = map[string]any{"blake2b": map[string]any{"active": true, "height": 1}}
			case "listwallets", "listunspent", "listreceivedbyaddress", "getrawmempool", "gettxspendingprevout":
				result = []any{}
			case "listwalletdir":
				result = map[string]any{"wallets": []any{}}
			case "createwallet", "setlabel":
				result = nil
			case "getwalletinfo":
				result = map[string]any{"scanning": false}
			case "listdescriptors":
				result = map[string]any{"descriptors": []any{}}
			case "getdescriptorinfo":
				var desc string
				_ = json.Unmarshal(req.Params[0], &desc)
				result = map[string]any{"descriptor": desc + "#fixture"}
			case "getaddressinfo":
				result = map[string]any{"labels": []string{"blakeswap-history-ready-v1"}}
			case "importdescriptors":
				var imports []any
				_ = json.Unmarshal(req.Params[0], &imports)
				success := make([]any, len(imports))
				for i := range success {
					success[i] = map[string]any{"success": true}
				}
				result = success
			case "getblock":
				result = map[string]any{"tx": []any{}}
			default:
				t.Errorf("unexpected snapshot fixture RPC %s", req.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": 1})
		}))
		t.Cleanup(server.Close)
		nodes[id] = daemon.NodeConfig{Kind: "rpc", URL: server.URL, Cookie: cookie}
	}
	root := filepath.Join(m.root, "wallets", "alice")
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := daemon.Open(ctx, daemon.Config{Name: "alice", Mode: "trader", Network: chain.Regtest, DataDir: filepath.Join(root, "regtest"), PasswordFile: filepath.Join(root, "vault.password"), Relays: []string{"ws://127.0.0.1:1"}, Nodes: nodes})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	m.engines["alice"] = engine
	m.runtimeCtx = ctx
	m.settings.ActiveNetwork = "regtest"
	m.settings.OnboardingStage = ""
	m.startWorkers(ctx)
	t.Cleanup(func() { m.mu.Lock(); m.closeNetwork(); m.mu.Unlock(); cancel() })
	return &reads
}

type pausedBackupView struct {
	backupSourceView
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *pausedBackupView) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	return v.backupSourceView.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
		v.once.Do(func() { close(v.entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.release:
		}
		return visit(record)
	})
}
func TestPortableCaptureWalletWorkerProgressDuringPreparationAndMaterialization(t *testing.T) {
	m := captureManagerFixture(t, true)
	reads := snapshotWorkerEngine(t, m)
	// Hold only the inactive credential source for a short, explicit preparation
	// interval. Worker refreshes must keep reaching both HTTP chain fixtures.
	root := filepath.Join(m.root, "wallets", "alice")
	_, password, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	master, err := storage.Open(filepath.Join(root, "master.db"), bytes.TrimSpace(password))
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan *portableCapture, 1)
	failed := make(chan error, 1)
	before := reads.Load()
	worker := m.workers["alice"]
	go func() {
		m.mu.Lock()
		c, err := m.captureBackupLocked(context.Background(), "alice", false)
		m.mu.Unlock()
		if err != nil {
			failed <- err
		} else {
			captured <- c
		}
	}()
	// Request an actual worker cycle directly while the lifecycle lock is busy.
	// This is the same worker channel used by ordinary explicit refreshes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := worker.check(ctx); err != nil {
		_ = master.Close()
		t.Fatal("worker stopped during inactive credential preparation", err)
	}
	if reads.Load() <= before {
		_ = master.Close()
		t.Fatal("no live chain work during preparation")
	}
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	var capture *portableCapture
	select {
	case capture = <-captured:
	case err := <-failed:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer capture.staging.close()
	defer capture.closeSources()
	view := &pausedBackupView{backupSourceView: capture.sources[0].page, entered: make(chan struct{}), release: make(chan struct{})}
	capture.sources[0].page = view
	materialized := make(chan error, 1)
	go func() { snapshot, err := capture.materialize(); defer snapshot.close(); materialized <- err }()
	select {
	case <-view.entered:
	case err := <-materialized:
		t.Fatal("did not reach slow archive callback", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	before = reads.Load()
	m.mu.Lock()
	worker = m.workers["alice"]
	m.mu.Unlock()
	if worker == nil {
		close(view.release)
		t.Fatal("capture did not resume wallet worker")
	}
	if _, err := worker.check(ctx); err != nil {
		close(view.release)
		t.Fatal("worker blocked by private materialization", err)
	}
	if reads.Load() <= before {
		close(view.release)
		t.Fatal("no live chain work during materialization")
	}
	t.Logf("all-source capture pause=%s; actual worker cycles advanced during credential preparation and blocked archive callback", capture.manifest.capturePause)
	close(view.release)
	if err := <-materialized; err != nil {
		t.Fatal(err)
	}
}
