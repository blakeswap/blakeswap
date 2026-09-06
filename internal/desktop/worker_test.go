package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

type workerFixture struct {
	name  string
	ticks atomic.Int32
	tick  func(context.Context) error
}

func (f *workerFixture) Tick(ctx context.Context) error {
	if f.tick != nil {
		if err := f.tick(ctx); err != nil {
			return err
		}
	}
	f.ticks.Add(1)
	return nil
}
func (f *workerFixture) Status() daemon.Status {
	return daemon.Status{Name: f.name, Network: chain.Regtest, Heights: map[chain.ID]uint32{chain.BTC: uint32(f.ticks.Load())}}
}
func stopWorker(w *walletWorker) { w.cancel(); <-w.done }

func TestWalletWorkersRefreshIndependentlyOfSelectedAndSlowWallets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	slow := &workerFixture{name: "bob", tick: func(ctx context.Context) error {
		select {
		case <-blocked:
		default:
			close(blocked)
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	alice := &workerFixture{name: "alice"}
	wa, wb := startWalletWorker(ctx, alice), startWalletWorker(ctx, slow)
	defer stopWorker(wa)
	defer stopWorker(wb)
	<-blocked
	settings := configuredDefaults()
	settings.ActiveNetwork = "regtest"
	settings.Wallets = []*pb.WalletProfile{{Id: "alice", Name: "Alice"}, {Id: "bob", Name: "Bob"}}
	m := &Manager{settings: settings, workers: map[string]*walletWorker{"alice": wa, "bob": wb}}
	// UI reads only Bob. Alice must continue advancing without a status read or
	// selection. Explicit refresh must never wait on Bob's stalled chain call.
	m.view.Store(&desktopView{settings: settings, workers: m.workers})
	if _, err := m.command(ctx, "bob", daemon.Request{Method: "status"}); err != nil {
		t.Fatal(err)
	}
	deadline, done := context.WithTimeout(ctx, 2*time.Second)
	defer done()
	raw, err := m.command(deadline, "alice", daemon.Request{Method: "status.refresh", Params: json.RawMessage(`{"expected_network":"regtest"}`)})
	if err != nil {
		t.Fatal("another wallet blocked fresh state", err)
	}
	var got daemon.Status
	if err := json.Unmarshal(raw.(json.RawMessage), &got); err != nil || got.Heights[chain.BTC] == 0 || got.Name != "alice" {
		t.Fatal("refresh returned stale/wrong wallet", err)
	}
	previous := alice.ticks.Load()
	limit := time.After(3 * time.Second)
	for alice.ticks.Load() <= previous {
		select {
		case <-limit:
			t.Fatal("background wallet waits for slow wallet or UI selection")
		case <-time.After(10 * time.Millisecond):
		}
	}
	latest, _ := m.command(ctx, "alice", daemon.Request{Method: "status"})
	if err := json.Unmarshal(latest.(json.RawMessage), &got); err != nil || got.Heights[chain.BTC] <= uint32(previous) {
		t.Fatal("automatic status snapshot is stale", err)
	}
	if _, err := m.command(ctx, "alice", daemon.Request{Method: "status.refresh", Params: json.RawMessage(`{"expected_network":"mainnet"}`)}); err == nil {
		t.Fatal("stale network refreshed")
	}
}

func TestRefreshDuringCycleWaitsForReadsStartedAfterRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := &workerFixture{name: "alice"}
	var calls atomic.Int32
	f.tick = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	w := startWalletWorker(context.Background(), f)
	defer stopWorker(w)
	<-entered
	reply := make(chan refreshResult, 1)
	w.refresh <- reply
	close(release)
	select {
	case result := <-reply:
		var s daemon.Status
		if err := json.Unmarshal(result.raw, &s); err != nil || s.Heights[chain.BTC] < 2 {
			t.Fatal("refresh reused in-flight stale reads", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not wake sleeping worker")
	}
	stop := &workerFixture{name: "stopped", tick: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	stopped := startWalletWorker(context.Background(), stop)
	stopWorker(stopped)
	if _, err := stopped.check(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("refresh survived stopped wallet", err)
	}
}
