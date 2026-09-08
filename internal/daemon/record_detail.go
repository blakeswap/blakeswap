package daemon

import (
	"encoding/json"
	"errors"
)

type RecordDetail struct {
	Kind               string         `json:"kind"`
	ID                 string         `json:"id"`
	Archived           bool           `json:"archived"`
	MonitoringRequired bool           `json:"monitoring_required"`
	Message            string         `json:"message"`
	Swap               *PublicSwap    `json:"swap,omitempty"`
	Send               *PublicSend    `json:"send,omitempty"`
	TowerJob           map[string]any `json:"tower_job,omitempty"`
}

func (e *Engine) recordDetail(raw json.RawMessage) (RecordDetail, error) {
	var q struct {
		Kind            string `json:"kind"`
		ID              string `json:"id"`
		ExpectedWallet  string `json:"expected_wallet"`
		ExpectedNetwork string `json:"expected_network"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return RecordDetail{}, err
	}
	if q.ExpectedWallet != e.Config.Name || q.ExpectedNetwork != string(e.Config.Network.Normalized()) {
		return RecordDetail{}, errors.New("record wallet or network changed; refresh the selected wallet")
	}
	if q.ID == "" || len(q.ID) > 128 {
		return RecordDetail{}, errors.New("invalid detail identity")
	}
	result := RecordDetail{Kind: q.Kind, ID: q.ID, MonitoringRequired: e.archivedObligationHeld(q.Kind + "/" + q.ID)}
	switch q.Kind {
	case "swap":
		value := e.s.Swaps[q.ID]
		if value == nil {
			var archived Swap
			found, err := e.archivedValue("swaps", q.ID, &archived)
			if err != nil {
				return result, err
			}
			if !found {
				return result, errors.New("swap detail unavailable")
			}
			value = &archived
			result.Archived = true
		}
		p := e.publicSwap(value)
		fill, err := e.fillSummary(value, result.Archived)
		if err != nil {
			return result, err
		}
		p.ParentID, p.ParentMaker, p.ParentRevision, p.Quantity = fill.ParentID, fill.ParentMaker, fill.ParentRevision, fill.Quantity
		p.Allocation, p.AllocatedQuantity, p.AllocationKnown = fill.Disposition, fill.AllocatedQuantity, fill.AllocationKnown
		result.MonitoringRequired = result.MonitoringRequired || fill.MonitoringRequired
		selection, err := e.retainedSwapFee(value)
		if err != nil {
			return result, err
		}
		p.FundingFee, p.Error = selection.FundingFee, value.Error
		result.Swap = &p
	case "send":
		value := e.s.Sends[q.ID]
		if value == nil {
			var archived WalletSend
			found, err := e.archivedValue("sends", q.ID, &archived)
			if err != nil {
				return result, err
			}
			if !found {
				return result, errors.New("send detail unavailable")
			}
			value = &archived
			result.Archived = true
		}
		p := value.public()
		result.Send = &p
	case "tower":
		value := e.s.TowerJobs[q.ID]
		if value == nil {
			var archived TowerJob
			found, err := e.archivedValue("tower_jobs", q.ID, &archived)
			if err != nil {
				return result, err
			}
			if !found {
				return result, errors.New("tower detail unavailable")
			}
			value = &archived
			result.Archived = true
		}
		result.TowerJob = map[string]any{"id": value.Job.ID, "swap_id": value.Job.SwapID, "kind": value.Job.Kind, "chain": value.Job.Target.Chain, "eligible_height": value.Job.Lock, "broadcast": value.Broadcast, "confirmations": value.Confirmed, "secret_observed": value.Secret != "", "error": value.Error, "variants": value.Variants}
	default:
		return result, errors.New("unsupported detail kind")
	}
	if result.Archived {
		result.Message = "Retained historical evidence. Confirmation counts describe its archived observation; canonical checkpoints monitor for a reorg that requires reactivation."
	}
	if result.MonitoringRequired {
		result.Message = "A known chain contradiction requires continued monitoring and positive current reconciliation."
	}
	return result, nil
}
