package chain

import (
	"context"
	"errors"
)

type historyLease struct {
	pool       *Failover
	active     int
	generation uint64
	held       bool
}

func (l *historyLease) release() {
	if l.held {
		<-l.pool.gate
		l.held = false
	}
}
func (l *historyLease) acquire(ctx context.Context) error {
	select {
	case l.pool.gate <- struct{}{}:
		l.held = true
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		l.release()
		return err
	}
	return nil
}

// The protocol lock order is Engine.mu then pool.gate. A durable history sink
// needs Engine.mu, so release this lease while delivering it synchronously. No
// backend state is read until the gate is reacquired and admission rechecked.
func (l *historyLease) witnessContext(ctx context.Context) context.Context {
	if ctx.Value(witnessSinkKey{}) == nil {
		return ctx
	}
	return WithSpendWitnessSink(ctx, func(w SpendWitness) error {
		l.release()
		if err := emitSpendWitness(ctx, w.Outpoint, w.Tx); err != nil {
			return err
		}
		if err := l.acquire(ctx); err != nil {
			return err
		}
		p := l.pool
		if p.active != l.active || p.Generation() != l.generation || !p.entries[l.active].validated {
			return errors.New("history source changed during witness persistence")
		}
		return nil
	})
}

// Borrow an admitted endpoint without changing routing, validation, imports,
// health or work budgets. Engine closure cancels and joins these advisory reads.
func (p *Failover) historyEntry(ctx context.Context, wallet bool) (*endpointEntry, *historyLease, error) {
	lease := &historyLease{pool: p}
	if err := lease.acquire(ctx); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	active, generation := p.active, p.generation
	p.mu.Unlock()
	if active < 0 || active >= len(p.entries) || generation == 0 || !p.entries[active].validated {
		lease.release()
		return nil, nil, errors.New("history requires a validated active chain endpoint")
	}
	entry := &p.entries[active]
	if wallet && (entry.watch == nil || p.name == "" || entry.observed != len(p.addresses)) {
		lease.release()
		return nil, nil, errors.New("wallet history is not ready on the active endpoint")
	}
	lease.active, lease.generation = active, generation
	return entry, lease, nil
}
func (p *Failover) AddressHistory(ctx context.Context, address, after string, limit int) (AddressHistoryPage, error) {
	entry, lease, err := p.historyEntry(ctx, true)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	defer lease.release()
	registered := false
	for _, known := range p.addresses {
		registered = registered || address == known
	}
	if !registered {
		return AddressHistoryPage{}, errors.New("history address is not registered in this wallet")
	}
	backend, ok := entry.watch.(AddressHistorian)
	if !ok {
		return AddressHistoryPage{}, errors.New("active backend does not support historical receipts")
	}
	source, generation := historySource(entry.health.Kind, entry.health.URL), lease.generation
	page, err := backend.AddressHistory(lease.witnessContext(ctx), address, after, limit)
	page.Source, page.Generation = source, generation
	if ctx.Err() != nil {
		return AddressHistoryPage{Source: source, Generation: generation}, ctx.Err()
	}
	return page, err
}
func (p *Failover) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (HistoryTransaction, error) {
	entry, lease, err := p.historyEntry(ctx, false)
	if err != nil {
		return HistoryTransaction{}, err
	}
	defer lease.release()
	backend, ok := entry.backend.(HistoryObserver)
	if !ok {
		return HistoryTransaction{}, errors.New("active backend does not support historical transaction observations")
	}
	source, generation := historySource(entry.health.Kind, entry.health.URL), lease.generation
	result, err := backend.HistoryTransaction(lease.witnessContext(ctx), id, height, block)
	result.Source, result.Generation = source, generation
	if ctx.Err() != nil {
		return HistoryTransaction{Source: source, Generation: generation}, ctx.Err()
	}
	return result, err
}
