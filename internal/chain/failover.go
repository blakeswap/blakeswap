package chain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Endpoint retains the legacy primary fields; Fallbacks are ordered after it.
// Each candidate owns its transport, header cache and watch-only wallet state.
type Endpoint struct {
	Kind              string     `json:"kind"`
	URL               string     `json:"url"`
	Cookie            string     `json:"cookie"`
	CertificateSHA256 string     `json:"certificate_sha256"`
	Fallbacks         []Endpoint `json:"fallbacks,omitempty"`
}

func (e Endpoint) Candidates() []Endpoint { return append([]Endpoint{e}, e.Fallbacks...) }
func NewEndpoint(n Network, id ID, e Endpoint) (Backend, error) {
	switch e.Kind {
	case "electrum":
		return NewElectrum(n, id, e.URL, e.CertificateSHA256)
	case "rpc", "":
		if e.CertificateSHA256 != "" {
			return nil, errors.New("RPC certificate pins are unsupported; use CA-validated HTTPS")
		}
		return NewFor(n, id, e.URL, e.Cookie)
	default:
		return nil, errors.New("unknown node backend")
	}
}

type EndpointHealth struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	Active      bool   `json:"active"`
	LastSuccess int64  `json:"last_success"`
	RetryAfter  int64  `json:"retry_after"`
	Error       string `json:"error"`
}
type EndpointStatus struct {
	Endpoints    []EndpointHealth `json:"endpoints"`
	Generation   uint64           `json:"generation"`
	Failovers    uint64           `json:"failovers"`
	LastFailover int64            `json:"last_failover"`
}
type endpointEntry struct {
	backend   Backend
	scanner   SpendScanner
	watch     Backend
	observed  int
	health    EndpointHealth
	failures  uint
	validated bool
	history   <-chan watchResult
}
type watchResult struct {
	backend Backend
	count   int
	err     error
}

// Failover is availability routing, not a quorum or consensus validator. A new
// source must agree with the previous source's last observed tip. A conflicting
// fork fails closed until the old source returns or the operator reconciles it.
type Failover struct {
	id            ID
	gate          chan struct{}
	mu            sync.Mutex
	entries       []endpointEntry
	active        int
	generation    uint64
	failovers     uint64
	lastFailover  int64
	anchorHeight  uint32
	anchorHash    string
	name          string
	addresses     []string
	attemptBudget time.Duration
	historyCtx    context.Context
	stopHistory   context.CancelFunc
	historyJobs   sync.WaitGroup
}

func NewFailover(n Network, id ID, config Endpoint) (*Failover, error) {
	list := config.Candidates()
	if len(list) > 4 {
		return nil, errors.New("configure at most four endpoints per chain")
	}
	historyCtx, stopHistory := context.WithCancel(context.Background())
	p := &Failover{id: id, active: -1, gate: make(chan struct{}, 1), attemptBudget: 2 * time.Second, historyCtx: historyCtx, stopHistory: stopHistory}
	seen := map[string]bool{}
	for _, c := range list {
		if c.Kind == "" {
			c.Kind = "rpc"
		}
		if len(c.Fallbacks) > 0 && len(p.entries) > 0 {
			p.Close()
			return nil, errors.New("nested fallback endpoints are invalid")
		}
		if seen[c.URL] {
			p.Close()
			return nil, errors.New("duplicate chain endpoint")
		}
		seen[c.URL] = true
		b, err := NewEndpoint(n, id, c)
		if err != nil {
			p.Close()
			return nil, err
		}
		var scanner SpendScanner
		if r, ok := b.(*RPC); ok {
			scanner = &Scanner{RPC: r}
		} else {
			scanner = b.(SpendScanner)
		}
		p.entries = append(p.entries, endpointEntry{backend: b, scanner: scanner, health: EndpointHealth{URL: c.URL, Kind: c.Kind}})
	}
	return p, nil
}
func (p *Failover) Status() EndpointStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := EndpointStatus{Generation: p.generation, Failovers: p.failovers, LastFailover: p.lastFailover}
	for i := range p.entries {
		h := p.entries[i].health
		h.Active = i == p.active
		s.Endpoints = append(s.Endpoints, h)
	}
	return s
}
func (p *Failover) Generation() uint64 { return p.Status().Generation }
func (p *Failover) do(ctx context.Context, fn func(context.Context, *endpointEntry) error) error {
	ctx, finishBudget := boundedChainWork(ctx, p.id)
	defer finishBudget()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	order := make([]int, 0, len(p.entries))
	if p.active >= 0 {
		order = append(order, p.active)
	}
	for i := range p.entries {
		if i != p.active {
			order = append(order, i)
		}
	}
	var errs []error
	for _, i := range order {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		p.mu.Lock()
		retry := p.entries[i].health.RetryAfter
		p.mu.Unlock()
		if retry > time.Now().Unix() {
			continue
		}
		c, cancel := context.WithTimeout(ctx, p.attemptBudget)
		entry := &p.entries[i]
		err := error(nil)
		if p.active != i || !entry.validated {
			err = entry.backend.Check(c)
			if err == nil && p.active != i {
				err = p.validateSwitch(c, entry)
			}
			if err == nil {
				p.mu.Lock()
				if (p.active >= 0 && p.active != i) || (p.active < 0 && i > 0) {
					p.failovers++
					p.lastFailover = time.Now().Unix()
				}
				p.active = i
				p.generation++
				entry.validated = true
				p.mu.Unlock()
			}
		}
		if err == nil {
			err = fn(c, entry)
		}
		cancel()
		var progress *scanProgressError
		if errors.As(err, &progress) {
			// Catch-up retained completed blocks on this endpoint. Its bounded
			// work slice is incomplete, not evidence of a broken transport.
			// Keep observations unavailable until the complete scan succeeds.
			return err
		}
		if ctx.Err() != nil {
			entry.validated = false
			return ctx.Err()
		}
		var held *broadcastGuardError
		if TransactionNotFound(err) || errors.As(err, &held) {
			return err
		}
		p.mu.Lock()
		if err == nil {
			entry.failures = 0
			entry.health.Error = ""
			entry.health.RetryAfter = 0
			entry.health.LastSuccess = time.Now().Unix()
		} else {
			entry.validated = false
			entry.failures++
			entry.health.Error = err.Error()
			entry.health.RetryAfter = time.Now().Add(time.Second * time.Duration(1<<min(entry.failures, 5))).Unix()
		}
		p.mu.Unlock()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("endpoint %d (%s): %w", i+1, entry.health.URL, err))
	}
	if len(errs) == 0 {
		return errors.New("all chain endpoints are in backoff")
	}
	return errors.Join(errs...)
}

type blockHasher interface {
	BlockHash(context.Context, uint32) (string, error)
}

func (p *Failover) validateSwitch(ctx context.Context, e *endpointEntry) error {
	h, err := e.backend.Height(ctx)
	if err != nil {
		return err
	}
	if p.anchorHash != "" {
		if h < p.anchorHeight {
			return errors.New("stale candidate tip is behind the last successful observation")
		}
		hash, err := e.backend.(blockHasher).BlockHash(ctx, p.anchorHeight)
		if err != nil {
			return err
		}
		if hash != p.anchorHash {
			return errors.New("conflicting chain history at last observed tip; unsafe actions stopped")
		}
	}
	return nil
}
func (p *Failover) Check(ctx context.Context) error {
	return p.do(ctx, func(c context.Context, e *endpointEntry) error { return e.backend.Check(c) })
}

// Preserve the selected endpoint's complete estimate provenance. Unavailable
// estimation is an advisory result and does not make an otherwise usable chain
// endpoint unhealthy or substitute a different chain's rate.
func (p *Failover) EstimateFee(ctx context.Context, target uint32) FeeEstimate {
	result := FeeEstimate{Chain: p.id, RequestedTarget: target, Target: target, State: "unavailable"}
	err := p.do(ctx, func(c context.Context, entry *endpointEntry) error {
		estimator, ok := entry.backend.(FeeEstimator)
		if !ok {
			result.Error = "backend has no fee estimator; select a manual total fee"
			return nil
		}
		result = estimator.EstimateFee(c, target)
		if result.Chain != p.id {
			return errors.New("estimator returned a different chain")
		}
		return c.Err()
	})
	if err != nil {
		return FeeEstimate{Chain: p.id, RequestedTarget: target, Target: target, State: "unavailable", Error: err.Error()}
	}
	return result
}
func (p *Failover) Height(ctx context.Context) (h uint32, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		h, er = e.backend.Height(c)
		if er != nil {
			return er
		}
		hash, er := e.backend.(blockHasher).BlockHash(c, h)
		if er == nil {
			p.anchorHeight = h
			p.anchorHash = hash
		}
		return er
	})
	return
}
func (p *Failover) MedianTime(ctx context.Context) (v uint32, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		v, er = e.backend.MedianTime(c)
		return er
	})
	return
}

// GuardedBroadcaster rechecks caller authorization after endpoint admission and
// before every write attempt. A source switch must not silently carry prior
// multi-chain observations into a publication on a new endpoint.
type GuardedBroadcaster interface {
	BroadcastGuarded(context.Context, string, func() error) (string, error)
}

type broadcastGuardError struct{ error }

func (p *Failover) Broadcast(ctx context.Context, raw string) (string, error) {
	return p.BroadcastGuarded(ctx, raw, nil)
}
func (p *Failover) BroadcastGuarded(ctx context.Context, raw string, guard func() error) (v string, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		if guard != nil {
			if err := guard(); err != nil {
				return &broadcastGuardError{err}
			}
		}
		var er error
		v, er = e.backend.Broadcast(c, raw)
		return er
	})
	return
}
func (p *Failover) Output(ctx context.Context, id string, n uint32) (v *TxOut, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		v, er = e.backend.Output(c, id, n)
		return er
	})
	return
}
func (p *Failover) Transaction(ctx context.Context, id string) (v Transaction, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		v, er = e.backend.Transaction(c, id)
		return er
	})
	return
}
func (p *Failover) Coinbase(ctx context.Context, h uint32) (v Transaction, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		v, er = e.backend.Coinbase(c, h)
		return er
	})
	return
}

// Observe keeps history imports independently cancellable. A mainnet rescan may
// take hours; cancelling a trading tick must not discard its eventual complete
// response or repeatedly restart that rescan. Close cancels and joins every job.
func (p *Failover) Observe(ctx context.Context, name string, addresses []string) (Backend, error) {
	return p.observe(ctx, name, addresses, false)
}
func (p *Failover) ObserveNew(ctx context.Context, name string, addresses []string) (Backend, error) {
	return p.observe(ctx, name, addresses, true)
}
func (p *Failover) observe(ctx context.Context, name string, addresses []string, fresh bool) (Backend, error) {
	err := p.do(ctx, func(c context.Context, e *endpointEntry) error {
		if p.name != "" && p.name != name {
			return errors.New("endpoint watch wallet changed")
		}
		p.name = name
		for _, a := range addresses {
			found := false
			for _, old := range p.addresses {
				found = found || a == old
			}
			if !found {
				p.addresses = append(p.addresses, a)
			}
		}
		return p.ensureWatch(c, e, fresh)
	})
	return p, err
}
func (p *Failover) ensureWatch(ctx context.Context, e *endpointEntry, fresh bool) error {
	for {
		if e.watch != nil && e.observed == len(p.addresses) {
			return nil
		}
		if e.history == nil {
			addresses := append([]string(nil), p.addresses...)
			name, count := p.name, len(addresses)
			observe := e.backend.Observe
			if fresh && e.watch != nil {
				if live, ok := e.backend.(interface {
					ObserveNew(context.Context, string, []string) (Backend, error)
				}); ok {
					observe = live.ObserveNew
					addresses = addresses[e.observed:]
				}
			}
			result := make(chan watchResult, 1)
			e.history = result
			historyCtx := p.historyCtx
			if historyCtx == nil {
				historyCtx = ctx
			}
			p.historyJobs.Add(1)
			go func() {
				defer p.historyJobs.Done()
				backend, err := observe(historyCtx, name, addresses)
				result <- watchResult{backend, count, err}
			}()
		}
		select {
		case result := <-e.history:
			e.history = nil
			if result.err != nil {
				return result.err
			}
			e.watch = result.backend
			e.observed = result.count
		case <-ctx.Done():
			return fmt.Errorf("wallet history synchronizing: %w", ctx.Err())
		}
	}
}
func (p *Failover) Unspent(ctx context.Context, a []string) (v []UTXO, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		if er := p.ensureWatch(c, e, false); er != nil {
			return er
		}
		var er error
		v, er = e.watch.Unspent(c, a)
		return er
	})
	return
}
func (p *Failover) ConfirmedReceived(ctx context.Context, a string) (v bool, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		if er := p.ensureWatch(c, e, false); er != nil {
			return er
		}
		var er error
		v, er = e.watch.ConfirmedReceived(c, a)
		return er
	})
	return
}
func (p *Failover) Scan(ctx context.Context, start uint32, points []string) (v map[string]Observation, err error) {
	err = p.do(ctx, func(c context.Context, e *endpointEntry) error {
		var er error
		v, er = e.scanner.Scan(c, start, points)
		return er
	})
	return
}
func (p *Failover) Close() error {
	if p.stopHistory != nil {
		p.stopHistory()
	}
	p.historyJobs.Wait()
	for i := range p.entries {
		_ = p.entries[i].backend.Close()
	}
	return nil
}

// RPCForTesting is restricted by the daemon to regtest development commands.
func (p *Failover) RPCForTesting(ctx context.Context) (r *RPC, err error) {
	err = p.do(ctx, func(_ context.Context, e *endpointEntry) error {
		var ok bool
		r, ok = e.backend.(*RPC)
		if !ok {
			return errors.New("regtest tool requires RPC")
		}
		return nil
	})
	return
}
func (r *RPC) BlockHash(ctx context.Context, h uint32) (hash string, err error) {
	err = r.Call(ctx, "getblockhash", &hash, h)
	return
}
func (e *Electrum) BlockHash(ctx context.Context, h uint32) (string, error) {
	raw, err := e.header(ctx, h)
	if err != nil {
		return "", err
	}
	hash, err := HeaderHash(raw)
	return hash.String(), err
}

// NewScanner keeps remote-job cursors separate from local settlement scans.
func (p *Failover) NewScanner() SpendScanner {
	scanners := make([]SpendScanner, len(p.entries))
	for i, e := range p.entries {
		if r, ok := e.backend.(*RPC); ok {
			scanners[i] = &Scanner{RPC: r}
		} else {
			scanners[i] = e.backend.(SpendScanner)
		}
	}
	return &failoverScanner{pool: p, scanners: scanners}
}

type scanProgressError struct{ cause error }

func (e *scanProgressError) Error() string {
	return "chain history catch-up incomplete; block scan progress retained"
}
func (e *scanProgressError) Unwrap() error { return e.cause }

type failoverScanner struct {
	pool     *Failover
	scanners []SpendScanner
}

func (s *failoverScanner) Scan(ctx context.Context, start uint32, points []string) (out map[string]Observation, err error) {
	err = s.pool.do(ctx, func(c context.Context, _ *endpointEntry) error {
		var e error
		scanner := s.scanners[s.pool.active]
		rpc, incremental := scanner.(*Scanner)
		var before uint64
		if incremental {
			before = rpc.progress
		}
		out, e = scanner.Scan(c, start, points)
		if incremental && rpc.progress != before && c.Err() != nil && errors.Is(e, context.DeadlineExceeded) {
			return &scanProgressError{cause: e}
		}
		return e
	})
	return
}
