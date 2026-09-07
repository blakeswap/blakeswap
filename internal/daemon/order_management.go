package daemon

import (
	"encoding/json"
	"errors"
	"time"

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
func (e *Engine) finishedOrder(id string) string {
	result := ""
	for _, swap := range e.s.Swaps {
		var o protocol.Offer
		if swap.Role != "maker" || json.Unmarshal([]byte(swap.Request.OfferEvent.Content), &o) != nil || o.ID != id || o.Maker != e.identity.Public().Hex() {
			continue
		}
		if !terminalSwap(swap) {
			return ""
		}
		switch swap.Stage {
		case "completed":
			result = "filled"
		case "refunded":
			result = "refunded"
		case "expired before funding", "expired before maker funding", "aborted; counterparty refunded":
			result = "cancelled"
		}
	}
	return result
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
	if !ok || event.ID.Hex() != p.SourceEventID {
		return empty, errors.New("source order changed; refresh and review it again")
	}
	o, err := historicalOffer(event)
	if err != nil || o.Maker != e.identity.Public().Hex() || o.Network.Normalized() != e.Config.Network || o.ID != p.SourceOfferID {
		return empty, errors.New("source order does not belong to this wallet and network")
	}
	if e.activeOrderSwap(o.ID) {
		return empty, errors.New("reserved orders must settle or refund before recreation")
	}
	switch p.OrderAction {
	case "replace":
		if o.Status != "open" || o.Expires <= now {
			return empty, errors.New("only a current unreserved order can be replaced")
		}
	case "recreate":
		if o.Status != "cancelled" && o.Status != "filled" && !(o.Status == "open" && o.Expires <= now) && !(o.Status == "reserved" && e.finishedOrder(o.ID) != "") {
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
	if o.Status == "cancelled" && (p.ExpectedEventID == "" || p.ExpectedEventID == event.ID.Hex() || e.s.OrderRecords[p.ID].CancelledEventID == p.ExpectedEventID) {
		return e.ownOffer(o), nil
	}
	if p.ExpectedEventID != "" && p.ExpectedEventID != event.ID.Hex() {
		return nil, errors.New("order changed; refresh before cancelling")
	}
	if o.Status != "open" || e.activeOrderSwap(o.ID) {
		return nil, errors.New("only unreserved offers can be cancelled; committed swaps settle or refund")
	}
	o.Status = "cancelled"
	if err = e.publishOffer(o); err != nil {
		return nil, err
	}
	record := e.s.OrderRecords[o.ID]
	record.CancelledEventID = event.ID.Hex()
	e.s.OrderRecords[o.ID] = record
	delete(e.s.CoinReservations, "offer/"+o.ID)
	return e.ownOffer(o), e.save()
}
