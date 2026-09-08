package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"strings"
	"time"
)

// Compact lookup indexes let a newly discovered receipt find an archived parent
// payment or an old owned input without scanning lifetime history in Tick.
func (e *Engine) indexActivityIdentity(a Activity) {
	if e.s.ActivityTransactions == nil {
		e.s.ActivityTransactions = map[string]string{}
	}
	if e.s.ActivityOwned == nil {
		e.s.ActivityOwned = map[string]int64{}
	}
	if a.Kind == "receive" {
		for _, point := range a.Outpoints {
			key := string(a.Chain) + "/" + pointKey(point)
			if _, ok := e.s.ActivityOwned[key]; ok {
				continue
			}
			var amount int64
			found, err := e.archivedValue("activity_owned", key, &amount)
			if err != nil {
				e.fatal = err
				return
			}
			if !found {
				e.s.ActivityOwned[key] = a.Principal
			}
		}
	} else {
		for _, txid := range a.Variants {
			key := string(a.Chain) + "/" + txid
			if _, ok := e.s.ActivityTransactions[key]; ok {
				continue
			}
			var id string
			found, err := e.archivedValue("activity_transactions", key, &id)
			if err != nil {
				e.fatal = err
				return
			}
			if !found {
				e.s.ActivityTransactions[key] = a.ID
			}
		}
	}
}
func (e *Engine) archivedActivityParent(key string) (Activity, bool) {
	id := e.s.ActivityTransactions[key]
	if id == "" {
		_, err := e.archivedValue("activity_transactions", key, &id)
		if err != nil {
			e.fatal = err
			return Activity{}, false
		}
	}
	if id == "" {
		return Activity{}, false
	}
	if a, ok := e.s.Activities[id]; ok {
		return a, true
	}
	var a Activity
	found, err := e.archivedValue("activities", id, &a)
	if err != nil {
		e.fatal = err
		return Activity{}, false
	}
	return a, found
}
func (e *Engine) archivedActivityOwns(key string) bool {
	if _, ok := e.s.ActivityOwned[key]; ok {
		return true
	}
	var amount int64
	found, err := e.archivedValue("activity_owned", key, &amount)
	if err != nil {
		e.fatal = err
	}
	return found
}

// History is advisory. Preserve already recorded outcomes and stop adding rows
// before it consumes settlement reserves. Witness discovery happens separately
// before this check, and signed obligations always keep their normal save path.
func (e *Engine) allowActivityGrowth(previous, next any) bool {
	before, _ := json.Marshal(previous)
	after, _ := json.Marshal(next)
	growth := max(0, len(after)-len(before)) + 256
	clear(before)
	clear(after)
	stored := capacitySum(e.stateBytes, e.activityGrowth)
	if !e.capacityDiskKnown {
		e.refreshCapacityDisk()
	}
	diskNeed := capacitySum(e.capacityHealth().ReservedBytes, 16<<20, capacityProduct(uint64(growth), 4))
	if capacitySum(stored, uint64(growth)) > admissionByteCeiling || (e.vault != nil && (!e.capacityDiskKnown || e.capacityDiskAvailable < diskNeed)) {
		e.s.ActivityError = "Historical indexing is paused by active working capacity or available disk. Existing rows and signed settlement evidence are retained; export a complete portable backup."
		return false
	}
	e.activityGrowth += uint64(growth)
	return true
}

// A current tip alone does not authenticate a cached activity's inclusion. Its
// exact selected block must belong to that same prefix before the row can stop
// receiving regular observations. Archive depth remains a storage policy.
func (e *Engine) activityArchiveProof(ctx context.Context, a Activity) (bool, error) {
	point := e.archiveCurrent[a.Chain]
	if point.Hash == "" || !e.fresh(a.Chain) || !e.activitySourceCurrent(a.Chain, point.Generation) {
		return false, nil
	}
	var observation *ActivityObservation
	for i := range a.Observations {
		o := &a.Observations[i]
		if o.TxID == a.TxID && o.BlockHash == a.BlockHash && o.Status == "confirmed" && o.Confirmations >= archiveSettlementDepth && o.Height > 0 && o.Height <= point.Height && point.Height-o.Height+1 >= archiveSettlementDepth && o.ObservedAt > 0 && o.ObservedAt <= time.Now().Unix() && e.activitySourceCurrent(a.Chain, o.Generation) {
			observation = o
			break
		}
	}
	if observation == nil {
		return false, nil
	}
	source, ok := e.nodes[a.Chain].(recoveryBlockHasher)
	if !ok {
		return false, nil
	}
	hash, err := source.BlockHash(ctx, observation.Height)
	if err != nil || hash == "" {
		return false, errors.Join(err, errors.New("archived activity block verification unavailable"))
	}
	tip, err := source.BlockHash(ctx, point.Height)
	if err != nil || tip != point.Hash {
		delete(e.archiveCurrent, a.Chain)
		return false, errors.Join(err, errors.New("chain changed during archived activity verification"))
	}
	if !e.activitySourceCurrent(a.Chain, point.Generation) || !e.activitySourceCurrent(a.Chain, observation.Generation) {
		return false, errors.New("chain source changed during archived activity verification")
	}
	if hash != observation.BlockHash {
		// Retain the old positive outcome, but make the positively contradicted
		// current projection explicit before any report can reuse it.
		a.Observations = append([]ActivityObservation{}, a.Observations...)
		for i := range a.Observations {
			if a.Observations[i].TxID == observation.TxID {
				a.Observations[i].Status = "orphaned"
				a.Observations[i].Confirmations = 0
				a.Observations[i].ObservedAt = time.Now().Unix()
				a.Observations[i].Error = "The current canonical block contradicted this recorded confirmation."
			}
		}
		if !e.putActivity(a, true) {
			return false, errors.Join(e.fatal, errors.New("activity contradiction remains pending; historical indexing capacity is unavailable"))
		}
		return false, e.fatal
	}
	return true, nil
}

func (e *Engine) compactActivity(ctx context.Context, remaining *int, valid map[chain.ID]bool) error {
	coins := map[string]bool{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, coin := range e.knownCoins(id) {
			coins[string(id)+"/"+pointKey(CoinOutpoint{TxID: coin.TxID, Vout: coin.Vout})] = true
		}
	}
	var verificationErr error
	for _, id := range sortedArchiveIDs(e.s.Activities) {
		if *remaining <= 0 {
			return verificationErr
		}
		a := e.s.Activities[id]
		if e.s.Swaps[a.SwapID] != nil || e.s.Sends[a.SendID] != nil || e.s.Offers[a.OrderID].Content != "" {
			continue
		}
		if a.Kind == "tower_earning" {
			if e.s.TowerJobs[strings.TrimPrefix(a.GroupID, "tower/")] != nil {
				continue
			}
		}
		settled := a.Status == "confirmed" && a.Confirmations >= archiveSettlementDepth && valid[a.Chain]
		if a.Kind == "order" {
			switch a.Status {
			case "cancelled", "expired", "filled":
				settled = true
			}
		}
		if !settled {
			continue
		}
		held := false
		for _, point := range a.Outpoints {
			held = held || coins[string(a.Chain)+"/"+pointKey(point)]
		}
		if held {
			continue
		}
		// Count attempts as well as moves so unavailable block lookups cannot
		// grow an unbounded IO loop inside settlement's compaction phase.
		*remaining--
		if a.Kind != "order" {
			verified, err := e.activityArchiveProof(ctx, a)
			verificationErr = errors.Join(verificationErr, err)
			if ctx.Err() != nil || e.fatal != nil {
				return errors.Join(verificationErr, ctx.Err(), e.fatal)
			}
			if !verified {
				continue
			}
		}
		if err := e.stageArchive("activities", id); err != nil {
			return err
		}
		if e.s.Capacity.Anchors == nil {
			e.s.Capacity.Anchors = map[chain.ID]ArchiveAnchor{}
		}
		if valid[a.Chain] {
			p := e.archiveCurrent[a.Chain]
			e.s.Capacity.Anchors[a.Chain] = ArchiveAnchor{Height: p.Height, Hash: p.Hash}
		}
		for _, txid := range a.Variants {
			key := string(a.Chain) + "/" + txid
			if e.s.ActivityTransactions[key] == a.ID {
				if err := e.stageArchive("activity_transactions", key); err != nil {
					return err
				}
			}
			if err := e.stageArchive("activity_receipts", key); err != nil {
				return err
			}
		}
		for _, point := range a.Outpoints {
			if err := e.stageArchive("activity_owned", string(a.Chain)+"/"+pointKey(point)); err != nil {
				return err
			}
		}
	}
	return verificationErr
}

func (e *Engine) activityReceiptEvidence(key string) (ReceiptEvidence, bool) {
	if evidence, ok := e.s.ActivityReceipts[key]; ok {
		return evidence, true
	}
	var evidence ReceiptEvidence
	found, err := e.archivedValue("activity_receipts", key, &evidence)
	if err != nil {
		e.fatal = err
		return evidence, false
	}
	return evidence, found
}
