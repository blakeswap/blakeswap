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
		}
	}()
	fee := e.fundingFee("offer/" + o.ID)
	available := e.chainBalances(e.publicCoins())[o.Sell].UnlockedConfirmed
	if oldOwner == "" && available < o.SellAmount+fee {
		return nil, fmt.Errorf("insufficient unlocked confirmed %s balance: need %d sats including the %d-sat funding fee; available %d sats", o.Sell, o.SellAmount+fee, fee, available)
	}
	if len(e.s.Offers) >= 1000 {
		return nil, errors.New("order capacity reached")
	}
	selectionOwner := "offer/" + o.ID
	if oldOwner != "" {
		selectionOwner = oldOwner
	}
	candidate, err := e.reservationCandidate(selectionOwner, o.Sell, o.SellAmount+fee)
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
	// Sign both events before changing the source offer. The engine lock excludes
	// acceptance and cancellation until this single durable transition completes.
	at := nostr.Now()
	if at <= e.s.EventTime {
		at = e.s.EventTime + 1
	}
	var cancelled nostr.Event
	if oldOwner != "" {
		source.Status = "cancelled"
		cancelled, err = e.signOffer(source, at)
		if err != nil {
			return nil, err
		}
		at++
	}
	created, err := e.signOffer(o, at)
	if err != nil {
		return nil, err
	}
	if oldOwner != "" {
		e.stageOffer(source, cancelled)
		old := e.s.OrderRecords[source.ID]
		old.ReplacedBy, old.CancelledEventID = o.ID, action.SourceEventID
		e.s.OrderRecords[source.ID] = old
		delete(e.s.CoinReservations, oldOwner)
	}
	if e.s.OfferTowers == nil {
		e.s.OfferTowers = map[string]protocol.Tower{}
	}
	e.s.OfferTowers[o.ID] = tower
	e.stageOffer(o, created)
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
	for _, existing := range e.s.Swaps {
		var requested protocol.Offer
		if existing.Role == "taker" && !terminalSwap(existing) && json.Unmarshal([]byte(existing.Request.OfferEvent.Content), &requested) == nil && requested.ID == o.ID && requested.Maker == o.Maker {
			return nil, errors.New("this wallet has already requested or reserved that order")
		}
	}
	if o.Status != "open" || o.Maker == e.identity.Public().Hex() {
		return nil, errors.New("offer not available to take")
	}
	tower, err := e.selectProtection(o, p.TowerBPS, p.TowerPubKey, false)
	if err != nil {
		return nil, err
	}
	if len(e.s.Swaps) >= 1000 {
		return nil, errors.New("swap capacity")
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
	request := protocol.Request{ID: id, OfferEvent: event, Taker: e.identity.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: keys}
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
	if err := e.reserveCoins("swap/"+id, o.Sell.Other(), o.BuyAmount+e.fundingFee("swap/"+id)); err != nil {
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
