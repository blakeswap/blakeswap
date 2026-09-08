package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Capture one committed active checkpoint and archive generation under the
// engine lock. Selected-category reads, private sorting, filtering and page
// serialization run outside it; no live database transaction spans a callback.
func (e *Engine) historyCommand(ctx context.Context, request Request) (result any, resultErr error) {
	release, err := e.lockHistory(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	e.mu.Lock()
	if e.activityClosed || e.fatal != nil {
		err := e.fatal
		if err == nil {
			err = errEngineClosed
		}
		e.mu.Unlock()
		return nil, err
	}
	view := &Engine{
		Config: e.Config, identity: e.identity, vault: e.vault, nodes: maps.Clone(e.nodes),
		chainFresh: maps.Clone(e.chainFresh), chainGeneration: maps.Clone(e.chainGeneration),
		archiveCurrent: maps.Clone(e.archiveCurrent), recoveryCheckpoints: maps.Clone(e.recoveryCheckpoints), recoveryReconciled: maps.Clone(e.recoveryReconciled),
		marketObservedAt: e.marketObservedAt, marketAllRelays: e.marketAllRelays,
		activitySnapshots: maps.Clone(e.activitySnapshots), activitySnapshotSequence: e.activitySnapshotSequence,
	}
	sourceGenerations := e.historySourceGenerations()
	var query ActivityQuery
	var market MarketQuery
	var fills FillQuery
	if request.Method == "market.list" {
		market, err = view.parseMarketQuery(request.Params)
	} else if request.Method == "fills.list" {
		fills, err = view.parseFillQuery(request.Params)
	} else {
		query, err = view.parseActivityQuery(request.Params)
	}
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	var source *storage.PageSnapshot
	freshQuery := query.Snapshot == "" && fills.Revision == ""
	if freshQuery || request.Method == "market.list" {
		if e.vault != nil {
			_, source, err = e.vault.CaptureArchive("", false)
		} else {
			var raw []byte
			raw, err = json.Marshal(e.s)
			if err == nil {
				err = json.Unmarshal(raw, &view.s)
			}
			clear(raw)
		}
	}
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	e.historyCancel = cancel
	view.historyContext = ctx
	e.activityReaders.Add(1)
	e.mu.Unlock()
	defer e.activityReaders.Done()
	defer cancel()
	if source != nil {
		defer source.Close()
		stats, _, err := source.LoadState(&view.s)
		if err != nil {
			return nil, err
		}
		if err := view.s.ValidateArchiveCheckpoint(stats); err != nil {
			return nil, err
		}
		view.archiveRead = source.ReadArchive
	}
	// Even a failed later page must retire expired/replaced private files. Close
	// joins this reader before disposing its result keys or the vault credential.
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if resultErr != nil {
			for id, snapshot := range view.activitySnapshots {
				if snapshot.Sequence > e.activitySnapshotSequence {
					if snapshot.Rows != nil {
						_ = snapshot.Rows.Close()
					}
					delete(view.activitySnapshots, id)
				}
			}
		}
		if e.activityClosed {
			for _, snapshot := range view.activitySnapshots {
				if snapshot.Rows != nil {
					_ = snapshot.Rows.Close()
				}
			}
			e.activitySnapshots = nil
		} else {
			if resultErr == nil {
				// Keep the one unpublished result alongside existing revisions
				// until every source/context fence has succeeded. A failed fifth
				// query must not destroy any still-valid published result.
				view.trimHistorySnapshots(4)
			}
			e.activitySnapshots = view.activitySnapshots
			e.activitySnapshotSequence = view.activitySnapshotSequence
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Method == "fills.list" {
		result, err = view.fillPage(ctx, source, fills)
	} else if e.vault == nil {
		switch request.Method {
		case "market.list":
			result, err = view.marketPage(request.Params)
		case "activity.list":
			result, err = view.activityPage(request.Params)
		case "activity.export":
			result, err = view.exportActivity(request.Params)
		}
	} else if request.Method == "market.list" {
		result, err = view.streamMarketPage(ctx, source, market)
	} else {
		if query.Snapshot == "" {
			query, err = view.freezeActivityRows(ctx, source, query)
		}
		if err == nil {
			var raw []byte
			raw, err = json.Marshal(query)
			if err == nil {
				if request.Method == "activity.export" {
					result, err = view.exportActivity(raw)
				} else {
					result, err = view.activityPage(raw)
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if view.archiveReadError != nil {
		return nil, view.archiveReadError
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.activityClosed {
		return nil, errEngineClosed
	}
	if e.Config.Name != view.Config.Name || e.Config.Network != view.Config.Network {
		return nil, errors.New("history wallet or network changed")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if e.chainGeneration[id] != view.chainGeneration[id] || e.chainFresh[id] != view.chainFresh[id] {
			return nil, errors.New("history chain context changed; refresh the result")
		}
	}
	if !maps.Equal(sourceGenerations, e.historySourceGenerations()) {
		return nil, errors.New("history chain source changed; refresh the result")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Engine) trimHistorySnapshots(limit int) {
	for len(e.activitySnapshots) > limit {
		oldest := ""
		for id, snapshot := range e.activitySnapshots {
			if oldest == "" || snapshot.Sequence < e.activitySnapshots[oldest].Sequence {
				oldest = id
			}
		}
		if rows := e.activitySnapshots[oldest].Rows; rows != nil {
			_ = rows.Close()
		}
		delete(e.activitySnapshots, oldest)
	}
}

// A canceled query need not wait for an unrelated full-history sort/report.
// The channel owns result-cache access; the engine mutex only creates the gate.
func (e *Engine) lockHistory(ctx context.Context) (func(), error) {
	e.mu.Lock()
	if e.historyGate == nil {
		e.historyGate = make(chan struct{}, 1)
	}
	gate := e.historyGate
	e.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Connection stability is independent of positive wallet observation. Generation
// zero is a valid unchanged offline source, but never a confirmation proof.
// Call under Engine.mu so the configured provider set belongs to one view.
func (e *Engine) historySourceGenerations() map[chain.ID]uint64 {
	generations := map[chain.ID]uint64{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if source, ok := e.nodes[id].(interface{ Generation() uint64 }); ok {
			generations[id] = source.Generation()
		}
	}
	return generations
}
