package chain

import (
	"context"
	"sync"
	"time"
)

type workBudgetKey struct{}
type workBudgets struct {
	mu        sync.Mutex
	remaining map[ID]time.Duration
}

// WithWorkBudgets caps cumulative backend work on EACH chain in this cycle.
// Relay time and the other chain's work do not consume a healthy chain's budget.
// Advisory reads have their own contexts and cannot consume settlement budgets.
func WithWorkBudgets(ctx context.Context, allowance time.Duration) context.Context {
	return context.WithValue(ctx, workBudgetKey{}, &workBudgets{remaining: map[ID]time.Duration{BTC: allowance, Blake: allowance}})
}
func boundedChainWork(ctx context.Context, id ID) (context.Context, func()) {
	budgets, ok := ctx.Value(workBudgetKey{}).(*workBudgets)
	if !ok {
		return ctx, func() {}
	}
	budgets.mu.Lock()
	remaining := budgets.remaining[id]
	budgets.mu.Unlock()
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, max(remaining, 0))
	return bounded, func() {
		cancel()
		budgets.mu.Lock()
		budgets.remaining[id] = max(0, budgets.remaining[id]-time.Since(started))
		budgets.mu.Unlock()
	}
}
