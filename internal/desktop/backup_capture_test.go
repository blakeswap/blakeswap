package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func captureManagerFixture(t *testing.T, fallback bool) *Manager {
	t.Helper()
	m := setupManager(t)
	prepare(t, m)
	m.backupNoClone = fallback
	root := filepath.Join(m.root, "wallets", "alice")
	seed, password, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	v, err := storage.Open(filepath.Join(root, string(chain.Regtest), "state.db"), bytes.TrimSpace(password))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	records := make([]storage.ArchiveRecord, 130)
	stats := storage.ArchiveStats{Count: 130, Kinds: map[string]uint64{"seen": 130}}
	for i := range records {
		records[i] = storage.ArchiveRecord{Kind: "seen", ID: fmt.Sprintf("sender:%d", i), Data: json.RawMessage(`"retained exact digest"`)}
		raw, _ := json.Marshal(records[i])
		stats.Bytes += uint64(len(raw) + 1)
	}
	active := daemon.State{Version: daemon.StateVersion, Network: chain.Regtest, Mnemonic: seed, Capacity: &daemon.CapacityRecord{Archived: stats}}
	normalizeState(&active)
	if _, err = v.CommitArchive(active, storage.ArchiveBatch{Put: records}, 0); err != nil {
		t.Fatal(err)
	}
	return m
}
func captureManager(t *testing.T, m *Manager) *portableCapture {
	t.Helper()
	m.mu.Lock()
	capture, err := m.captureBackupLocked(context.Background(), "alice", false)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { capture.closeSources(); capture.staging.close() })
	return capture
}
func TestPortableCaptureFallbackFrozenTokenSurvivesWriterGrowth(t *testing.T) {
	m := captureManagerFixture(t, true)
	capture := captureManager(t, m)
	source := capture.sources[0]
	if source.cloned || source.page == nil {
		t.Fatal("forced fallback not selected")
	}
	var before daemon.State
	if _, _, err := source.page.LoadState(&before); err != nil {
		t.Fatal(err)
	}
	frozen := daemon.BackupSemanticToken(before)
	if frozen == "" {
		t.Fatal("missing token was not durably established")
	}
	live := before
	live.Seen = map[string]string{"later-large": strings.Repeat("x", 8<<20)}
	if _, err := daemon.PrepareStoredBackupToken(&live); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- source.vault.Save(live) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture holds source writer")
	}
	if !m.mu.TryLock() {
		t.Fatal("captured source retains lifecycle lock")
	}
	m.mu.Unlock()
	snapshot, err := capture.materialize()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.close()
	state, err := snapshot.Wallets[0].networkState(chain.Regtest)
	if err != nil || state.Seen["later-large"] != "" || len(state.Seen) != 0 || len(state.Archive) != 130 {
		t.Fatal("snapshot mixed active generations", err)
	}
	if snapshot.Wallets[0].marks[chain.Regtest].SemanticToken != frozen {
		t.Fatal("materialization used later semantic token")
	}
	m.mu.Lock()
	err = m.recordPortableLocked(snapshot)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, password, err := readMaster(filepath.Join(m.root, "wallets", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	v, err := storage.Open(filepath.Join(m.root, "wallets", "alice", "regtest", "state.db"), bytes.TrimSpace(password))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var saved daemon.State
	if _, err = v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	freshness, err := daemon.StateBackupFreshness(saved)
	if err != nil || !freshness.StateChanged || saved.Backup.SemanticToken != frozen {
		t.Fatal("late export falsely covered later state", freshness, err)
	}
}
func TestPortableCaptureRejectsChangedArchiveWithoutPublishingOrMarking(t *testing.T) {
	m := captureManagerFixture(t, true)
	capture := captureManager(t, m)
	source := capture.sources[0]
	var live daemon.State
	if _, err := source.vault.Load(&live); err != nil {
		t.Fatal(err)
	}
	record, found, err := source.vault.ReadArchive("seen", "sender:0")
	if err != nil || !found {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record)
	live.Capacity.Archived.Count--
	live.Capacity.Archived.Kinds["seen"]--
	live.Capacity.Archived.Bytes -= uint64(len(raw) + 1)
	if _, err = source.vault.CommitArchive(live, storage.ArchiveBatch{Delete: []storage.ArchiveKey{{Kind: "seen", ID: "sender:0"}}}, 0); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capture.materialize()
	defer snapshot.close()
	if !errors.Is(err, storage.ErrArchiveChanged) {
		t.Fatal("mixed archive accepted", err)
	}
	if _, err = os.Stat(capture.staging.root); !os.IsNotExist(err) {
		t.Fatal("failed private snapshot remains", err)
	}
	if len(snapshot.Wallets) != 0 {
		t.Fatal("partial snapshot published")
	}
	for _, key := range capture.staging.clonePasswords {
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Fatal("failed source key retained")
		}
	}
}
func TestPortableCaptureLifecycleCancellationJoinsWithoutManagerLock(t *testing.T) {
	m := captureManagerFixture(t, true)
	capture := captureManager(t, m)
	entered := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		err := capture.sources[0].page.VisitArchive(capture.ctx, func(storage.ArchiveRecord) error { close(entered); <-capture.ctx.Done(); return capture.ctx.Err() })
		capture.closeSources()
		capture.staging.close()
		finished <- err
	}()
	<-entered
	// A periodic connect must not open another bbolt handle against an owned
	// inactive source while it is being streamed.
	m.mu.Lock()
	m.connect(context.Background())
	m.mu.Unlock()
	if len(m.openings) != 0 {
		t.Fatal("opened inactive source during capture")
	}
	stopped := make(chan struct{})
	go func() { m.mu.Lock(); m.stopOpening(); m.mu.Unlock(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle cancellation deadlocked on source release")
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(capture.staging.root); !os.IsNotExist(err) {
		t.Fatal("cancelled clone/staging remains", err)
	}
}
func TestPortableCaptureCloneKeyOwnershipAndIndependentProvider(t *testing.T) {
	m := captureManagerFixture(t, false)
	capture := captureManager(t, m)
	if !capture.sources[0].cloned {
		t.Skip("filesystem clone unavailable")
	}
	if capture.sources[0].page != nil || capture.sources[0].release != nil {
		t.Fatal("cloned source registered a nil fallback lease")
	}
	// Release the live handle before validation. The provider owns both an
	// independent encrypted file and copied source credentials.
	_ = capture.sources[0].vault.Close()
	capture.sources[0].vault = nil
	snapshot, err := capture.materialize()
	if err != nil {
		t.Fatal(err)
	}
	state, err := snapshot.Wallets[0].networkState(chain.Regtest)
	if err != nil || len(state.Archive) != 130 {
		t.Fatal("clone depended on source handle", err)
	}
	path := capture.staging.root
	keys := capture.staging.clonePasswords
	snapshot.close()
	snapshot.close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("private clone remains", err)
	}
	for _, key := range keys {
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Fatal("copied credential survives provider close")
		}
	}
}
