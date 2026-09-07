package daemon

import (
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/chain"
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
	stored := e.stateBytes + e.activityGrowth
	if e.s.Capacity != nil {
		stored += e.s.Capacity.Archived.Bytes
	}
	if stored+uint64(growth) > admissionByteCeiling {
		e.s.ActivityError = "Historical indexing is paused at the recovery-data budget. Existing rows and signed settlement evidence are retained; export a complete portable backup."
		return false
	}
	e.activityGrowth += uint64(growth)
	return true
}

func (e *Engine) compactActivity(remaining *int, valid map[chain.ID]bool) error {
	coins := map[string]bool{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, coin := range e.knownCoins(id) {
			coins[string(id)+"/"+pointKey(CoinOutpoint{TxID: coin.TxID, Vout: coin.Vout})] = true
		}
	}
	for _, id := range sortedArchiveIDs(e.s.Activities) {
		if *remaining <= 0 {
			return nil
		}
		a := e.s.Activities[id]
		if e.s.Swaps[a.SwapID] != nil || e.s.Sends[a.SendID] != nil || e.s.Offers[a.OrderID].Content != "" {
			continue
		}
		if a.Kind == "tower_earning" {
			if e.s.TowerJobs[a.GroupID] != nil {
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
		*remaining--
	}
	return nil
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
