package daemon

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

const maxActivityRecords = 50000

func activityID(kind, key string) string { return kind + "/" + key }
func activityOutcome(a Activity) ActivityOutcome {
	outcome := ActivityOutcome{Status: a.Status, TxID: a.TxID, Amount: a.Amount, Fee: a.Fee, FeeKnown: a.FeeKnown, BlockHash: a.BlockHash, BlockTime: a.BlockTime, ObservedAt: a.ObservedAt, Source: a.Source, Generation: a.Generation}
	if outcome.ObservedAt == 0 && a.UpdatedAt > 0 {
		outcome.ObservedAt = a.UpdatedAt
		outcome.Source = "local_state"
	}
	return outcome
}
func activityMaterial(a Activity) string {
	a.ObservedAt = 0 // Routine polling is not another economic outcome.
	a.UpdatedAt = 0
	return protocol.Digest(struct {
		Outcome                                               ActivityOutcome
		Kind, Direction, LocalStatus, Classification, GroupID string
		RelatedIDs, Variants                                  []string
		Movement                                              bool
	}{activityOutcome(a), a.Kind, a.Direction, a.LocalStatus, a.Classification, a.GroupID, a.RelatedIDs, a.Variants, a.Movement})
}
func mergeActivityIDs(a, b []string) []string {
	set := map[string]bool{}
	for _, id := range append(append([]string{}, a...), b...) {
		if id != "" {
			set[id] = true
		}
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
func (e *Engine) putActivity(next Activity, backfill bool) {
	if e.s.Activities == nil {
		e.s.Activities = map[string]Activity{}
	}
	previous, exists := e.s.Activities[next.ID]
	if !exists && len(e.s.Activities) >= maxActivityRecords {
		e.s.ActivityError = "Activity history capacity reached; retained history is preserved and indexing is paused."
		return
	}
	now := time.Now().Unix()
	next.Version = 1
	next.Wallet = e.Config.Name
	next.Network = e.Config.Network
	if exists {
		next.CreatedAt, next.CreatedSource, next.RecordedAt = previous.CreatedAt, previous.CreatedSource, previous.RecordedAt
		next.Variants = mergeActivityIDs(previous.Variants, next.Variants)
		next.History = previous.History
		if next.Observations == nil {
			next.Observations = previous.Observations
		}
		values := map[string]ActivityVariant{}
		for _, v := range previous.VariantAmounts {
			values[v.TxID] = v
		}
		for _, v := range next.VariantAmounts {
			values[v.TxID] = v
		}
		next.VariantAmounts = nil
		for _, id := range next.Variants {
			if v, ok := values[id]; ok {
				next.VariantAmounts = append(next.VariantAmounts, v)
			}
		}
	} else {
		next.RecordedAt = now
		if next.CreatedAt > 0 {
			next.CreatedSource = "local_creation"
		} else if !backfill {
			next.CreatedAt = now
			next.CreatedSource = "local_first_recorded"
		} else {
			next.CreatedSource = "unknown"
		}
	}
	projectActivityObservation(&next, e.Config.Network.Confirmations(), now)
	next.UpdatedAt = previous.UpdatedAt
	if !exists || activityMaterial(previous) != activityMaterial(next) {
		if exists {
			next.History = append(append([]ActivityOutcome{}, previous.History...), activityOutcome(previous))
		}
		next.UpdatedAt = now
		e.s.ActivityRevision++
	}
	e.s.Activities[next.ID] = next
}
func (e *Engine) knownReceiveAddress(address string, id chain.ID) bool {
	for _, entry := range e.receiveBook[id] {
		if entry.address == address {
			return true
		}
	}
	return false
}
func rawActivityIDs(raws []string) []string { return transactionIDs(raws) }
func rawActivity(raws []string, id string) (int64, int64, int64, bool) {
	for _, raw := range raws {
		tx, err := contract.Parse(raw)
		if err != nil || tx.TxHash().String() != id || len(tx.TxOut) == 0 {
			continue
		}
		var total int64
		for _, out := range tx.TxOut {
			if out.Value < 0 || out.Value > contract.MaxMoney-total {
				return 0, 0, 0, false
			}
			total += out.Value
		}
		bounty := int64(0)
		if len(tx.TxOut) == 2 {
			bounty = tx.TxOut[1].Value
		}
		return tx.TxOut[0].Value, total, bounty, true
	}
	return 0, 0, 0, false
}

// Project only local lifecycle data here. This function performs no IO and runs
// before the same encrypted snapshot that commits the underlying obligation.
// Chain history enrichment has its own bounded pass after settlement work.
func (e *Engine) syncActivity() {
	backfill := e.s.ActivityVersion == 0
	for id, event := range e.s.Offers {
		var o protocol.Offer
		if json.Unmarshal([]byte(event.Content), &o) != nil {
			continue
		}
		key := activityID("order", id)
		if o.Status == "open" && o.Expires <= time.Now().Unix() {
			o.Status = "expired"
		}
		e.putActivity(Activity{ID: key, GroupID: key, Kind: "order", Chain: o.Sell, Direction: "info", Principal: o.SellAmount, CounterChain: o.Sell.Other(), CounterAmount: o.BuyAmount, OrderID: id, SwapID: o.Reservation, LocalStatus: o.Status, Status: o.Status, Label: "Order " + o.Status}, backfill)
	}
	for id, send := range e.s.Sends {
		key := activityID("send", id)
		p := send.public()
		direction := "outgoing"
		amount := p.Amount + p.Fee
		if e.knownReceiveAddress(p.Destination, p.Chain) {
			direction = "internal"
			amount = p.Fee
		}
		variants := []string{p.TxID}
		amounts := []ActivityVariant{}
		for _, v := range p.Variants {
			variants = append(variants, v.TxID)
			value := p.Amount + v.Fee
			if direction == "internal" {
				value = v.Fee
			}
			amounts = append(amounts, ActivityVariant{TxID: v.TxID, Amount: value, Principal: p.Amount, Fee: v.Fee, FeeKnown: true, FeePayer: "wallet"})
		}
		e.putActivity(Activity{ID: key, GroupID: key, Kind: "send", Chain: p.Chain, Direction: direction, Movement: true, Amount: amount, Principal: p.Amount, Fee: p.Fee, FeeKnown: true, FeePayer: "wallet", Address: p.Destination, SendID: id, TxID: p.TxID, Variants: variants, VariantAmounts: amounts, LocalStatus: p.State, Status: p.State, Confirmations: p.Confirmations, CreatedAt: send.Created, Label: "Wallet send"}, backfill)
	}
	for id, s := range e.s.Swaps {
		var o protocol.Offer
		if json.Unmarshal([]byte(s.Request.OfferEvent.Content), &o) != nil {
			continue
		}
		paid, principal, received, receive := o.Sell, o.SellAmount, o.Sell.Other(), o.BuyAmount
		if s.Role == "taker" {
			paid, principal, received, receive = received, receive, paid, principal
		}
		group := activityID("swap", id)
		e.putActivity(Activity{ID: group, GroupID: group, Kind: "swap", Chain: paid, Direction: "info", Principal: principal, CounterChain: received, CounterAmount: receive, OrderID: o.ID, SwapID: id, LocalStatus: s.Stage, Status: s.Stage, Label: "Swap " + s.Role}, backfill)
		own, incoming, ownRaw, ownSent, ownSpend, incomingSpend := s.Short, s.Long, s.ShortFunding, s.ShortSent, s.ShortSpend, s.LongSpend
		owner := "offer/" + o.ID
		if s.Role == "taker" {
			own, incoming, ownRaw, ownSent, ownSpend, incomingSpend = s.Long, s.Short, s.LongFunding, s.LongSent, s.LongSpend, s.ShortSpend
			owner = "swap/" + id
		}
		if ownRaw != "" {
			status := "prepared"
			if ownSent {
				status = "broadcast"
			}
			fee := e.fundingFee(owner)
			e.putActivity(Activity{ID: group + "/funding", GroupID: group, Kind: "swap_funding", Chain: own.Chain, Direction: "outgoing", Movement: true, Amount: own.Amount + fee, Principal: own.Amount, Fee: fee, FeeKnown: true, FeePayer: "wallet", OrderID: o.ID, SwapID: id, TxID: own.TxID, Variants: []string{own.TxID}, Outpoints: []CoinOutpoint{{TxID: own.TxID, Vout: own.Vout}}, LocalStatus: status, Status: status, Label: "Swap funding"}, backfill)
		}
		claimRaw := append([]string{}, s.SelfClaims...)
		if s.SelfClaim != "" {
			claimRaw = append(claimRaw, s.SelfClaim)
		}
		refundRaw := append([]string{}, s.SelfRefunds...)
		for _, job := range s.Jobs {
			if job.Kind == "claim" {
				claimRaw = append(claimRaw, job.Templates...)
			} else {
				refundRaw = append(refundRaw, job.Templates...)
			}
		}
		claimID := incomingSpend
		if claimID == "" && s.ClaimLastAttempt > 0 {
			claimID, _ = settlementVariant(s.SelfClaims, s.ClaimVariant, s.Long, s.Short, s.Role == "maker")
		}
		if claimID == "" && s.SelfClaim != "" {
			ids := rawActivityIDs([]string{s.SelfClaim})
			if len(ids) > 0 {
				claimID = ids[0]
			}
		}
		refundID := ownSpend
		if refundID == "" && s.RefundLastAttempt > 0 {
			refundID, _ = settlementVariant(s.SelfRefunds, s.RefundVariant, s.Long, s.Short, s.Role != "maker")
		}
		for _, leg := range []struct {
			kind, id string
			target   contract.HTLC
			raws     []string
		}{{"claim", claimID, incoming, claimRaw}, {"refund", refundID, own, refundRaw}} {
			net, total, bounty, ok := rawActivity(leg.raws, leg.id)
			if !ok {
				continue
			}
			fee := leg.target.Amount - total
			if fee < 0 {
				continue
			}
			e.putActivity(Activity{ID: group + "/" + leg.kind, GroupID: group, Kind: "swap_" + leg.kind, Chain: leg.target.Chain, Direction: "incoming", Movement: true, Amount: net, Principal: leg.target.Amount, Fee: fee, FeeKnown: true, FeePayer: "wallet", Bounty: bounty, OrderID: o.ID, SwapID: id, TxID: leg.id, Variants: rawActivityIDs(leg.raws), VariantAmounts: activityVariantAmounts(leg.raws, leg.target.Amount, false), Outpoints: []CoinOutpoint{{TxID: leg.target.TxID, Vout: leg.target.Vout}}, LocalStatus: s.Stage, Status: "broadcast", Label: "Swap " + leg.kind}, backfill)
		}
	}
	for id, state := range e.s.TowerJobs {
		if state.Broadcast == "" {
			continue
		}
		job := state.Job
		_, total, bounty, ok := rawActivity(job.Templates, state.Broadcast)
		if !ok || bounty == 0 {
			continue
		}
		status := "broadcast"
		if state.Confirmed > 0 {
			status = "confirmed"
		}
		e.putActivity(Activity{ID: activityID("tower", id), GroupID: activityID("tower", id), Kind: "tower_earning", Chain: job.Target.Chain, Direction: "incoming", Movement: true, Amount: bounty, Principal: bounty, Fee: job.Target.Amount - total, FeeKnown: true, FeePayer: "contract_owner", SwapID: job.SwapID, TxID: state.Broadcast, Variants: rawActivityIDs(job.Templates), VariantAmounts: activityVariantAmounts(job.Templates, job.Target.Amount, true), Outpoints: []CoinOutpoint{{TxID: job.Target.TxID, Vout: job.Target.Vout}}, LocalStatus: status, Status: status, Confirmations: state.Confirmed, Label: "Earned tower " + job.Kind + " bounty"}, backfill)
	}
	e.seedActivityCoins()
	e.reconcileActivityReceipts()
	e.s.ActivityVersion = 1
}

func projectActivityObservation(a *Activity, required int, now int64) {
	if len(a.Variants) == 0 {
		return
	}
	var selected *ActivityObservation
	for i := range a.Observations {
		observation := &a.Observations[i]
		current := observation.Status == "confirmed" && now-observation.ObservedAt <= 120
		if current && (selected == nil || selected.Status != "confirmed" || observation.Sequence > selected.Sequence) {
			selected = observation
		}
	}
	if selected == nil {
		for i := range a.Observations {
			if a.Observations[i].TxID == a.TxID {
				selected = &a.Observations[i]
				break
			}
		}
	}
	if selected == nil {
		if a.Status == "confirmed" {
			a.Status = "unverified"
		}
		return
	}
	a.Status = selected.Status
	a.Confirmations = selected.Confirmations
	a.BlockHash = selected.BlockHash
	a.BlockTime = selected.BlockTime
	a.ObservedAt = selected.ObservedAt
	a.Source = selected.Source
	a.Generation = selected.Generation
	if now-selected.ObservedAt > 120 {
		a.Status = "unknown"
	}
	if a.Status == "confirmed" && a.Confirmations < required {
		a.Status = "confirming"
	}
	if a.Status == "confirmed" || a.Status == "confirming" || a.Status == "mempool" {
		a.TxID = selected.TxID
		for _, v := range a.VariantAmounts {
			if v.TxID == a.TxID {
				a.Amount = v.Amount
				a.Principal = v.Principal
				a.Fee = v.Fee
				a.FeeKnown = v.FeeKnown
				a.FeePayer = v.FeePayer
				a.Bounty = v.Bounty
			}
		}
	}
}

func activityVariantAmounts(raws []string, principal int64, tower bool) []ActivityVariant {
	var result []ActivityVariant
	for _, id := range rawActivityIDs(raws) {
		net, total, bounty, ok := rawActivity(raws, id)
		if !ok || principal < total {
			continue
		}
		v := ActivityVariant{TxID: id, Amount: net, Principal: principal, Fee: principal - total, FeeKnown: true, FeePayer: "wallet", Bounty: bounty}
		if tower {
			v.Amount = bounty
			v.Principal = bounty
			v.Bounty = 0
			v.FeePayer = "contract_owner"
		}
		result = append(result, v)
	}
	return result
}
