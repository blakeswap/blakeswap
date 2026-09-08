package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// VisitActivities is an explicit cancellable report scan, never a Tick helper.
// It captures one committed wallet view, reads cold records in short transactions, and never
// reactivates them. Rows use the same current-source rule as paged history; old
// positive outcomes remain in History when the current source is unavailable.
// The returned monitoring projection belongs to that same committed view. A
// caller must discard partial aggregates on any error or relevant recovery hold.
func (e *Engine) VisitActivities(ctx context.Context, visit func(Activity, bool) error) (ArchiveMonitoringState, error) {
	e.historyMu.Lock()
	defer e.historyMu.Unlock()
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
	contextView := &Engine{Config: e.Config, nodes: e.nodes, chainFresh: maps.Clone(e.chainFresh), chainGeneration: maps.Clone(e.chainGeneration)}
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
	emit := func(a Activity, archived bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.Wallet = contextView.Config.Name
		if a.Generation > 0 && !contextView.activitySourceCurrent(a.Chain, a.Generation) {
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
	return ArchiveMonitoring(active), nil
}
