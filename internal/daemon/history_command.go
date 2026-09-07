package daemon

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// Freeze a committed database view under the short engine lock, then assemble,
// filter and serialize lifetime history outside it. Snapshot-page requests use
// their already frozen rows and never reread archive bodies. Close joins these
// readers before closing backend/vault resources.
func (e *Engine) historyCommand(ctx context.Context, request Request) (any, error) {
	e.historyMu.Lock()
	defer e.historyMu.Unlock()
	var query ActivityQuery
	if request.Method != "market.list" {
		if err := json.Unmarshal(request.Params, &query); err != nil {
			return nil, err
		}
	}
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
		Config: e.Config, identity: e.identity, vault: e.vault, nodes: e.nodes,
		chainFresh: maps.Clone(e.chainFresh), chainGeneration: maps.Clone(e.chainGeneration),
		archiveCurrent: maps.Clone(e.archiveCurrent), recoveryCheckpoints: maps.Clone(e.recoveryCheckpoints), recoveryReconciled: maps.Clone(e.recoveryReconciled),
		marketObservedAt: e.marketObservedAt, marketAllRelays: e.marketAllRelays,
		activitySnapshots: e.activitySnapshots, activitySnapshotSequence: e.activitySnapshotSequence,
	}
	var frozen *storage.ReadSnapshot
	var err error
	if query.Snapshot == "" || request.Method == "market.list" {
		if e.vault != nil {
			frozen, err = e.vault.Freeze()
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
	e.activityReaders.Add(1)
	e.mu.Unlock()
	defer e.activityReaders.Done()
	if frozen != nil {
		records, _, readErr := frozen.LoadComplete(&view.s, 256<<20)
		closeErr := frozen.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		view.s.Archive = records
		view.s, err = CompleteState(view.s)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result any
	switch request.Method {
	case "market.list":
		result, err = view.marketPage(request.Params)
	case "activity.list":
		result, err = view.activityPage(request.Params)
	case "activity.export":
		result, err = view.exportActivity(request.Params)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.activityClosed {
		return nil, errEngineClosed
	}
	e.activitySnapshots, e.activitySnapshotSequence = view.activitySnapshots, view.activitySnapshotSequence
	return result, err
}
