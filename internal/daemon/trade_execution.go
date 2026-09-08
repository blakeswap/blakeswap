package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"time"
)

func (e *Engine) createOffer(ctx context.Context, raw json.RawMessage, receipt *TradeReceipt) (any, error) {
	if err := e.recoveryTradingReady(); err != nil {
		return nil, err
	}
	if e.Config.Mode != "trader" {
		return nil, errors.New("tower cannot trade")
	}
	var o protocol.Offer
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	var action OrderActionFields
	if err := json.Unmarshal(raw, &action); err != nil {
		return nil, err
	}
	if (action.OrderAction != "" || action.SourceOfferID != "" || action.SourceEventID != "") && receipt == nil {
		return nil, errors.New("order replacement and recreation require a reviewed confirmation")
	}
	if err := e.validateAutomationReceipt(receipt); err != nil {
		return nil, err
	}
	source, err := e.orderSource(action, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	oldOwner := ""
	if action.OrderAction == "replace" {
		oldOwner = "offer/" + source.ID
	}
	o.Network = e.Config.Network
	o.Version, o.Revision, o.Available = protocol.Version, 1, o.SellAmount
	o.ID = tradeRequestID(receipt)
	o.Maker = e.identity.Public().Hex()
	o.Status = "open"
	o.Reservation = ""
	if o.Expires == 0 {
		o.Expires = time.Now().Unix() + 24*3600
	}
	if err := o.Validate(time.Now().Unix()); err != nil {
		return nil, err
	}
	var selection struct {
		PubKey string `json:"tower_pubkey"`
	}
	if err := json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}
	tower, err := e.selectProtection(o, o.TowerBPS, selection.PubKey, true)
	if err != nil {
		return nil, err
	}
	o.Tower = nil
	if tower.BPS > 0 {
		o.Tower = &tower
	}
	if err := e.refresh(ctx); err != nil {
		return nil, err
	}
	if receipt != nil {
		if err := e.validateAutomationReceipt(receipt); err != nil {
			return nil, err
		}
		if err := e.validateTradeSource(receipt.Snapshot, time.Now().Unix()); err != nil {
			return nil, err
		}
	}
	if err := e.recoveryTradingReady(); err != nil {
		return nil, err
	}
	if err := e.selectFundingFee(raw, "offer/"+o.ID, o.Sell); err != nil {
		return nil, err
	}
	defer func() {
		if _, ok := e.s.Offers[o.ID]; !ok {
			delete(e.s.FundingFees, "offer/"+o.ID)
			delete(e.s.CoinReservations, "offer/"+o.ID)
			delete(e.s.ParentOrders, o.ID)
		}
	}()
	var authorization FillOrderFields
	if err := json.Unmarshal(raw, &authorization); err != nil {
		return nil, err
	}
	parent, err := newParentOrder(o, e.s.FundingFees["offer/"+o.ID], authorization, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	reserve, err := parent.fundingReserve(o.SellAmount)
	if err != nil {
		return nil, err
	}
	available := e.chainBalances(e.publicCoins())[o.Sell].UnlockedConfirmed
	if oldOwner == "" && available < o.SellAmount+reserve {
		return nil, fmt.Errorf("insufficient unlocked confirmed %s balance: need %d sats including the %d-sat aggregate funding reserve; available %d sats", o.Sell, o.SellAmount+reserve, reserve, available)
	}
	if err := e.admitWork("offer"); err != nil {
		return nil, err
	}
	selectionOwner := "offer/" + o.ID
	if oldOwner != "" {
		selectionOwner = oldOwner
	}
	candidate, err := e.reservationCandidate(selectionOwner, o.Sell, o.SellAmount+reserve)
	if err != nil {
		delete(e.s.CoinReservations, "offer/"+o.ID)
		return nil, err
	}
	if e.s.CoinReservations == nil {
		e.s.CoinReservations = map[string]CoinReservation{}
	}
	e.s.CoinReservations["offer/"+o.ID] = candidate
	if err := e.validateTradeInputs("offer/"+o.ID, receipt); err != nil {
		return nil, err
	}
	if err := e.validateFundingReview("offer/"+o.ID, o.Sell); err != nil {
		return nil, err
	}
	// Prepare both parent transitions before assigning their real input pool.
	// A same-second withdrawal is durable immediately and published on the next
	// per-parent timestamp; it can no longer accept a stale request meanwhile.
	var oldParent *ParentOrder
	var oldEvent *nostr.Event
	if oldOwner != "" {
		current := e.s.ParentOrders[source.ID]
		if current == nil {
			return nil, errors.New("source parent authorization unavailable")
		}
		copy := current.clone()
		copy.Quantities, err = copy.Quantities.withdrawAvailable()
		if err != nil {
			return nil, err
		}
		copy, oldEvent, err = e.prepareParentPublication(copy, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		oldParent = &copy
	}
	prepared, newEvent, err := e.prepareParentPublication(*parent, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if newEvent == nil {
		return nil, errors.New("initial parent publication unavailable")
	}
	parent = &prepared
	if e.s.ParentOrders == nil {
		e.s.ParentOrders = map[string]*ParentOrder{}
	}
	if oldParent != nil {
		e.s.ParentOrders[source.ID] = oldParent
		if oldEvent != nil {
			e.stageOffer(parentPublicOffer(*oldParent), *oldEvent)
		}
		old := e.s.OrderRecords[source.ID]
		old.ReplacedBy, old.CancelledEventID = o.ID, action.SourceEventID
		e.s.OrderRecords[source.ID] = old
		delete(e.s.CoinReservations, oldOwner)
	}
	e.s.ParentOrders[o.ID] = parent
	if e.s.OfferTowers == nil {
		e.s.OfferTowers = map[string]protocol.Tower{}
	}
	e.s.OfferTowers[o.ID] = tower
	e.stageOffer(parentPublicOffer(*parent), *newEvent)
	record := e.s.OrderRecords[o.ID]
	if oldOwner != "" {
		record.Replaces = source.ID
	}
	if action.OrderAction == "recreate" {
		record.RecreatedFrom = source.ID
	}
	e.s.OrderRecords[o.ID] = record
	acceptTrade(receipt)
	e.acceptAutomationOffer(receipt)
	return o, e.save()
}

func (e *Engine) takeOffer(ctx context.Context, raw json.RawMessage, receipt *TradeReceipt) (any, error) {
	if err := e.recoveryTradingReady(); err != nil {
		return nil, err
	}
	if e.Config.Mode != "trader" {
		return nil, errors.New("trader is unavailable")
	}
	var p struct {
		FillTakeFields
		Maker       string `json:"maker"`
		ID          string `json:"id"`
		TowerBPS    int64  `json:"tower_bps"`
		TowerPubKey string `json:"tower_pubkey"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	event, ok := e.s.Book[p.Maker+":"+p.ID]
	if !ok {
		return nil, errors.New("offer not in verified orderbook")
	}
	o, err := protocol.DecodeOffer(event, time.Now().Unix())
	if err != nil {
		return nil, err
	}

	if o.Status != "open" || o.Maker == e.identity.Public().Hex() {
		return nil, errors.New("offer not available to take")
	}
	if p.ParentRevision != o.Revision {
		return nil, errors.New("parent revision changed; review the exact current fill")
	}
	amounts, err := o.FillPolicy.Quote(o.SellAmount, o.BuyAmount, o.Available, p.Quantity)
	if err != nil {
		return nil, err
	}
	if err := protocol.ValidateRescueAmounts(p.TowerBPS, amounts.Sell, amounts.Buy); err != nil {
		return nil, err
	}
	tower, err := e.selectProtection(o, p.TowerBPS, p.TowerPubKey, false)
	if err != nil {
		return nil, err
	}
	if err := e.admitWork("swap"); err != nil {
		return nil, err
	}
	id := tradeRequestID(receipt)
	secret, err := hex.DecodeString(transport.RandomID())
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(secret)
	keys, err := e.swapKeys(id)
	if err != nil {
		return nil, err
	}
	request := protocol.Request{Version: protocol.Version, Revision: p.ParentRevision, Quantity: p.Quantity, ID: id, OfferEvent: event, Taker: e.identity.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: keys}
	s := &Swap{ID: id, Role: "taker", Protection: &tower, Request: request, Secret: hex.EncodeToString(secret), Receipts: map[string]protocol.Receipt{}, Stage: "request queued"}
	if err := e.refresh(ctx); err != nil {
		return nil, err
	}
	if receipt != nil {
		if err := e.validateTradeSource(receipt.Snapshot, time.Now().Unix()); err != nil {
			return nil, err
		}
	}
	if err := e.recoveryTradingReady(); err != nil {
		return nil, err
	}
	if err := e.selectFundingFee(raw, "swap/"+id, o.Sell.Other()); err != nil {
		return nil, err
	}
	if err := e.reserveCoins("swap/"+id, o.Sell.Other(), amounts.Buy+e.fundingFee("swap/"+id)); err != nil {
		delete(e.s.CoinReservations, "swap/"+id)
		delete(e.s.FundingFees, "swap/"+id)
		return nil, err
	}
	if err := e.validateTradeInputs("swap/"+id, receipt); err != nil {
		delete(e.s.FundingFees, "swap/"+id)
		delete(e.s.CoinReservations, "swap/"+id)
		return nil, err
	}
	if err := e.validateFundingReview("swap/"+id, o.Sell.Other()); err != nil {
		delete(e.s.FundingFees, "swap/"+id)
		delete(e.s.CoinReservations, "swap/"+id)
		return nil, err
	}
	s.OwnerFeeCap = e.s.FundingFees["swap/"+id].OwnerFeeCap
	if err = e.queue(o.Maker, "request", id, request); err != nil {
		return nil, err
	}
	e.s.Swaps[id] = s
	acceptTrade(receipt)
	return map[string]string{"id": id}, e.save()
}
