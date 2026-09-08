package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
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
	portableScaleCores(t, state, core)
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
	funding := portableScaleGrowFunding(t, &active)
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
	active.Version = daemon.StateVersion
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
		want, found := funding[swap.ID]
		if !found || want.Bytes < 24000 || len(swap.ShortFunding) != want.Bytes || sha256.Sum256([]byte(swap.ShortFunding)) != want.Digest || swap.SelfRefunds[0] != "original retained refund bytes" || !got.Recovery.Swaps[swap.ID] {
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

// This replaces the manifest's single sample with an explicit population, before
// any installation. Each whole child has its own signed parent and disjoint
// retained input; no index helper invents custody for incomplete records.
func portableScaleCores(t *testing.T, state *daemon.State, count int) {
	t.Helper()
	state.Swaps = map[string]*daemon.Swap{}
	state.ParentOrders, state.FillRecords, state.FundingFees, state.CoinReservations = nil, nil, nil, nil
	maker := fixtureIdentity(t, state.Network, state.Mnemonic)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%064x", i+1)
		child := fixtureSwap(t, state.Network, state.Mnemonic, "maker")
		_, event := fixtureOffer(t, state.Network, fmt.Sprintf("%064x", count+i+1), maker)
		child.ID, child.Request.ID, child.Request.OfferEvent = id, id, event
		child.Stage, child.SelfRefunds = "awaiting peer evidence", []string{"original retained refund bytes"}
		state.Swaps[id] = child
		fixtureMakerCustody(t, state, child, true)
	}
	fixtureIndexSwaps(t, state)
}

type portableFundingExpectation struct {
	Bytes  int
	Digest [32]byte
}

// Many ordinary change outputs provide the retained-byte growth of the original
// format fixture without putting arbitrary hex in an authenticated funding field.
// Inputs/keys here are synthetic and never sent to a node. The exact transaction
// still spends its retained input, pays the agreed HTLC and fee, and passes the
// witness script interpreter after signing all of its actual outputs.
func portableScaleGrowFunding(t *testing.T, state *daemon.State) map[string]portableFundingExpectation {
	t.Helper()
	expected := map[string]portableFundingExpectation{}
	for id, child := range state.Swaps {
		fill := state.FillRecords[id]
		if child.Role != "maker" || child.Terms == nil || child.Terms.Short.Chain != chain.BTC || fill == nil || len(fill.Inputs) != 1 || !fill.Allocation.EverCommitted {
			t.Fatal("scale growth requires exact committed BTC maker custody")
		}
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		_, script, err := wallet.Address(key.PubKey())
		if err != nil {
			t.Fatal(err)
		}
		const changes = 381
		amount := child.Terms.Short.Amount + fill.FundingPolicy.FundingFee + changes*contract.Dust
		coin := chain.UTXO{TxID: fill.Inputs[0].TxID, Vout: fill.Inputs[0].Vout, Amount: chain.Coins(amount), Script: hex.EncodeToString(script), Confirmations: 6}
		tx, err := contract.Fund(child.Terms.Short, []chain.UTXO{coin}, key, fill.FundingPolicy.FundingFee)
		if err != nil || len(tx.TxOut) != 2 {
			t.Fatal("valid initial funding", err)
		}
		tx.TxOut[1].Value = contract.Dust
		for n := 1; n < changes; n++ {
			tx.AddTxOut(wire.NewTxOut(contract.Dust, append([]byte(nil), script...)))
		}
		pub := key.PubKey().SerializeCompressed()
		code, err := txscript.NewScriptBuilder().AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).AddData(btcutil.Hash160(pub)).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG).Script()
		if err != nil {
			t.Fatal(err)
		}
		spent := []*wire.TxOut{wire.NewTxOut(amount, script)}
		digest, err := contract.Digest(chain.BTC, tx, 0, code, spent)
		if err != nil {
			t.Fatal(err)
		}
		tx.TxIn[0].Witness = wire.TxWitness{append(ecdsa.Sign(key, digest).Serialize(), byte(txscript.SigHashAll)), pub}
		fetcher := txscript.NewCannedPrevOutputFetcher(script, amount)
		vm, err := txscript.NewEngine(script, tx, 0, txscript.StandardVerifyFlags, nil, txscript.NewTxSigHashes(tx, fetcher), amount, fetcher)
		if err != nil {
			t.Fatal(err)
		}
		if err = vm.Execute(); err != nil {
			t.Fatal("grown funding signature is invalid", err)
		}
		var total int64
		for _, output := range tx.TxOut {
			total += output.Value
		}
		if amount-total != fill.FundingPolicy.FundingFee {
			t.Fatal("growth changed exact authorized funding fee")
		}
		child.ShortFunding = contract.Hex(tx)
		child.Short.TxID, child.Short.Vout = tx.TxHash().String(), 0
		if state.FillKeys == nil {
			state.FillKeys = map[string]string{}
		}
		index := fmt.Sprintf("funding/%s/%s", child.Short.Chain, child.Short.TxID)
		if prior := state.FillKeys[index]; prior != "" && prior != id {
			t.Fatal("grown funding identity belongs to another child")
		}
		state.FillKeys[index] = id
		if len(child.ShortFunding) < 24000 {
			t.Fatal("valid funding did not preserve original retained-byte growth")
		}
		expected[id] = portableFundingExpectation{len(child.ShortFunding), sha256.Sum256([]byte(child.ShortFunding))}
	}
	return expected
}

func TestPortableScaleFixtureHasValidExactCustodyAndSignedGrowth(t *testing.T) {
	manifest := portableManifest(t)
	state := manifest.Wallets[0].Networks[chain.Regtest]
	portableScaleCores(t, state, 2)
	if len(state.Swaps) != 2 || len(state.FillRecords) != 2 || len(state.ParentOrders) != 2 {
		t.Fatal("explicit fixture population changed")
	}
	if err := daemon.ValidateCompleteFillState(state); err != nil {
		t.Fatal("initial complete custody", err)
	}
	before, err := json.Marshal(state.ParentOrders)
	if err != nil {
		t.Fatal(err)
	}
	funding := portableScaleGrowFunding(t, state)
	if err := daemon.ValidateCompleteFillState(state); err != nil {
		t.Fatal("grown complete custody", err)
	}
	after, _ := json.Marshal(state.ParentOrders)
	if string(before) != string(after) {
		t.Fatal("growth changed current bins or permanent monetary charges")
	}
	for id, swap := range state.Swaps {
		want := funding[id]
		if want.Bytes < 24000 || len(swap.ShortFunding) != want.Bytes || sha256.Sum256([]byte(swap.ShortFunding)) != want.Digest || swap.SelfRefunds[0] != "original retained refund bytes" {
			t.Fatal("growth lost exact funding or retained refund payload")
		}
	}
}
