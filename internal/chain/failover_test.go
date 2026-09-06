package chain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type failoverFeeBackend struct {
	*failoverBackend
	quote FeeEstimate
	stall bool
}

func (b *failoverFeeBackend) EstimateFee(ctx context.Context, target uint32) FeeEstimate {
	if b.stall {
		<-ctx.Done()
	}
	return b.quote
}

func TestFailoverForwardsFeeEstimateProvenanceAndCancellation(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		quote := FeeEstimate{Chain: id, RequestedTarget: 3, Target: 7, Timestamp: time.Now().Unix(), Rate: 7654, State: "available", Source: "rpc:estimatesmartfee"}
		backend := &failoverFeeBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}, quote: quote}
		p := testPool(&failoverBackend{check: errors.New("offline")}, backend)
		p.id = id
		if result := p.EstimateFee(context.Background(), 3); !reflect.DeepEqual(result, quote) {
			t.Fatal("estimate provenance changed", result)
		}
		backend.quote.State, backend.quote.Error = "unavailable", "insufficient fee history"
		if result := p.EstimateFee(context.Background(), 3); !reflect.DeepEqual(result, backend.quote) || p.Status().Endpoints[1].Error != "" {
			t.Fatal("advisory absence broke a healthy endpoint", result)
		}
		backend.stall = true
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		result := p.EstimateFee(ctx, 3)
		cancel()
		if result.State != "unavailable" || result.Chain != id || result.Error == "" {
			t.Fatal("cancelled estimate accepted", result)
		}
	}
	wrong := &failoverFeeBackend{failoverBackend: &failoverBackend{height: 100}, quote: FeeEstimate{Chain: Blake, State: "available", Rate: 1000}}
	p := testPool(wrong)
	p.id = BTC
	if result := p.EstimateFee(context.Background(), 3); result.Chain != BTC || result.State != "unavailable" {
		t.Fatal("wrong chain substituted", result)
	}
}

type failoverBackend struct {
	Backend
	check                     error
	blocked                   bool
	height                    uint32
	hash                      string
	reads, checks, broadcasts int
	raw                       string
	observed                  []string
	outputError               error
}

func (b *failoverBackend) Check(context.Context) error { b.checks++; return b.check }
func (b *failoverBackend) Height(ctx context.Context) (uint32, error) {
	if b.blocked {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	b.reads++
	return b.height, nil
}
func (b *failoverBackend) BlockHash(context.Context, uint32) (string, error) { return b.hash, nil }
func (b *failoverBackend) Broadcast(_ context.Context, raw string) (string, error) {
	b.broadcasts++
	b.raw = raw
	if b.blocked {
		return "", context.DeadlineExceeded
	}
	return "accepted", nil
}
func (b *failoverBackend) Output(context.Context, string, uint32) (*TxOut, error) {
	return &TxOut{Value: 123}, b.outputError
}
func (b *failoverBackend) Observe(_ context.Context, _ string, a []string) (Backend, error) {
	b.observed = append([]string(nil), a...)
	return b, nil
}
func (b *failoverBackend) Unspent(context.Context, []string) ([]UTXO, error) {
	return []UTXO{{Amount: 123}}, nil
}
func (b *failoverBackend) Close() error { return nil }
func testPool(backends ...Backend) *Failover {
	p := &Failover{active: -1, gate: make(chan struct{}, 1), attemptBudget: 20 * time.Millisecond}
	for i, b := range backends {
		p.entries = append(p.entries, endpointEntry{backend: b, health: EndpointHealth{URL: strings.Repeat("x", i+1)}})
	}
	return p
}
func retryNow(p *Failover) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.entries {
		p.entries[i].health.RetryAfter = 0
	}
}
func TestFailoverTimeoutHealthAndOrderedValidation(t *testing.T) {
	a := &failoverBackend{height: 100, hash: "a", blocked: true}
	wrong := &failoverBackend{height: 100, hash: "a", check: errors.New("wrong network")}
	b := &failoverBackend{height: 100, hash: "a"}
	p := testPool(a, wrong, b)
	start := time.Now()
	if h, err := p.Height(context.Background()); err != nil || h != 100 {
		t.Fatal(h, err)
	}
	if time.Since(start) > time.Second || wrong.reads != 0 || b.checks != 1 {
		t.Fatal("unbounded or unvalidated failover")
	}
	s := p.Status()
	if !s.Endpoints[2].Active || s.Endpoints[0].Error == "" || s.Endpoints[0].RetryAfter == 0 || s.Endpoints[2].LastSuccess == 0 {
		t.Fatal(s)
	}
	if _, err := p.Height(context.Background()); err != nil || a.checks != 1 {
		t.Fatal("did not retain active healthy source", err)
	}
}
func TestFailoverRejectsStaleConflictAndRecoversOriginalReorg(t *testing.T) {
	for _, kind := range []string{"stale", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			a := &failoverBackend{height: 100, hash: "original"}
			b := &failoverBackend{height: 100, hash: "original"}
			p := testPool(a, b)
			if _, err := p.Height(context.Background()); err != nil {
				t.Fatal(err)
			}
			a.blocked = true
			if kind == "stale" {
				b.height = 99
			} else {
				b.hash = "fork"
			}
			if _, err := p.Height(context.Background()); err == nil || !strings.Contains(err.Error(), map[string]string{"stale": "stale", "conflict": "conflicting"}[kind]) {
				t.Fatal(err)
			}
			// A recovering original source may report a legitimate reorg. Validate its
			// chain rules again, then adopt its new canonical tip before later failover.
			a.blocked = false
			a.height = 101
			a.hash = "fork"
			retryNow(p)
			if _, err := p.Height(context.Background()); err != nil {
				t.Fatal("original reorg remained permanently pinned", err)
			}
			a.blocked = true
			b.height = 101
			b.hash = "fork"
			retryNow(p)
			if _, err := p.Height(context.Background()); err != nil {
				t.Fatal("reconciled failover failed", err)
			}
			if p.Status().Failovers != 1 {
				t.Fatal(p.Status())
			}
		})
	}
}
func TestFailoverAmbiguousBroadcastRetainsExactBytesAndWatchProvenance(t *testing.T) {
	a := &failoverBackend{height: 100, hash: "tip"}
	b := &failoverBackend{height: 100, hash: "tip"}
	p := testPool(a, b)
	if _, err := p.Height(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Observe(context.Background(), "wallet", []string{"old", "current"}); err != nil {
		t.Fatal(err)
	}
	a.blocked = true
	if _, err := p.Broadcast(context.Background(), "identical signed bytes"); err != nil {
		t.Fatal(err)
	}
	if a.broadcasts != 1 || b.broadcasts != 1 || a.raw != b.raw {
		t.Fatal("ambiguous response changed or lost transaction")
	}
	if _, err := p.Unspent(context.Background(), []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.observed, ",") != "old,current" {
		t.Fatal("fallback skipped full watch import", b.observed)
	}
}
func TestFailoverRejectsBadProofAndPinBeforeUse(t *testing.T) {
	a := &failoverBackend{height: 100, hash: "tip", outputError: errors.New("invalid merkle proof")}
	b := &failoverBackend{height: 100, hash: "tip"}
	p := testPool(a, b)
	if _, err := p.Output(context.Background(), "tx", 0); err != nil {
		t.Fatal(err)
	}
	if p.Status().Endpoints[0].Error != "invalid merkle proof" || !p.Status().Endpoints[1].Active {
		t.Fatal(p.Status())
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("bad pin accepted") }))
	defer server.Close()
	electrum, err := NewElectrum(Regtest, BTC, "ssl"+strings.TrimPrefix(server.URL, "https"), strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	p = testPool(electrum, b)
	p.attemptBudget = time.Second
	if _, err := p.Height(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Status().Endpoints[0].Error, "pin mismatch") || !p.Status().Endpoints[1].Active {
		t.Fatal(p.Status())
	}
	if _, err := NewElectrum(Regtest, BTC, "tcp://127.0.0.1:1", strings.Repeat("0", 64)); err == nil {
		t.Fatal("plaintext pin ignored")
	}
}
func TestFailoverCancelledCallerAndConfiguration(t *testing.T) {
	p := testPool(&failoverBackend{height: 100, hash: "a"})
	p.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Height(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-p.gate
	for _, cfg := range []Endpoint{
		{Kind: "rpc", URL: "http://public.example"},
		{Kind: "rpc", URL: "http://127.0.0.1:1", CertificateSHA256: strings.Repeat("0", 64)},
		{Kind: "rpc", URL: "http://127.0.0.1:1", Fallbacks: []Endpoint{{Kind: "rpc", URL: "http://127.0.0.1:1"}}},
		{Kind: "rpc", URL: "http://127.0.0.1:1", Fallbacks: []Endpoint{{Kind: "rpc", URL: "http://127.0.0.1:2", Fallbacks: []Endpoint{{}}}}},
	} {
		if p, err := NewFailover(Regtest, BTC, cfg); err == nil {
			p.Close()
			t.Fatal("accepted invalid endpoint list")
		}
	}
}

type historyBackend struct {
	*failoverBackend
	started chan struct{}
	release chan struct{}
	calls   int
}

func (b *historyBackend) Observe(ctx context.Context, _ string, _ []string) (Backend, error) {
	b.calls++
	close(b.started)
	select {
	case <-b.release:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestFailoverHistoryCompletionSurvivesPollingDeadlineAndCloseJoins(t *testing.T) {
	for _, finish := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "shutdown"}[finish], func(t *testing.T) {
			b := &historyBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}, started: make(chan struct{}), release: make(chan struct{})}
			p := testPool(b)
			p.historyCtx, p.stopHistory = context.WithCancel(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_, err := p.Observe(ctx, "wallet", []string{"deposit"})
			cancel()
			if err == nil {
				t.Fatal("unfinished history reported ready")
			}
			<-b.started
			if finish {
				close(b.release)
				if _, err := p.Observe(context.Background(), "wallet", []string{"deposit"}); err != nil {
					t.Fatal("completion discarded after polling timeout", err)
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if b.calls != 1 {
				t.Fatal("repeated historical rescan", b.calls)
			}
		})
	}
}

func TestFailoverPerChainBudgetCannotStarveHealthyChain(t *testing.T) {
	broken := testPool(&failoverBackend{height: 100, hash: "tip", blocked: true})
	broken.id = BTC
	broken.attemptBudget = time.Second
	healthy := testPool(&failoverBackend{height: 100, hash: "tip"})
	healthy.id = Blake
	ctx := WithWorkBudgets(context.Background(), 20*time.Millisecond)
	started := time.Now()
	for i := 0; i < 100; i++ {
		retryNow(broken)
		_, _ = broken.Height(ctx)
	}
	if time.Since(started) > time.Second {
		t.Fatal("per-chain work was unbounded")
	}
	if h, err := healthy.Height(ctx); err != nil || h != 100 {
		t.Fatal("unavailable chain consumed the healthy chain budget", err)
	}
}

func TestFailoverBroadcastGuardRechecksAfterEndpointSwitch(t *testing.T) {
	primary, secondary := &failoverBackend{height: 100, hash: "tip"}, &failoverBackend{height: 100, hash: "tip"}
	p := testPool(primary, secondary)
	if _, err := p.Height(context.Background()); err != nil {
		t.Fatal(err)
	}
	generation := p.Generation()
	primary.blocked = true
	guardCalls := 0
	guard := func() error {
		guardCalls++
		if p.Generation() != generation {
			return errors.New("refresh publication evidence")
		}
		return nil
	}
	if _, err := p.BroadcastGuarded(context.Background(), "same signed bytes", guard); err == nil {
		t.Fatal("guarded failover published")
	}
	if primary.broadcasts != 1 || secondary.broadcasts != 0 || guardCalls != 2 {
		t.Fatal("guard did not stop switched endpoint", primary.broadcasts, secondary.broadcasts, guardCalls)
	}
	if p.Status().Endpoints[1].Error != "" {
		t.Fatal("caller authorization failure marked healthy endpoint invalid")
	}
	// A later fresh observation can authorize the exact saved bytes on this source.
	generation = p.Generation()
	if _, err := p.BroadcastGuarded(context.Background(), "same signed bytes", guard); err != nil {
		t.Fatal(err)
	}
	if secondary.broadcasts != 1 || secondary.raw != "same signed bytes" {
		t.Fatal("authorized retry changed")
	}
}
