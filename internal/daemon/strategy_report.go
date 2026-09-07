package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
)

// The report is explicit advisory work; it never runs on Tick, save or review.
// Legacy active history is bounded by maxActivityRecords. The visitor boundary
// lets the archive store stream a pinned committed view without reactivation.
func (e *Engine) visitStrategyActivities(ctx context.Context, visit func(Activity) error) error {
	e.mu.Lock()
	if e.fatal != nil || e.activityClosed {
		e.mu.Unlock()
		return errEngineClosed
	}
	e.activityReaders.Add(1)
	defer e.activityReaders.Done()
	rows := make([]Activity, 0, len(e.s.Activities))
	for _, a := range e.s.Activities {
		// Copy only scalar accounting facts, not lifetime outcomes/raw variants.
		projectActivityObservation(&a, e.Config.Network.Confirmations(), time.Now().Unix())
		if a.Generation > 0 && !e.activitySourceCurrent(a.Chain, a.Generation) {
			a.Status = "unknown"
			a.Confirmations = 0
		}
		a.Observations = nil
		a.History = nil
		a.Variants = nil
		a.VariantAmounts = nil
		a.Outpoints = nil
		a.RelatedIDs = nil
		rows = append(rows, a)
	}
	e.mu.Unlock()
	for _, a := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(a); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil || e.activityClosed {
		return errEngineClosed
	}
	return ctx.Err()
}

// Partial or changed-context results are discarded in their entirety. This is
// reporting, never a source of spendable coins or authorization replenishment.
func (e *Engine) strategyReport(ctx context.Context, raw json.RawMessage) (StrategyView, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedWallet   string `json:"expected_wallet"`
		ExpectedNetwork  string `json:"expected_network"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return StrategyView{}, err
	}
	if !e.strategyReporting.CompareAndSwap(false, true) {
		return StrategyView{}, errors.New("strategy report already running; retry shortly")
	}
	defer e.strategyReporting.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e.mu.Lock()
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		e.mu.Unlock()
		return StrategyView{}, err
	}
	p := e.s.MakerStrategies[q.ID]
	if p == nil || p.Revision != q.ExpectedRevision {
		e.mu.Unlock()
		return StrategyView{}, errors.New("strategy changed; refresh before reporting")
	}
	v := e.strategyView(p)
	owned := map[string]bool{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for order := range e.s.Automations[strategyPolicyID(q.ID, id)].Charges {
			owned[order] = true
		}
	}
	revision := p.Revision
	generations := map[chain.ID]uint64{chain.BTC: e.chainGeneration[chain.BTC], chain.Blake: e.chainGeneration[chain.Blake]}
	e.mu.Unlock()
	funding, claims := map[string]Activity{}, map[string]Activity{}
	seen := map[string]bool{}
	now := time.Now().Unix()
	err := e.visitStrategyActivities(ctx, func(a Activity) error {
		if !owned[a.OrderID] || a.TxID == "" {
			return nil
		}
		if a.Status != "confirmed" || a.Confirmations < chain.Network(q.ExpectedNetwork).Confirmations() || a.ObservedAt <= 0 || a.ObservedAt > now || now-a.ObservedAt > 120 {
			return nil
		}
		if a.Kind == "swap_funding" {
			funding[a.SwapID] = a
		}
		if a.Kind == "swap_claim" {
			claims[a.SwapID] = a
		}
		key := string(a.Chain) + "/" + a.TxID
		if seen[key] || a.FeePayer != "wallet" || !a.Movement {
			return nil
		}
		seen[key] = true
		i := v.Inventory[a.Chain]
		if a.FeeKnown {
			i.KnownFees += a.Fee
		} else {
			i.UnknownFees++
		}
		i.KnownBounties += a.Bounty
		v.Inventory[a.Chain] = i
		return nil
	})
	if err != nil {
		return StrategyView{}, err
	}
	for id, f := range funding {
		if _, ok := claims[id]; ok {
			i := v.Inventory[f.Chain]
			i.ConfirmedVolume += f.Principal
			v.Inventory[f.Chain] = i
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p = e.s.MakerStrategies[q.ID]
	if ctx.Err() != nil {
		return StrategyView{}, ctx.Err()
	}
	if e.fatal != nil || e.activityClosed || p == nil || p.Revision != revision || e.Config.Name != q.ExpectedWallet || string(e.Config.Network) != q.ExpectedNetwork || e.chainGeneration[chain.BTC] != generations[chain.BTC] || e.chainGeneration[chain.Blake] != generations[chain.Blake] {
		return StrategyView{}, errors.New("wallet or chain source changed; discard the old report")
	}
	v.ReportIncluded = true
	return v, nil
}
