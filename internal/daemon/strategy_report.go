package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

// The report uses the same committed selected-kind visitor as history. Old
// timestamps remain intact: only independently verified archive coverage can
// make a cold row eligible after its ordinary polling freshness has elapsed.
func (e *Engine) projectStrategyActivity(a Activity) Activity {
	projectActivityObservation(&a, e.Config.Network.Confirmations(), time.Now().Unix())
	if a.Generation > 0 && !e.activitySourceCurrent(a.Chain, a.Generation) {
		a.Status = "unknown"
		a.Confirmations = 0
	}
	return a
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
	uncertain := map[string]strategyReportSwap{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for order, c := range e.s.Automations[strategyPolicyID(q.ID, id)].Charges {
			if proof := c.ExposureSettled; proof != nil {
				identity := strategyReportSwap{order: order, sell: proof.Sell, funding: proof.FundingTxID, ownSpend: proof.SpendTxID, peerSpend: proof.PeerSpendTxID, settled: true}
				if !proof.Held {
					owned[proof.SwapID] = identity
				} else {
					uncertain[proof.SwapID] = identity
				}
			}
		}
	}
	for id, swap := range e.s.Swaps {
		// A live record supersedes any prior archived proof, including after a
		// reorg. An order ID alone cannot identify the maker of a foreign swap.
		delete(owned, id)
		delete(uncertain, id)
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
	token := BackupSemanticToken(e.s)
	generations := map[chain.ID]uint64{chain.BTC: e.chainGeneration[chain.BTC], chain.Blake: e.chainGeneration[chain.Blake]}
	e.mu.Unlock()
	funding, claims := map[string]Activity{}, map[string]Activity{}
	seen := map[string]bool{}
	now := time.Now().Unix()
	monitoring, err := e.VisitActivities(ctx, func(a Activity, archived bool) error {
		a.Observations = nil
		a.History = nil
		a.Variants = nil
		a.VariantAmounts = nil
		a.Outpoints = nil
		a.RelatedIDs = nil
		identity, ok := owned[a.SwapID]
		if held, found := uncertain[a.SwapID]; archived && found && held.matches(a) {
			return errors.New("strategy history is incomplete: retained settlement ownership requires fresh recovery proof")
		}
		if !ok || !identity.matches(a) {
			return nil
		}
		if archived && !a.ArchiveVerified {
			return errors.New("strategy history is incomplete: archived inclusion has not been verified against the current source")
		}
		if a.Status != "confirmed" || a.Confirmations < chain.Network(q.ExpectedNetwork).Confirmations() || a.ObservedAt <= 0 || a.ObservedAt > now || (!archived && now-a.ObservedAt > 120) {
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
	if monitoring.Reactivating || len(monitoring.Invalidated) != 0 || monitoring.SemanticToken != token {
		return StrategyView{}, errors.New("strategy history or authority changed; discard the incomplete report")
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
	if e.fatal != nil || e.activityClosed || BackupSemanticToken(e.s) != token || ArchiveMonitoring(e.s).Revision != monitoring.Revision || e.archiveHold() != nil || p == nil || p.Revision != revision || e.Config.Name != q.ExpectedWallet || string(e.Config.Network) != q.ExpectedNetwork || e.chainGeneration[chain.BTC] != generations[chain.BTC] || e.chainGeneration[chain.Blake] != generations[chain.Blake] {
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
