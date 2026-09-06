package desktop

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type walletRuntime interface {
	Tick(context.Context) error
	Status() daemon.Status
}
type refreshResult struct {
	raw json.RawMessage
	err error
}
type walletWorker struct {
	statusMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	refresh  chan chan refreshResult
	snapshot atomic.Pointer[json.RawMessage]
}

func startWalletWorker(ctx context.Context, engine walletRuntime) *walletWorker {
	ctx, cancel := context.WithCancel(ctx)
	w := &walletWorker{cancel: cancel, done: make(chan struct{}), refresh: make(chan chan refreshResult, 64)}
	w.capture(engine, nil)
	go w.run(ctx, engine)
	return w
}

// Serialize reading and publishing across commands and ticks so an older
// snapshot cannot overwrite a command's newer state.
func (w *walletWorker) capture(engine walletRuntime, tickError error) json.RawMessage {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	s := engine.Status()
	if tickError != nil {
		s.LastError = tickError.Error()
	}
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	result := json.RawMessage(raw)
	w.snapshot.Store(&result)
	return result
}
func (w *walletWorker) read() json.RawMessage { return *w.snapshot.Load() }
func (w *walletWorker) check(ctx context.Context) (json.RawMessage, error) {
	reply := make(chan refreshResult, 1)
	select {
	case w.refresh <- reply:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, context.Canceled
	}
	select {
	case result := <-reply:
		return result.raw, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, context.Canceled
	}
}
func (w *walletWorker) run(ctx context.Context, engine walletRuntime) {
	defer close(w.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var replies []chan refreshResult
		select {
		case <-ctx.Done():
			return
		case reply := <-w.refresh:
			replies = append(replies, reply)
		case <-timer.C:
		}
		// Only requests queued before this cycle share its result. A request arriving
		// during IO schedules another cycle so its chain reads start after the click.
	drain:
		for {
			select {
			case reply := <-w.refresh:
				replies = append(replies, reply)
			default:
				break drain
			}
		}
		if ctx.Err() != nil {
			return
		}
		cycle, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := engine.Tick(cycle)
		cancel()
		raw := w.capture(engine, err)
		for _, reply := range replies {
			reply <- refreshResult{raw, err}
		}
		timer.Reset(1500 * time.Millisecond)
	}
}

// Resolve the wallet under the lifecycle lock, then wait without blocking other
// wallets or settings. Closing/reconfiguring its worker cancels the wait.
func (m *Manager) refreshStatus(ctx context.Context, profile string, req daemon.Request) (any, error) {
	m.mu.Lock()
	if err := daemon.CheckCommandNetwork(req, chain.Network(m.settings.ActiveNetwork), true); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	worker := m.workers[profile]
	unavailable := worker == nil || m.restart || m.stopped || m.settings.OnboardingStage != ""
	m.mu.Unlock()
	if unavailable {
		return nil, status.Error(codes.Unavailable, "wallet is connecting; check Settings and connection status")
	}
	return worker.check(ctx)
}
