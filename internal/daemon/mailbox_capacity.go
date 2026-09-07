package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// Canonical protocol payload identity is separate from exact sender/message-ID
// binding. A peer cannot bypass unsolicited admission by repeatedly wrapping an
// already handled payload under new IDs or adding ignored JSON fields. New valid
// funding/terms/receipt evidence still passes the established-obligation path.
func mailboxSemantic(from string, message transport.Message) (string, error) {
	var body any
	switch message.Type {
	case "request":
		body = &protocol.Request{}
	case "accepted":
		body = &protocol.Terms{}
	case "tower-job":
		body = &protocol.Job{}
	case "tower-receipt":
		body = &protocol.Receipt{}
	case "long-funded", "short-funded":
		body = &fundingMessage{}
	case "rejected":
		// The first rejection is retained. Later changed explanatory text does
		// not create another authorization or monetary fact.
		body = &struct{}{}
	default:
		return "", errors.New("unsupported durable mailbox payload")
	}
	if err := json.Unmarshal(message.Body, body); err != nil {
		return "", err
	}
	if funding, ok := body.(*fundingMessage); ok {
		if tx, err := contract.Parse(funding.Raw); err == nil {
			// Funding identity is its non-witness txid. Alternate witness
			// serializations cannot manufacture unlimited durable alias records.
			// Exact sender/message-ID binding still compares the complete payload.
			funding.Raw = tx.TxHash().String()
		}
	}
	return protocol.Digest([]any{from, message.Type, message.SwapID, body}), nil
}

func (e *Engine) admitMailboxAlias(from string) error {
	if !e.capacityHealth().AdmissionAvailable {
		return errors.New("new aliases for already handled mailbox contents are held by capacity; retry the original message ID")
	}
	return e.consumeUnsolicitedSlot(from)
}

func (e *Engine) establishedMessage(from string, message transport.Message) (bool, error) {
	if message.Type == "tower-job" {
		var job protocol.Job
		if err := json.Unmarshal(message.Body, &job); err != nil {
			return false, err
		}
		if existing := e.s.TowerJobs[job.ID]; existing != nil {
			return existing.Job.Owner == from && existing.Job.SwapID == message.SwapID, nil
		}
		var archived TowerJob
		found, err := e.archivedValue("tower_jobs", job.ID, &archived)
		return found && archived.Job.Owner == from && archived.Job.SwapID == message.SwapID, err
	}
	swap := e.s.Swaps[message.SwapID]
	if swap == nil {
		var archived Swap
		found, err := e.archivedValue("swaps", message.SwapID, &archived)
		if err != nil || !found {
			return false, err
		}
		swap = &archived
	}
	peer := swap.Request.OfferEvent.PubKey.Hex()
	if swap.Role == "maker" {
		peer = swap.Request.Taker
	}
	known := from == peer || (message.Type == "tower-receipt" && from == swap.protection().PubKey)
	if known && e.s.Swaps[message.SwapID] == nil {
		for _, item := range []struct{ kind, id string }{{"swaps", message.SwapID}, {"recovery_swaps", message.SwapID}, {"funding_fees", "swap/" + message.SwapID}} {
			if _, err := e.activateArchived(item.kind, item.id); err != nil {
				return false, err
			}
		}
	}
	return known, nil
}

func (e *Engine) admitMailbox(from string, message transport.Message) error {
	known, err := e.establishedMessage(from, message)
	if err != nil || known {
		return err
	}
	if message.Type != "request" && message.Type != "tower-job" {
		return errors.New("message does not belong to an established obligation")
	}
	if !e.capacityHealth().AdmissionAvailable {
		return errors.New("unsolicited mailbox admission is paused by recovery capacity")
	}
	return e.consumeUnsolicitedSlot(from)
}

func (e *Engine) consumeUnsolicitedSlot(from string) error {
	window := time.Now().Unix() / 60
	if e.mailboxWindow != window || e.mailboxAdmissions == nil {
		e.mailboxWindow, e.mailboxAdmissions = window, map[string]int{}
	}
	if e.mailboxAdmissions[""] >= 64 || e.mailboxAdmissions[from] >= 16 {
		return errors.New("unsolicited mailbox rate limit reached; established obligation messages remain admitted")
	}
	e.mailboxAdmissions[""]++
	e.mailboxAdmissions[from]++
	return nil
}

func (e *Engine) durableMailboxSemantic(from string, message transport.Message) (string, error) {
	if message.Type == "accepted" {
		swap := e.s.Swaps[message.SwapID]
		if swap == nil {
			var archived Swap
			found, err := e.archivedValue("swaps", message.SwapID, &archived)
			if err != nil {
				return "", err
			}
			if found {
				swap = &archived
			}
		}
		// Valid late acceptances cannot revive a terminal released intention. Their
		// varying proposed heights are not new retained settlement authority. handle
		// still verifies the complete signed request and terms before any ACK.
		if swap != nil && swap.Terms == nil && terminalSwap(swap) {
			return protocol.Digest([]string{from, message.Type, message.SwapID, "inactive-acceptance"}), nil
		}
	}
	return mailboxSemantic(from, message)
}
