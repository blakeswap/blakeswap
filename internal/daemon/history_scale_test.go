package daemon

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// A physical selected-kind query workload, separate from the existing complete
// backup/import resource fixture. Every record is written to an encrypted vault;
// counters never stand in for retained bytes. No network or user keys are used.
func TestHistoryPhysicalQueriesAndSettlementProgress(t *testing.T) {
	if os.Getenv("BLAKESWAP_HISTORY_SCALE") != "1" {
		t.Skip("explicit physical query resource measurement")
	}
	records := 95000
	if raw := os.Getenv("BLAKESWAP_HISTORY_SCALE_RECORDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 100 {
			t.Fatal("invalid physical row count")
		}
		records = n
	}
	ctx := context.Background()
	e, b, request := sendFixture(t)
	e.Config.Name = "physical-query"
	e.s.Network = chain.Regtest
	var broadcasts atomic.Uint64
	var expectedRaw string
	b.broadcast = func(raw string) (string, error) {
		if expectedRaw != "" && raw != expectedRaw {
			return "", fmt.Errorf("saved settlement bytes changed")
		}
		var saved State
		if _, err := e.vault.Load(&saved); err != nil {
			return "", err
		}
		if saved.Sends[request.ID] == nil || saved.Sends[request.ID].Raw != raw {
			return "", fmt.Errorf("settlement retry was not durably retained")
		}
		broadcasts.Add(1)
		return "", nil
	}
	raw, _ := json.Marshal(request)
	if _, err := e.Command(ctx, Request{Method: "wallet.send", Params: raw}); err != nil {
		t.Fatal(err)
	}
	expectedRaw = e.s.Sends[request.ID].Raw
	// Completed own orders include signed sources and query metadata. They remain
	// cold during the workload, unlike the one deliberately active saved payment.
	var orders []storage.ArchiveRecord
	now := time.Now().Unix()
	for i := 0; i < 1001; i++ {
		o := protocol.Offer{ID: fmt.Sprintf("%064x", i+1), Network: chain.Regtest, Maker: e.identity.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Status: "filled", Expires: now + 3600}
		event, err := e.signOffer(o, nostr.Timestamp(now))
		if err != nil {
			t.Fatal(err)
		}
		for kind, value := range map[string]any{"offers": event, "order_records": OrderRecord{Offer: o, EventID: event.ID.Hex()}} {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			orders = append(orders, storage.ArchiveRecord{Kind: kind, ID: o.ID, Data: data})
		}
	}
	produce := func(write func(storage.ArchiveRecord) error) error {
		for i := 0; i < records; i++ {
			txid := fmt.Sprintf("%064x", i+1)
			id := "receive/" + txid + "/0"
			a := Activity{Version: 1, ID: id, Wallet: e.Config.Name, Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Direction: "incoming", Movement: true, TxID: txid, Variants: []string{txid}, Status: "confirmed", Principal: 100000, Amount: 100000, CreatedAt: 100 + int64(i/3), Observations: []ActivityObservation{{TxID: txid, Status: "confirmed", Height: 100, BlockHash: txid, Source: "configured-endpoint", Generation: 1}}}
			for j := 0; j < 4; j++ {
				a.History = append(a.History, ActivityOutcome{Status: "unknown", TxID: txid, Amount: 100000, BlockHash: txid, Source: "configured-endpoint", Generation: 1})
			}
			data, err := json.Marshal(a)
			if err != nil {
				return err
			}
			err = write(storage.ArchiveRecord{Kind: "activities", ID: id, Data: data})
			clear(data)
			if err != nil {
				return err
			}
		}
		for i := 0; i < records*2; i++ {
			data, _ := json.Marshal(fmt.Sprintf("%064x", i+1))
			if err := write(storage.ArchiveRecord{Kind: "seen", ID: e.identity.Public().Hex() + ":" + fmt.Sprintf("%064x", i+1), Data: data}); err != nil {
				return err
			}
		}
		for _, r := range orders {
			if err := write(r); err != nil {
				return err
			}
		}
		return nil
	}
	stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
	if err := produce(func(r storage.ArchiveRecord) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		stats.Count++
		stats.Bytes += uint64(len(raw) + 1)
		stats.Kinds[r.Kind]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.s.Version = 2
	e.s.Capacity = &CapacityRecord{Archived: stats}
	v, err := storage.Open(filepath.Join(t.TempDir(), "large-history.db"), []byte("private-history-scale-credential"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	historyScalePhase(t, v.PrivateDirectory(), "write_real_archive", func() {
		if err := v.ImportArchive(ctx, e.s, stats, produce); err != nil {
			t.Fatal(err)
		}
	})
	e.vault = v
	orders = nil
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	active, _ := json.Marshal(e.s)
	t.Logf("physical_archive_records=%d physical_archive_bytes=%d active_checkpoint_bytes=%d activity_rows=%d unrelated_seen_rows=%d closed_orders=1001", stats.Count, stats.Bytes, len(active), records, records*2)
	if records >= 95000 && stats.Bytes+uint64(len(active)) <= 256<<20 {
		t.Fatal("physical vault did not exceed old whole-wallet query ceiling")
	}
	clear(active)
	e.relayCancel = func() {}
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	e.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	stop, done, started := make(chan struct{}), make(chan error, 1), make(chan struct{})
	var ticks, maxTick, maxStatus atomic.Uint64
	updateMax := func(value *atomic.Uint64, n uint64) {
		for old := value.Load(); n > old; old = value.Load() {
			if value.CompareAndSwap(old, n) {
				return
			}
		}
	}
	go func() {
		first := true
		for {
			e.mu.Lock()
			e.s.Sends[request.ID].LastAttempt = 0
			e.mu.Unlock()
			// Eligibility is advanced only in this deterministic retry fixture. The
			// production thirty-second retry rule and signed bytes remain unchanged.
			before := broadcasts.Load()
			began := time.Now()
			err := e.Tick(ctx)
			updateMax(&maxTick, uint64(time.Since(began)))
			if err != nil {
				if first {
					close(started)
				}
				done <- err
				return
			}
			if broadcasts.Load() != before+1 {
				if first {
					close(started)
				}
				done <- fmt.Errorf("saved payment did not retry in its eligible tick")
				return
			}
			ticks.Add(1)
			began = time.Now()
			status := e.Status()
			updateMax(&maxStatus, uint64(time.Since(began)))
			if len(status.Sends) != 1 {
				if first {
					close(started)
				}
				done <- fmt.Errorf("bounded status lost active payment")
				return
			}
			if first {
				close(started)
				first = false
			}
			select {
			case <-stop:
				done <- nil
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	<-started
	joined := false
	defer func() {
		if !joined {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	// The active payment adds its own Blake receipt; this query selects the
	// retained BTC population while that independent settlement remains live.
	q := ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Kind: "receive", Chain: chain.BTC, Limit: 37}
	var first ActivityPage
	before := ticks.Load()
	historyScalePhase(t, v.PrivateDirectory(), "first_activity_page", func() {
		// Match the existing typed API/native deadline instead of hiding a slow
		// first page behind this opt-in test's longer overall process timeout.
		queryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		raw, _ := json.Marshal(q)
		result, err := e.historyCommand(queryCtx, Request{Method: "activity.list", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		first = result.(ActivityPage)
	})
	during := ticks.Load() - before
	if first.Total != uint32(records) || len(first.Records) != 37 || first.Records[0].ID != fmt.Sprintf("receive/%064x/0", records) {
		t.Fatalf("physical history ordering/count lost: total=%d rows=%d first=%s expected=%s", first.Total, len(first.Records), first.Records[0].ID, fmt.Sprintf("receive/%064x/0", records))
	}
	if during == 0 {
		t.Fatal("no settlement tick completed while full history result was built")
	}
	t.Logf("activity_collection_concurrent_ticks=%d", during)
	q.Snapshot = first.Snapshot
	q.Cursor = uint32(records - 37)
	historyScalePhase(t, v.PrivateDirectory(), "frozen_last_page_and_csv", func() {
		page := durableActivityQuery(t, e, q)
		if len(page.Records) != 37 || page.Records[len(page.Records)-1].ID != "receive/"+fmt.Sprintf("%064x", 1)+"/0" {
			t.Fatal("cold ending boundary lost")
		}
		raw, _ := json.Marshal(q)
		result, err := e.historyCommand(ctx, Request{Method: "activity.export", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		csvRows, err := csv.NewReader(strings.NewReader(result.(ActivityExport).CSV)).ReadAll()
		if err != nil || len(csvRows) != 37 {
			t.Fatal("physical CSV mismatch", err)
		}
		for i, row := range page.Records {
			if csvRows[i][0] != row.ID {
				t.Fatal("physical CSV order differs")
			}
		}
	})
	historyScalePhase(t, v.PrivateDirectory(), "closed_own_market_page", func() {
		raw, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Owner: "mine", Status: "filled", Limit: 1})
		result, err := e.historyCommand(ctx, Request{Method: "market.list", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		page := result.(MarketPage)
		if page.Total != 1001 || len(page.Records) != 1 {
			t.Fatal("closed own archive query lost orders")
		}
	})
	prior := historyScratch(t, e)
	historyScalePhase(t, v.PrivateDirectory(), "cancel_new_full_query", func() {
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		timer := time.AfterFunc(25*time.Millisecond, cancel)
		defer timer.Stop()
		raw, _ := json.Marshal(ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Kind: "receive", Chain: chain.BTC, Limit: 1})
		if result, err := e.historyCommand(cancelCtx, Request{Method: "activity.list", Params: raw}); err == nil || result != nil {
			t.Fatal("canceled physical query succeeded")
		}
	})
	if historyScratch(t, e) != prior {
		t.Fatal("canceled physical result retained files")
	}
	close(stop)
	err = <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("query_phase_ticks=%d total_signed_broadcasts=%d max_tick=%s max_status=%s", ticks.Load(), broadcasts.Load(), time.Duration(maxTick.Load()), time.Duration(maxStatus.Load()))
	if time.Duration(maxTick.Load()) > 2*time.Second || time.Duration(maxStatus.Load()) > 250*time.Millisecond {
		t.Fatal("settlement/status latency exceeded existing local workload budgets")
	}
	var saved State
	if _, err := v.Load(&saved); err != nil || saved.Sends[request.ID] == nil || saved.Sends[request.ID].Raw != expectedRaw {
		t.Fatal("saved settlement identity lost", err)
	}
	got, err := v.ArchiveStats()
	if err != nil || got.Count != stats.Count || got.Bytes != stats.Bytes {
		t.Fatal("read-only queries changed cold history", err)
	}
	e.nodes = nil // All backends above are disposable in-memory fixtures.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if historyScratch(t, e) != 0 {
		t.Fatal("close retained physical query results")
	}
}

func historyScalePhase(t *testing.T, root, name string, run func()) {
	t.Helper()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	before := m.HeapAlloc
	var heap, disk, logical atomic.Uint64
	heap.Store(before)
	measureDisk := func() {
		var d, l uint64
		seen := map[[2]uint64]bool{}
		_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				key := [2]uint64{uint64(st.Dev), st.Ino}
				if seen[key] {
					return nil
				}
				seen[key] = true
				d += uint64(st.Blocks) * 512
			}
			l += uint64(info.Size())
			return nil
		})
		if d > disk.Load() {
			disk.Store(d)
		}
		if l > logical.Load() {
			logical.Store(l)
		}
	}
	measureDisk()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				n++
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > heap.Load() {
					heap.Store(m.HeapAlloc)
				}
				if n%10 == 0 {
					measureDisk()
				}
			}
		}
	}()
	start := time.Now()
	defer func() {
		close(stop)
		<-done
		measureDisk()
		runtime.ReadMemStats(&m)
		t.Logf("phase=%s elapsed=%s heap_before=%d sampled_peak_heap=%d heap_after=%d sampled_summed_file_allocation=%d sampled_logical_disk=%d", name, time.Since(start), before, heap.Load(), m.HeapAlloc, disk.Load(), logical.Load())
	}()
	run()
}
