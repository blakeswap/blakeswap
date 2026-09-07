package chain

import (
	"context"
	"errors"
)

// historyEntry borrows an already admitted endpoint without changing routing,
// validation, wallet imports, health or work budgets. The advisory caller owns
// its deadline; Engine.Close cancels and joins these reads before pool closure.
func (p *Failover) historyEntry(ctx context.Context, wallet bool) (*endpointEntry, uint64, func(), error) {
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, nil, ctx.Err()
	}
	release := func() { <-p.gate }
	if err := ctx.Err(); err != nil {
		release()
		return nil, 0, nil, err
	}
	p.mu.Lock()
	active, generation := p.active, p.generation
	p.mu.Unlock()
	if active < 0 || active >= len(p.entries) || generation == 0 || !p.entries[active].validated {
		release()
		return nil, 0, nil, errors.New("history requires a validated active chain endpoint")
	}
	entry := &p.entries[active]
	if wallet && (entry.watch == nil || p.name == "" || entry.observed != len(p.addresses)) {
		release()
		return nil, 0, nil, errors.New("wallet history is not ready on the active endpoint")
	}
	return entry, generation, release, nil
}

func (p *Failover) AddressHistory(ctx context.Context, address, after string, limit int) (AddressHistoryPage, error) {
	entry, generation, release, err := p.historyEntry(ctx, true)
	if err != nil {
		return AddressHistoryPage{}, err
	}
	defer release()
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
	page, err := backend.AddressHistory(ctx, address, after, limit)
	page.Source, page.Generation = historySource(entry.health.Kind, entry.health.URL), generation
	if ctx.Err() != nil {
		return AddressHistoryPage{Source: page.Source, Generation: generation}, ctx.Err()
	}
	return page, err
}

func (p *Failover) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (HistoryTransaction, error) {
	entry, generation, release, err := p.historyEntry(ctx, false)
	if err != nil {
		return HistoryTransaction{}, err
	}
	defer release()
	backend, ok := entry.backend.(HistoryObserver)
	if !ok {
		return HistoryTransaction{}, errors.New("active backend does not support historical transaction observations")
	}
	result, err := backend.HistoryTransaction(ctx, id, height, block)
	result.Source, result.Generation = historySource(entry.health.Kind, entry.health.URL), generation
	if ctx.Err() != nil {
		return HistoryTransaction{Source: result.Source, Generation: generation}, ctx.Err()
	}
	return result, err
}
