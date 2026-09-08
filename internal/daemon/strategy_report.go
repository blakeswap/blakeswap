package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
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
	owned := map[string]strategyReportSwap{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for order, c := range e.s.Automations[strategyPolicyID(q.ID, id)].Charges {
			if proof := c.ExposureSettled; proof != nil && !proof.Held {
				owned[proof.SwapID] = strategyReportSwap{order: order, sell: proof.Sell, funding: proof.FundingTxID, ownSpend: proof.SpendTxID, peerSpend: proof.PeerSpendTxID, settled: true}
			}
		}
	}
	for id, swap := range e.s.Swaps {
		// A live record supersedes any prior archived proof, including after a
		// reorg. An order ID alone cannot identify the maker of a foreign swap.
		delete(owned, id)
		o, err := historicalOffer(swap.Request.OfferEvent)
		if err != nil || swap.Role != "maker" || o.Maker != e.identity.Public().Hex() {
			continue
		}
		child := e.s.Automations[strategyPolicyID(q.ID, o.Sell)]
		if child == nil || child.Charges[o.ID] == nil {
			continue
		}
		owned[id] = strategyReportSwap{order: o.ID, sell: o.Sell, funding: swap.Short.TxID, ownSpend: swap.ShortSpend, peerSpend: swap.LongSpend, settled: swap.Stage == "completed" && e.strategyVerifiedSwaps[id] && strategySettled(swap, e.Config.Network.Confirmations())}
	}
	revision := p.Revision
	generations := map[chain.ID]uint64{chain.BTC: e.chainGeneration[chain.BTC], chain.Blake: e.chainGeneration[chain.Blake]}
	e.mu.Unlock()
	funding, claims := map[string]Activity{}, map[string]Activity{}
	seen := map[string]bool{}
	now := time.Now().Unix()
	err := e.visitStrategyActivities(ctx, func(a Activity) error {
		identity, ok := owned[a.SwapID]
		if !ok || !identity.matches(a) {
			return nil
		}
		if a.Status != "confirmed" || a.Confirmations < chain.Network(q.ExpectedNetwork).Confirmations() || a.ObservedAt <= 0 || a.ObservedAt > now || now-a.ObservedAt > 120 {
			return nil
		}
		if a.Kind == "swap_funding" {
			funding[a.SwapID] = a
		}
		if a.Kind == "swap_claim" && identity.settled {
			claims[a.SwapID] = a
		}
		key := string(a.Chain) + "/" + a.TxID
		if seen[key] || a.FeePayer != "wallet" || !a.Movement {
			return nil
		}
		seen[key] = true
		i := v.Inventory[a.Chain]
		if a.Fee < 0 || a.Bounty < 0 || a.Fee > contract.MaxMoney-i.KnownFees || a.Bounty > contract.MaxMoney-i.KnownBounties {
			return errors.New("strategy report fee total exceeds supported native-asset range")
		}
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
			if f.Principal < 0 || f.Principal > contract.MaxMoney-i.ConfirmedVolume {
				return StrategyView{}, errors.New("strategy report volume exceeds supported native-asset range")
			}
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

// Keep exact chain/transaction identities so an unrelated maker reusing an order
// ID, or a superseded claim/refund variant, cannot enter this strategy's report.
type strategyReportSwap struct {
	order, funding, ownSpend, peerSpend string
	sell                                chain.ID
	settled                             bool
}

func (s strategyReportSwap) matches(a Activity) bool {
	if a.OrderID != s.order || a.TxID == "" {
		return false
	}
	switch a.Kind {
	case "swap_funding":
		return a.Chain == s.sell && a.TxID == s.funding
	case "swap_claim":
		return a.Chain == s.sell.Other() && a.TxID == s.peerSpend
	case "swap_refund":
		return a.Chain == s.sell && a.TxID == s.ownSpend
	}
	return false
}
