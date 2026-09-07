package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
)

// Reserve space before new authority is admitted. Existing signed transactions
// and witness reconciliation continue at the admission ceiling. A new optional
// signature/fee variant must pass the same growth check before it is created.
const activeRecoveryReserve = 1 << 20
const archivedEvidenceReserve = 4 << 10
const admissionByteCeiling = WalletRecoveryBudget - 16<<20

type CapacityHealth struct {
	PublicLimited      bool              `json:"public_limited"`
	Relays             []RelaySyncRecord `json:"relays"`
	State              string            `json:"state"`
	Active             uint64            `json:"active"`
	Archived           uint64            `json:"archived"`
	StoredBytes        uint64            `json:"stored_bytes"`
	ReservedBytes      uint64            `json:"reserved_bytes"`
	BudgetBytes        uint64            `json:"budget_bytes"`
	AdmissionAvailable bool              `json:"admission_available"`
	Reactivating       bool              `json:"reactivating"`
	MonitoringHolds    uint64            `json:"monitoring_holds"`
	Message            string            `json:"message"`
}

func (e *Engine) activeWork(kind string) int {
	count := 0
	if kind == "" || kind == "offer" {
		for _, event := range e.s.Offers {
			var offer protocol.Offer
			if json.Unmarshal([]byte(event.Content), &offer) != nil || offer.Status == "reserved" || (offer.Status == "open" && offer.Expires > time.Now().Unix()) {
				count++
			}
		}
	}
	if kind == "" || kind == "swap" {
		for _, swap := range e.s.Swaps {
			if swap == nil || !terminalSwap(swap) {
				count++
			}
		}
	}
	if kind == "" || kind == "send" {
		for _, send := range e.s.Sends {
			if send == nil || send.Confirmations < e.Config.Network.Confirmations() {
				count++
			}
		}
	}
	if kind == "" || kind == "tower" {
		for _, job := range e.s.TowerJobs {
			// Expiry alone cannot retire an accepted signed authorization.
			if job == nil || job.Confirmed < e.Config.Network.Confirmations() {
				count++
			}
		}
	}
	if kind == "receipt" {
		for _, receipt := range e.s.TradeReceipts {
			if receipt == nil || receipt.Result.State == "pending" {
				count++
			}
		}
	}
	return count
}

func (e *Engine) capacityHealth() CapacityHealth {
	health := CapacityHealth{State: "healthy", PublicLimited: e.s.PublicLimited, Relays: e.relayHealth(), Active: uint64(e.activeWork("")), StoredBytes: e.stateBytes, BudgetBytes: WalletRecoveryBudget, Message: "Encrypted archives retain signed recovery evidence. Canonical checkpoints continue to monitor archived settlements."}
	if e.s.Capacity != nil {
		c := e.s.Capacity
		health.Archived = c.Archived.Count
		health.StoredBytes += c.Archived.Bytes
		health.Reactivating = c.Reactivating
		health.MonitoringHolds = uint64(len(c.Invalidated))
		health.ReservedBytes = (c.Archived.Kinds["sends"] + c.Archived.Kinds["swaps"] + c.Archived.Kinds["tower_jobs"]) * archivedEvidenceReserve
	}
	health.ReservedBytes += health.Active * activeRecoveryReserve
	health.AdmissionAvailable = health.StoredBytes+health.ReservedBytes+activeRecoveryReserve <= admissionByteCeiling && e.archiveHold() == nil
	if health.Reactivating || health.MonitoringHolds > 0 {
		health.State, health.Message = "recovering", "Archived settlement evidence was contradicted. Keep monitoring while records reactivate and positive current evidence resolves each obligation."
	} else if !health.AdmissionAvailable {
		health.State, health.Message = "limited", "New work is held by the recovery-data budget or unavailable archived checkpoints. Existing signed recovery and message acknowledgments continue; retain a complete portable backup."
	}
	return health
}

func (e *Engine) admitWork(kind string) error {
	if e.activeWork(kind) >= activeWorkLimit {
		return fmt.Errorf("active %s capacity reached; finish existing work before admitting more", kind)
	}
	if !e.capacityHealth().AdmissionAvailable {
		return fmt.Errorf("new %s exceeds available recovery capacity or awaits archived checkpoint verification", kind)
	}
	return nil
}

// ArchiveMonitoring is a small durable projection for all-wallet shutdown and
// network-switch summaries. Ordinary unavailable anchors do not manufacture a
// new obligation; positively contradicted settlements remain explicit holds.
type ArchiveMonitoringState struct {
	Reactivating bool
	Invalidated  []string
}

func ArchiveMonitoring(state State) ArchiveMonitoringState {
	result := ArchiveMonitoringState{}
	if state.Capacity != nil {
		result.Reactivating = state.Capacity.Reactivating
		result.Invalidated = sortedArchiveIDs(state.Capacity.Invalidated)
	}
	return result
}
func (e *Engine) archivedObligationHeld(id string) bool {
	return e.s.Capacity != nil && (e.s.Capacity.Reactivating || e.s.Capacity.Invalidated[id]) || e.s.Recovery != nil && e.s.Recovery.InvalidatedSettlements[id]
}
