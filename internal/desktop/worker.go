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
	ctx      context.Context
	advisory sync.WaitGroup
	command  func(context.Context, daemon.Request) (any, error)
	statusMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	refresh  chan chan refreshResult
	snapshot atomic.Pointer[json.RawMessage]
}

func startWalletWorker(ctx context.Context, engine walletRuntime) *walletWorker {
	ctx, cancel := context.WithCancel(ctx)
	w := &walletWorker{ctx: ctx, cancel: cancel, done: make(chan struct{}), refresh: make(chan chan refreshResult, 64)}
	if runtime, ok := engine.(interface {
		Command(context.Context, daemon.Request) (any, error)
	}); ok {
		w.command = runtime.Command
	}
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

func (m *Manager) stopWorkers() {
	for _, worker := range m.workers {
		worker.cancel()
	}
	for _, worker := range m.workers {
		<-worker.done
		worker.advisory.Wait()
	}
	m.workers = map[string]*walletWorker{}
}
func (m *Manager) startWorkers(ctx context.Context) {
	if m.workers == nil {
		m.workers = map[string]*walletWorker{}
	}
	for id, engine := range m.engines {
		if m.workers[id] == nil {
			m.workers[id] = startWalletWorker(ctx, engine)
		}
	}
}

// Advisory reads may perform bounded chain IO but must not hold the global
// lifecycle lock or wait for a post-read Tick snapshot. Register the call while
// holding m.mu so stopping a worker can cancel and join every reader before its
// engine/backends are closed. WaitGroup additions and lifecycle waits serialize
// under that same lock; advisory completion never needs it.
func (m *Manager) preflightFunds(ctx context.Context, profile string, req daemon.Request) (any, error) {
	m.mu.Lock()
	if err := daemon.CheckCommandNetwork(req, chain.Network(m.settings.ActiveNetwork), true); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	worker := m.workers[profile]
	if worker == nil || worker.command == nil || m.restart || m.stopped || m.settings.OnboardingStage != "" {
		m.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "wallet is connecting; check Settings and connection status")
	}
	worker.advisory.Add(1)
	m.mu.Unlock()
	defer worker.advisory.Done()
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(worker.ctx, cancel)
	defer stop()
	if err := worker.ctx.Err(); err != nil {
		return nil, err
	}
	result, err := worker.command(call, req)
	if cancelled := call.Err(); cancelled != nil {
		return nil, cancelled
	}
	return result, err
}
