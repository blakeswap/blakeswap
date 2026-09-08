package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

type OrderActionFields struct {
	OrderAction   string `json:"order_action,omitempty"`
	SourceOfferID string `json:"source_offer_id,omitempty"`
	SourceEventID string `json:"source_event_id,omitempty"`
}

func (e *Engine) activeOrderSwap(id string) bool {
	for _, s := range e.s.Swaps {
		var o protocol.Offer
		if s.Role == "maker" && !terminalSwap(s) && json.Unmarshal([]byte(s.Request.OfferEvent.Content), &o) == nil && o.ID == id && o.Maker == e.identity.Public().Hex() {
			return true
		}
	}
	return false
}

// A terminal local maker swap is stronger evidence than a historical relay
// status. Keep its signed reserved terms intact, but allow a fresh new order.
func orderSettlementStatus(stage string) string {
	switch stage {
	case "completed":
		return "filled"
	case "refunded":
		return "refunded"
	case "expired before funding", "expired before maker funding", "aborted; counterparty refunded":
		return "cancelled"
	}
	return ""
}

func (e *Engine) finishedOrderRecord(id string, record OrderRecord) string {
	// Live state always overrides retained settlement evidence, including a
	// reactivated obligation which has lost its previous terminal observation.
	stages := make(map[string]string, len(record.Settlements))
	for key, stage := range record.Settlements {
		stages[key] = stage
	}
	for key, swap := range e.s.Swaps {
		if swap == nil {
			continue
		}
		var o protocol.Offer
		if swap.Role != "maker" || json.Unmarshal([]byte(swap.Request.OfferEvent.Content), &o) != nil || o.ID != id || o.Maker != e.identity.Public().Hex() {
			continue
		}
		if !terminalSwap(swap) {
			return ""
		}
		stages[key] = swap.Stage
	}
	result := ""
	for _, stage := range stages {
		status := orderSettlementStatus(stage)
		if status == "filled" || status == "refunded" && result != "filled" || status == "cancelled" && result == "" {
			result = status
		}
	}
	return result
}

func (e *Engine) finishedOrderChecked(id string) (string, error) {
	record, ok := e.s.OrderRecords[id]
	if !ok {
		if _, err := e.archivedValue("order_records", id, &record); err != nil {
			return "", err
		}
	}
	if err := validateOrderSettlement(id, record, e.s.Network); err != nil {
		return "", err
	}
	return e.finishedOrderRecord(id, record), nil
}

func (e *Engine) finishedOrder(id string) string {
	result, _ := e.finishedOrderChecked(id)
	return result
}

// Record the exact positively retired maker identity before its core moves.
// This is retained history, never a publisher or an absence-based settlement
// inference. The record and core ownership changes share the next vault commit.
func (e *Engine) retainOrderSettlement(swap *Swap) error {
	if swap.Role != "maker" || orderSettlementStatus(swap.Stage) == "" {
		return nil
	}
	offer, err := historicalOffer(swap.Request.OfferEvent)
	if err != nil || offer.Maker != e.identity.Public().Hex() || offer.Network.Normalized() != e.Config.Network {
		return nil
	}
	if _, exists := e.s.OrderRecords[offer.ID]; !exists {
		if _, err := e.activateArchived("order_records", offer.ID); err != nil {
			return err
		}
	}
	if e.s.OrderRecords == nil {
		e.s.OrderRecords = map[string]OrderRecord{}
	}
	record, exists := e.s.OrderRecords[offer.ID]
	if !exists {
		event := swap.Request.OfferEvent
		if own, ok := e.s.Offers[offer.ID]; ok {
			event = own
		}
		source, err := historicalOffer(event)
		if err != nil || source.ID != offer.ID || source.Maker != offer.Maker || source.Network.Normalized() != e.Config.Network {
			return errors.New("invalid retained maker source")
		}
		record = OrderRecord{Offer: source, EventID: event.ID.Hex(), Publication: "unknown"}
	}
	if record.Settlements == nil {
		record.Settlements = map[string]string{}
	}
	record.Settlements[swap.ID] = swap.Stage
	if tower, ok := e.s.OfferTowers[offer.ID]; ok {
		copy := tower
		record.Protection = &copy
	}
	e.s.OrderRecords[offer.ID] = record
	return nil
}

func (e *Engine) orderSource(p OrderActionFields, now int64) (protocol.Offer, error) {
	var empty protocol.Offer
	if p.OrderAction == "" {
		if p.SourceOfferID != "" || p.SourceEventID != "" {
			return empty, errors.New("source order requires a replace or recreate action")
		}
		return empty, nil
	}
	if !protocol.Hex32(p.SourceOfferID) || !protocol.Hex32(p.SourceEventID) {
		return empty, errors.New("review the exact source order before managing it")
	}
	event, ok := e.s.Offers[p.SourceOfferID]
	if !ok && p.OrderAction == "recreate" && e.s.Recovery != nil {
		if err := e.recoveryTradingReady(); err != nil {
			return empty, err
		}
		event, ok = e.s.Recovery.Offers[p.SourceOfferID]
	}
	if !ok && p.OrderAction == "recreate" {
		if err := e.recoveryTradingReady(); err != nil {
			return empty, err
		}
		for _, kind := range []string{"offers", "quarantined_offers"} {
			var err error
			ok, err = e.archivedValue(kind, p.SourceOfferID, &event)
			if err != nil {
				return empty, err
			}
			if ok {
				break
			}
		}
	}
	if !ok || event.ID.Hex() != p.SourceEventID {
		return empty, errors.New("source order changed; refresh and review it again")
	}
	o, err := historicalOffer(event)
	if err != nil || o.Maker != e.identity.Public().Hex() || o.Network.Normalized() != e.Config.Network || o.ID != p.SourceOfferID {
		return empty, errors.New("source order does not belong to this wallet and network")
	}
	if p.OrderAction != "replace" && e.activeOrderSwap(o.ID) {
		return empty, errors.New("reserved orders must settle or refund before recreation")
	}
	switch p.OrderAction {
	case "replace":
		parent := e.s.ParentOrders[o.ID]
		if parent == nil || parent.RestoreHold || parent.Quantities.Closed || parent.Quantities.Available == 0 || parent.SignedRevision != parent.Quantities.Revision || o.Status != "open" || o.Expires <= now {
			return empty, errors.New("only currently available parent quantity can be replaced")
		}
	case "recreate":
		finished, err := e.finishedOrderChecked(o.ID)
		if err != nil {
			return empty, err
		}
		if o.Status != "cancelled" && o.Status != "filled" && !(o.Status == "open" && o.Expires <= now) && !(o.Status == "reserved" && finished != "") {
			return empty, errors.New("only a finished, cancelled or expired order can be recreated")
		}
	default:
		return empty, errors.New("order action must be replace or recreate")
	}
	return o, nil
}

func (e *Engine) replacementOwner(p OrderActionFields) (string, error) {
	if p.OrderAction == "" {
		return "", nil
	}
	if p.OrderAction != "replace" {
		return "", errors.New("only a replacement may consider its existing reservation")
	}
	o, err := e.orderSource(p, time.Now().Unix())
	if err != nil {
		return "", err
	}
	return "offer/" + o.ID, nil
}

func replacementFields(p TradeQuoteRequest) OrderActionFields {
	if p.OrderAction == "replace" {
		return p.OrderActionFields
	}
	return OrderActionFields{}
}

func (e *Engine) cancelOffer(raw json.RawMessage) (any, error) {
	var p struct {
		ID              string `json:"id"`
		ExpectedWallet  string `json:"expected_wallet"`
		ExpectedEventID string `json:"expected_event_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.ExpectedWallet != "" && p.ExpectedWallet != e.Config.Name {
		return nil, errors.New("wallet changed; reopen the order")
	}
	event, ok := e.s.Offers[p.ID]
	if !ok {
		return nil, errors.New("unknown own offer")
	}
	o, err := historicalOffer(event)
	if err != nil {
		return nil, err
	}
	if o.ID != p.ID || o.Maker != e.identity.Public().Hex() || o.Network.Normalized() != e.Config.Network {
		return nil, errors.New("order does not belong to this wallet and network")
	}
	parent := e.s.ParentOrders[p.ID]
	if parent == nil {
		return nil, errors.New("parent authorization unavailable")
	}
	if parent.Quantities.Closed && (p.ExpectedEventID == "" || p.ExpectedEventID == event.ID.Hex() || e.s.OrderRecords[p.ID].CancelledEventID == p.ExpectedEventID) {
		return e.ownOffer(parentPublicOffer(*parent)), nil
	}
	if p.ExpectedEventID != "" && p.ExpectedEventID != event.ID.Hex() {
		return nil, errors.New("order changed; refresh before cancelling")
	}
	if err = e.withdrawParentAvailable(o.ID, time.Now().Unix()); err != nil {
		return nil, err
	}
	record := e.s.OrderRecords[o.ID]
	record.CancelledEventID = event.ID.Hex()
	e.s.OrderRecords[o.ID] = record
	delete(e.s.CoinReservations, "offer/"+o.ID)
	return e.ownOffer(parentPublicOffer(*parent)), e.save()
}

// ValidateOrderSettlements leaves older records without this optional companion
// unchanged, while rejecting malformed new authority-linked history on load.
func ValidateOrderSettlements(state *State) error {
	for id, record := range state.OrderRecords {
		if err := validateOrderSettlement(id, record, state.Network); err != nil {
			return err
		}
	}
	return nil
}
func validateOrderSettlement(id string, record OrderRecord, network chain.Network) error {
	if len(record.Settlements) == 0 {
		return nil
	}
	if record.Offer.ID != id || record.Offer.Network.Normalized() != network.Normalized() || record.Offer.Validate(record.Offer.Expires-1) != nil {
		return errors.New("invalid retained order settlement source")
	}
	for swapID, stage := range record.Settlements {
		if !protocol.Hex32(swapID) || orderSettlementStatus(stage) == "" {
			return errors.New("invalid retained order settlement identity or outcome")
		}
	}
	return nil
}
