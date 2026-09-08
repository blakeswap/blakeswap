package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"maps"
	"slices"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// VisitActivities is an explicit cancellable report scan, never a Tick helper.
// It captures one committed wallet view, reads cold records in short transactions, and never
// reactivates them. Rows use the same current-source rule as paged history; old
// positive outcomes remain in History when the current source is unavailable.
// The returned monitoring projection belongs to that same committed view. A
// caller must discard partial aggregates on any error or relevant recovery hold.
func (e *Engine) VisitActivities(ctx context.Context, visit func(Activity, bool) error) (ArchiveMonitoringState, error) {
	release, err := e.lockHistory(ctx)
	if err != nil {
		return ArchiveMonitoringState{}, err
	}
	defer release()
	e.mu.Lock()
	if e.activityClosed || e.fatal != nil {
		err := e.fatal
		if err == nil {
			err = errEngineClosed
		}
		e.mu.Unlock()
		return ArchiveMonitoringState{}, err
	}
	if e.vault == nil {
		e.mu.Unlock()
		return ArchiveMonitoringState{}, errors.New("activity report requires a durable wallet checkpoint")
	}
	_, view, err := e.vault.CaptureArchive("", false)
	if err != nil {
		e.mu.Unlock()
		return ArchiveMonitoringState{}, err
	}
	contextView := &Engine{Config: e.Config, nodes: maps.Clone(e.nodes), chainFresh: maps.Clone(e.chainFresh), chainGeneration: maps.Clone(e.chainGeneration), chainObserved: maps.Clone(e.chainObserved), archiveCurrent: maps.Clone(e.archiveCurrent)}
	sourceGenerations := e.historySourceGenerations()
	ctx, cancel := context.WithCancel(ctx)
	e.historyCancel = cancel
	defer cancel()
	e.activityReaders.Add(1)
	e.mu.Unlock()
	defer e.activityReaders.Done()
	defer view.Close()
	var active State
	stats, _, err := view.LoadState(&active)
	if err != nil {
		return ArchiveMonitoringState{}, err
	}
	if err = active.ValidateArchiveCheckpoint(stats); err != nil {
		return ArchiveMonitoringState{}, err
	}
	contextView.s = active
	monitoring := ArchiveMonitoring(active)
	coverage := map[chain.ID]HistoryCoverage{}
	if !monitoring.Reactivating && len(monitoring.Invalidated) == 0 && active.Capacity != nil {
		for id, prefix := range active.Capacity.HistoryCoverage {
			if contextView.historyCoverageCurrent(ctx, prefix, id) {
				coverage[id] = prefix
			}
		}
	}
	emit := func(a Activity, archived bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.Wallet = contextView.Config.Name
		a.ArchiveVerified = archived && activityCovered(a, coverage[a.Chain])
		if !archived {
			a = contextView.projectStrategyActivity(a)
		}
		if !a.ArchiveVerified && a.Generation > 0 && !contextView.activitySourceCurrent(a.Chain, a.Generation) {
			a.History = append(append([]ActivityOutcome{}, a.History...), activityOutcome(a))
			a.Status = "unknown"
			a.Confirmations = 0
		}
		return visit(a, archived)
	}
	for _, activity := range active.Activities {
		if err = emit(activity, false); err != nil {
			return ArchiveMonitoringState{}, err
		}
	}
	err = view.VisitArchiveKind(ctx, "activities", func(record storage.ArchiveRecord) error {
		if record.Kind != "activities" {
			return nil
		}
		if _, duplicate := active.Activities[record.ID]; duplicate {
			return errors.New("activity appears in both active and archived ownership")
		}
		var activity Activity
		if err := json.Unmarshal(record.Data, &activity); err != nil {
			return err
		}
		if activity.ID != record.ID || activity.Network.Normalized() != active.Network.Normalized() {
			return errors.New("archived activity identity or network mismatch")
		}
		return emit(activity, true)
	})
	if err != nil {
		return ArchiveMonitoringState{}, err
	}
	if err := view.Check(ctx); err != nil {
		return ArchiveMonitoringState{}, err
	}
	for id, prefix := range coverage {
		if !contextView.historyCoverageCurrent(ctx, prefix, id) {
			return ArchiveMonitoringState{}, errors.New("history canonical coverage changed during scan")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current := ArchiveMonitoring(e.s)
	if e.activityClosed || e.fatal != nil {
		return ArchiveMonitoringState{}, errEngineClosed
	}
	if e.Config.Name != contextView.Config.Name || e.Config.Network != contextView.Config.Network || current.Revision != monitoring.Revision || current.Reactivating != monitoring.Reactivating || !slices.Equal(current.Invalidated, monitoring.Invalidated) {
		return ArchiveMonitoringState{}, errors.New("history wallet or archive context changed during scan")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if e.chainGeneration[id] != contextView.chainGeneration[id] || e.chainFresh[id] != contextView.chainFresh[id] {
			return ArchiveMonitoringState{}, errors.New("history source changed during scan")
		}
	}
	if !maps.Equal(sourceGenerations, e.historySourceGenerations()) {
		return ArchiveMonitoringState{}, errors.New("history source changed during scan")
	}
	return monitoring, nil
}
