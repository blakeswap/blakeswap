package desktop

import (
	"context"
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

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type portableScaleCounter uint64

func (c *portableScaleCounter) Write(p []byte) (int, error) {
	*c += portableScaleCounter(len(p))
	return len(p), nil
}
func portableScalePhase(t *testing.T, name string, run func()) {
	t.Helper()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	before := stats.HeapAlloc
	var peak, diskPeak, logicalPeak atomic.Uint64
	peak.Store(before)
	diskRoot := filepath.Dir(t.TempDir())
	measureDisk := func() {
		var allocated, logical uint64
		seen := map[[2]uint64]bool{}
		_ = filepath.Walk(diskRoot, func(_ string, info os.FileInfo, err error) error {
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				key := [2]uint64{uint64(st.Dev), st.Ino}
				if seen[key] {
					return nil
				}
				seen[key] = true
				allocated += uint64(st.Blocks) * 512
			}
			logical += uint64(info.Size())
			return nil
		})
		if allocated > diskPeak.Load() {
			diskPeak.Store(allocated)
		}
		if logical > logicalPeak.Load() {
			logicalPeak.Store(logical)
		}
	}
	measureDisk()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		ticks := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ticks++
				if ticks%10 == 0 {
					measureDisk()
				}
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for previous := peak.Load(); m.HeapAlloc > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	start := time.Now()
	defer func() {
		close(stop)
		<-done
		measureDisk()
		runtime.ReadMemStats(&stats)
		t.Logf("phase=%s elapsed=%s heap_before=%d sampled_peak_heap=%d heap_after=%d sampled_peak_allocated_disk=%d sampled_peak_logical_disk=%d", name, time.Since(start), before, peak.Load(), stats.HeapAlloc, diskPeak.Load(), logicalPeak.Load())
	}()
	run()
}

// Explicit opt-in physical resource fixture. It exercises the actual v1 reader,
// private v2 staging, complete encrypted export and published restore. Its large
// retained byte strings are format/continuation payloads, never broadcast chain
// fixtures; protocol-specific growth bounds have separate tests.
func TestPortablePhysicalLargeHistoryAndCoreContinuation(t *testing.T) {
	if os.Getenv("BLAKESWAP_PORTABLE_SCALE") != "1" {
		t.Skip("explicit physical resource measurement")
	}
	records := 95000
	if raw := os.Getenv("BLAKESWAP_SCALE_RECORDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 100 {
			t.Fatal("invalid scale record count")
		}
		records = n
	}
	core := max(1, records/47)
	ctx := context.Background()
	manifest := portableManifest(t)
	entry := &manifest.Wallets[0]
	state := entry.Networks[chain.Regtest]
	entry.Networks = map[chain.Network]*daemon.State{chain.Regtest: state}
	state.Activities = map[string]daemon.Activity{}
	state.ActivityReceipts = nil
	state.Swaps = map[string]*daemon.Swap{}
	for i := 0; i < records; i++ {
		txid := fmt.Sprintf("%064x", i+1)
		id := "receive/" + txid + "/0"
		a := daemon.Activity{Version: 1, ID: id, Wallet: entry.ID, Network: chain.Regtest, Kind: "receive", Chain: chain.BTC, Direction: "incoming", TxID: txid, Variants: []string{txid}, Status: "confirmed", Principal: 100000, Amount: 100000, Observations: []daemon.ActivityObservation{{TxID: txid, Status: "confirmed", Height: 100, BlockHash: txid, Source: "configured-endpoint", Generation: 1}}}
		for j := 0; j < 4; j++ {
			a.History = append(a.History, daemon.ActivityOutcome{Status: "unknown", TxID: txid, Amount: 100000, BlockHash: txid, Source: "configured-endpoint", Generation: 1})
		}
		state.Activities[id] = a
	}
	for i := 0; i < core; i++ {
		id := fmt.Sprintf("retained-%08d", i)
		state.Swaps[id] = &daemon.Swap{ID: id, Role: "maker", Stage: "awaiting peer evidence", SelfRefunds: []string{"original retained refund bytes"}}
	}
	var plain portableScaleCounter
	if err := storage.WriteJSONRecord(ctx, &plain, manifest); err != nil {
		t.Fatal(err)
	}
	if uint64(plain) > storage.PortableLimit {
		t.Fatal("fixture not previously accepted v1 population", plain)
	}
	t.Logf("accepted_v1_plaintext=%d activity_records=%d core_obligations=%d", plain, records, core)
	legacyPath := filepath.Join(t.TempDir(), "accepted-v1.backup")
	password := []byte("physical scale chosen portable password")
	portableScalePhase(t, "write_v1", func() {
		if err := storage.WritePortable(ctx, legacyPath, password, manifest); err != nil {
			t.Fatal(err)
		}
	})
	manifest = backupManifest{}
	entry = nil
	state = nil
	runtime.GC()
	m := installedManager(t)
	var result portableImportResult
	portableScalePhase(t, "import_v1", func() {
		var err error
		result, err = m.importPortable(ctx, portableImportRequest{Path: legacyPath, Password: string(password), Revision: m.settings.Revision})
		if err != nil {
			t.Fatal(err)
		}
	})
	root := filepath.Join(m.root, "wallets", result.ProfileID)
	_, secret, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	path := filepath.Join(root, "regtest", "state.db")
	var active daemon.State
	vault, err := storage.Open(path, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vault.Load(&active); err != nil {
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	history := active.Activities
	active.Activities = nil
	// A recorded obligation can gain additional durable transaction evidence after
	// a near-limit old backup. All bytes must remain exportable; no count heuristic
	// from old runtime admission caps can shrink this authenticated population.
	for _, swap := range active.Swaps {
		swap.ShortFunding = strings.Repeat("ab", 12000)
	}
	stats := storage.ArchiveStats{Kinds: map[string]uint64{"activities": uint64(len(history))}, Count: uint64(len(history))}
	visit := func(write func(storage.ArchiveRecord) error) error {
		for id, a := range history {
			raw, err := json.Marshal(a)
			if err != nil {
				return err
			}
			err = write(storage.ArchiveRecord{Kind: "activities", ID: id, Data: raw})
			clear(raw)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err = visit(func(record storage.ArchiveRecord) error {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		stats.Bytes += uint64(len(raw) + 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if active.Capacity == nil {
		active.Capacity = &daemon.CapacityRecord{}
	}
	active.Version = 2
	active.Capacity.Archived = stats
	nextPath := filepath.Join(root, "regtest", "growth.db")
	portableScalePhase(t, "stage_actual_cold_history", func() {
		next, err := storage.Open(nextPath, secret)
		if err != nil {
			t.Fatal(err)
		}
		err = next.ImportArchive(ctx, active, stats, visit)
		closeErr := next.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
	})
	if err = os.Rename(nextPath, path); err != nil {
		t.Fatal(err)
	}
	history = nil
	active = daemon.State{}
	runtime.GC()
	var snapshot backupManifest
	portableScalePhase(t, "source_snapshot_total", func() {
		var err error
		snapshot, err = m.backupSnapshot(ctx, result.ProfileID, false)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("source_snapshot_worker_pause=%s", snapshot.capturePause)
	output := filepath.Join(t.TempDir(), "complete-grown-v2.backup")
	portableScalePhase(t, "export_complete_v2", func() {
		if err := writeStreamManifest(ctx, output, password, snapshot); err != nil {
			t.Fatal(err)
		}
	})
	snapshot.close()
	snapshot = backupManifest{}
	runtime.GC()
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("grown_v2_ciphertext=%d retained_cold_bytes=%d", info.Size(), stats.Bytes)
	if records >= 95000 && info.Size() <= storage.PortableLimit {
		t.Fatal("fixture did not exceed old envelope after core growth")
	}
	m2 := installedManager(t)
	var restored portableImportResult
	portableScalePhase(t, "inspect_stage_and_publish_v2_restore", func() {
		var err error
		restored, err = m2.importPortable(ctx, portableImportRequest{Path: output, Password: string(password), Revision: m2.settings.Revision})
		if err != nil {
			t.Fatal(err)
		}
	})
	restoredRoot := filepath.Join(m2.root, "wallets", restored.ProfileID)
	_, key, err := readMaster(restoredRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	v, err := storage.Open(filepath.Join(restoredRoot, "regtest", "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var got daemon.State
	if _, err = v.Load(&got); err != nil {
		t.Fatal(err)
	}
	actual, err := v.ArchiveStats()
	if err != nil || actual.Count != uint64(records) || actual.Bytes != stats.Bytes || len(got.Activities) != 0 || len(got.Swaps) != core || len(got.Recovery.Swaps) != core {
		t.Fatal("complete cold/core population changed", actual, err)
	}
	for _, swap := range got.Swaps {
		if len(swap.ShortFunding) != 24000 || swap.SelfRefunds[0] != "original retained refund bytes" || !got.Recovery.Swaps[swap.ID] {
			t.Fatal("core continuation/recovery fact omitted")
		}
	}
	for _, index := range []int{1, records} {
		id := fmt.Sprintf("receive/%064x/0", index)
		record, ok, err := v.ReadArchive("activities", id)
		if err != nil || !ok {
			t.Fatal("cold boundary missing", err)
		}
		var a daemon.Activity
		if err = json.Unmarshal(record.Data, &a); err != nil || len(a.History) != 4 || a.Amount != 100000 {
			t.Fatal("cold history changed", err)
		}
	}
}
