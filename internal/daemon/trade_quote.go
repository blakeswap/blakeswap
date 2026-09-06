package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

const tradeQuoteLifetime int64 = 120
const tradeQuoteCapacity = 64
const tradeReceiptCapacity = 1000

type TradeQuoteRequest struct {
	FeeSelection
	Kind            string   `json:"kind"`
	ExpectedWallet  string   `json:"expected_wallet"`
	ExpectedNetwork string   `json:"expected_network"`
	Maker           string   `json:"maker"`
	ID              string   `json:"id"`
	Sell            chain.ID `json:"sell"`
	SellAmount      int64    `json:"sell_amount"`
	BuyAmount       int64    `json:"buy_amount"`
	Expires         int64    `json:"expires"`
	TowerBPS        int64    `json:"tower_bps"`
	TowerPubKey     string   `json:"tower_pubkey"`
}
type TradeTiming struct {
	Unit           string `json:"unit"`
	Confirmations  int    `json:"confirmations"`
	OwnRefund      uint32 `json:"own_refund"`
	IncomingRefund uint32 `json:"incoming_refund"`
	RevealBefore   uint32 `json:"reveal_before"`
	TowerTakeover  uint32 `json:"tower_takeover"`
	RefundGrace    uint32 `json:"refund_grace"`
	FirstRevealer  string `json:"first_revealer"`
}
type TradeOutcome struct {
	Kind      string   `json:"kind"`
	Chain     chain.ID `json:"chain"`
	Principal int64    `json:"principal"`
	FeeMin    int64    `json:"fee_min"`
	FeeMax    int64    `json:"fee_max"`
	Bounty    int64    `json:"bounty"`
	NetMin    int64    `json:"net_min"`
	NetMax    int64    `json:"net_max"`
}
type TradeQuote struct {
	Token             string         `json:"token"`
	Revision          string         `json:"revision"`
	Kind              string         `json:"kind"`
	Wallet            string         `json:"wallet"`
	WalletKey         string         `json:"wallet_key"`
	Network           chain.Network  `json:"network"`
	Created           int64          `json:"created"`
	Expires           int64          `json:"expires"`
	OfferEventID      string         `json:"offer_event_id"`
	OfferID           string         `json:"offer_id"`
	OfferMaker        string         `json:"offer_maker"`
	OfferExpires      int64          `json:"offer_expires"`
	PaidChain         chain.ID       `json:"paid_chain"`
	PaidPrincipal     int64          `json:"paid_principal"`
	PaidTotal         int64          `json:"paid_total"`
	ReceivedChain     chain.ID       `json:"received_chain"`
	ReceivedPrincipal int64          `json:"received_principal"`
	RateNumerator     int64          `json:"rate_numerator"`
	RateDenominator   int64          `json:"rate_denominator"`
	RateDisplay       string         `json:"rate_display"`
	Fees              FeeSelection   `json:"fees"`
	FundingSize       int64          `json:"funding_size"`
	Provider          protocol.Tower `json:"provider"`
	ProviderRevision  string         `json:"provider_revision"`
	TowerCoverage     string         `json:"tower_coverage"`
	Timing            TradeTiming    `json:"timing"`
	Outcomes          []TradeOutcome `json:"outcomes"`
	Funds             FundsPreflight `json:"funds"`
	Ready             bool           `json:"ready"`
	Error             string         `json:"error"`
}
type TradeQuoteSnapshot struct {
	Quote   TradeQuote        `json:"quote"`
	Request TradeQuoteRequest `json:"request"`
	Offer   protocol.Offer    `json:"offer"`
}
type ConfirmTradeRequest struct {
	Token           string `json:"token"`
	Revision        string `json:"revision"`
	RequestID       string `json:"request_id"`
	ExpectedWallet  string `json:"expected_wallet"`
	ExpectedNetwork string `json:"expected_network"`
}
type ConfirmTradeResult struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Error string `json:"error"`
}

// A receipt is committed in the same encrypted snapshot as its offer/request.
// Pending authorization can be retried; accepted and rejected IDs never change
// meaning, even after quote expiry, a restart or an ambiguous API response.
type TradeReceipt struct {
	Digest   string             `json:"digest"`
	Snapshot TradeQuoteSnapshot `json:"snapshot"`
	Result   ConfirmTradeResult `json:"result"`
}

func (e *Engine) tradeBinding(wallet, network string) error {
	if wallet == "" || wallet != e.Config.Name || network != string(e.Config.Network.Normalized()) {
		return errors.New("wallet or network changed; reopen the review in the selected wallet")
	}
	if e.Config.Mode != "trader" {
		return errors.New("trader is unavailable")
	}
	if e.fatal != nil {
		return e.fatal
	}
	return nil
}

// Called with the engine lock held. No signing, reservations, messages or durable
// mutation occurs while constructing or refreshing a quote.
func (e *Engine) tradeSnapshot(p TradeQuoteRequest, now int64) (TradeQuoteSnapshot, error) {
	var s TradeQuoteSnapshot
	if err := e.tradeBinding(p.ExpectedWallet, p.ExpectedNetwork); err != nil {
		return s, err
	}
	if p.Kind != "maker" && p.Kind != "taker" {
		return s, errors.New("trade quote kind must be maker or taker")
	}
	if p.FundingFee < 1 || (p.OwnerFeeCap != 0 && p.OwnerFeeCap != 20000) || p.Rate < 0 {
		return s, errors.New("review an explicit bounded funding and owner fee policy")
	}
	o := protocol.Offer{Network: e.Config.Network, ID: transport.RandomID(), Maker: e.identity.Public().Hex(), Sell: p.Sell, SellAmount: p.SellAmount, BuyAmount: p.BuyAmount, Expires: p.Expires, Status: "open"}
	eventID := ""
	if p.Kind == "taker" {
		event, ok := e.s.Book[p.Maker+":"+p.ID]
		if !ok {
			return s, errors.New("offer not in verified orderbook; refresh the market")
		}
		var err error
		o, err = protocol.DecodeOffer(event, now)
		if err != nil {
			return s, err
		}
		if o.Status != "open" || o.Maker == e.identity.Public().Hex() {
			return s, errors.New("offer is no longer available to take")
		}
		if p.Sell != o.Sell || p.SellAmount != o.SellAmount || p.BuyAmount != o.BuyAmount {
			return s, errors.New("order amounts changed; refresh the market before reviewing")
		}
		eventID = event.ID.Hex()
	} else if o.Expires == 0 {
		o.Expires = now + 24*3600
	}
	if err := o.Validate(now); err != nil {
		return s, err
	}
	p.Expires = o.Expires
	if p.TowerBPS > 0 && p.TowerPubKey == "" {
		return s, errors.New("select a discovered watchtower with a current signed proof")
	}
	tower, err := e.selectProtection(o, p.TowerBPS, p.TowerPubKey, p.Kind == "maker")
	if err != nil {
		return s, err
	}
	if tower.BPS > 0 && (tower.Event == "" || tower.Expires <= now) {
		return s, errors.New("watchtower proof expired; refresh before reviewing")
	}
	q := TradeQuote{Kind: p.Kind, Wallet: e.Config.Name, WalletKey: e.identity.Public().Hex(), Network: e.Config.Network, Created: now, Expires: now + tradeQuoteLifetime, OfferEventID: eventID, OfferID: o.ID, OfferMaker: o.Maker, OfferExpires: o.Expires, Fees: p.FeeSelection, Provider: tower, ProviderRevision: protocol.Digest(tower), TowerCoverage: "none", Outcomes: []TradeOutcome{}}
	q.Expires = min(q.Expires, o.Expires)
	if tower.BPS > 0 {
		q.Expires = min(q.Expires, tower.Expires)
	}
	if p.Rate > 0 {
		q.Expires = min(q.Expires, p.Timestamp+120)
	}
	if q.Expires <= now {
		return s, errors.New("fee or provider review expired; refresh it")
	}
	q.PaidChain, q.PaidPrincipal, q.ReceivedChain, q.ReceivedPrincipal = o.Sell, o.SellAmount, o.Sell.Other(), o.BuyAmount
	if p.Kind == "taker" {
		q.PaidChain, q.PaidPrincipal, q.ReceivedChain, q.ReceivedPrincipal = o.Sell.Other(), o.BuyAmount, o.Sell, o.SellAmount
	}
	if p.FundingFee > feeLimits(q.PaidChain).Funding {
		return s, errors.New("funding fee exceeds the selected chain cap")
	}
	q.PaidTotal = q.PaidPrincipal + p.FundingFee
	ratio := new(big.Rat).SetFrac64(q.ReceivedPrincipal, q.PaidPrincipal)
	q.RateNumerator, q.RateDenominator, q.RateDisplay = ratio.Num().Int64(), ratio.Denom().Int64(), ratio.FloatString(8)
	ownerMax := int64(2000)
	if p.OwnerFeeCap > 0 {
		ownerMax = p.OwnerFeeCap
	}
	add := func(kind string, id chain.ID, principal, maximum, bps int64) {
		bounty := protocol.Bounty(principal, bps)
		q.Outcomes = append(q.Outcomes, TradeOutcome{Kind: kind, Chain: id, Principal: principal, FeeMin: 2000, FeeMax: maximum, Bounty: bounty, NetMin: principal - maximum - bounty, NetMax: principal - 2000 - bounty})
	}
	add("owner_claim", q.ReceivedChain, q.ReceivedPrincipal, ownerMax, 0)
	add("owner_refund", q.PaidChain, q.PaidPrincipal, ownerMax, 0)
	if tower.BPS > 0 {
		q.TowerCoverage = "refund only; owner must reveal first"
		if p.Kind == "maker" {
			q.TowerCoverage = "delayed incoming claim and own refund"
			add("tower_claim", q.ReceivedChain, q.ReceivedPrincipal, 20000, tower.BPS)
		}
		add("tower_refund", q.PaidChain, q.PaidPrincipal, 20000, tower.BPS)
	}
	timing := TradeTiming{Unit: "seconds", Confirmations: e.Config.Network.Confirmations(), OwnRefund: protocol.ShortSeconds, IncomingRefund: protocol.LongSeconds, RevealBefore: protocol.RevealSeconds, TowerTakeover: protocol.TakeoverSeconds, RefundGrace: protocol.RefundDelay(e.Config.Network), FirstRevealer: "taker"}
	if e.Config.Network.Normalized() == chain.Regtest {
		timing.Unit = "blocks"
		timing.OwnRefund = protocol.ShortBlocks
		timing.IncomingRefund = protocol.LongBlocks
		timing.RevealBefore = protocol.RevealBlocks
		timing.TowerTakeover = protocol.TakeoverBlocks
	}
	if p.Kind == "taker" {
		timing.OwnRefund, timing.IncomingRefund = timing.IncomingRefund, timing.OwnRefund
	}
	q.Timing = timing
	return TradeQuoteSnapshot{Quote: q, Request: p, Offer: o}, nil
}

func (e *Engine) validateTradeSource(s TradeQuoteSnapshot, now int64) error {
	q, p := s.Quote, s.Request
	if err := e.tradeBinding(q.Wallet, string(q.Network)); err != nil {
		return err
	}
	if q.WalletKey != e.identity.Public().Hex() || q.Expires <= now {
		return errors.New("trade quote expired; review it again")
	}
	if err := s.Offer.Validate(now); err != nil {
		return err
	}
	if p.Kind == "taker" {
		event, ok := e.s.Book[p.Maker+":"+p.ID]
		if !ok || event.ID.Hex() != q.OfferEventID {
			return errors.New("signed offer changed; review the current order again")
		}
		o, err := protocol.DecodeOffer(event, now)
		if err != nil {
			return err
		}
		if o.Status != "open" {
			return errors.New("offer is no longer open")
		}
	}
	tower, err := e.selectProtection(s.Offer, p.TowerBPS, p.TowerPubKey, p.Kind == "maker")
	if err != nil {
		return err
	}
	if protocol.Digest(tower) != q.ProviderRevision {
		return errors.New("selected provider proof changed; review it again")
	}
	return validateFeeRate(q.Fees.Rate, q.Fees.Timestamp, q.Fees.FundingFee, q.FundingSize)
}

func (e *Engine) quoteTrade(ctx context.Context, raw json.RawMessage) (TradeQuote, error) {
	var p TradeQuoteRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return TradeQuote{}, err
	}
	if !e.tradeQuoteBusy.CompareAndSwap(false, true) {
		return TradeQuote{}, errors.New("a trade quote is already running; retry shortly")
	}
	defer e.tradeQuoteBusy.Store(false)
	e.mu.Lock()
	s, err := e.tradeSnapshot(p, time.Now().Unix())
	e.mu.Unlock()
	if err != nil {
		return TradeQuote{}, err
	}
	feeRaw, _ := json.Marshal(FeeQuoteRequest{Kind: "funding", Chain: s.Quote.PaidChain, Amount: s.Quote.PaidPrincipal, Fee: p.FundingFee})
	fee, err := e.quoteFee(ctx, feeRaw)
	if err != nil {
		s.Quote.Error = err.Error()
		return s.Quote, nil
	}
	s.Quote.FundingSize = fee.VSize
	fundsRaw, _ := json.Marshal(FundsPreflightRequest{Chain: s.Quote.PaidChain, Amount: s.Quote.PaidPrincipal, Fee: p.FundingFee, Inputs: fee.Inputs})
	funds, err := e.preflightFunds(ctx, Request{Method: "wallet.preflight", Params: fundsRaw})
	if err != nil {
		return s.Quote, err
	}
	s.Quote.Funds = funds
	s.Quote.Ready = funds.Sufficient && (funds.State == "proven" || funds.State == "not_applicable")
	if !s.Quote.Ready {
		s.Quote.Error = funds.Message
		return s.Quote, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TradeQuote{}, err
	}
	now := time.Now().Unix()
	if err := e.validateTradeSource(s, now); err != nil {
		return TradeQuote{}, err
	}
	if e.tradeQuotes == nil {
		e.tradeQuotes = map[string]TradeQuoteSnapshot{}
	}
	for token, old := range e.tradeQuotes {
		if old.Quote.Expires <= now {
			delete(e.tradeQuotes, token)
		}
	}
	if len(e.tradeQuotes) >= tradeQuoteCapacity {
		return TradeQuote{}, errors.New("too many open reviews; wait for an old quote to expire")
	}
	s.Quote.Token = transport.RandomID()
	s.Quote.Revision = protocol.Digest(s)
	e.tradeQuotes[s.Quote.Token] = s
	return s.Quote, nil
}

func (e *Engine) confirmTrade(ctx context.Context, raw json.RawMessage) (ConfirmTradeResult, error) {
	var p ConfirmTradeRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return ConfirmTradeResult{}, err
	}
	if !protocol.Hex32(p.RequestID) || !protocol.Hex32(p.Token) || !protocol.Hex32(p.Revision) {
		return ConfirmTradeResult{}, errors.New("confirmation requires the original request ID, quote token and revision")
	}
	digest := protocol.Digest(p)
	e.mu.Lock()
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return ConfirmTradeResult{}, err
	}
	if err := e.tradeBinding(p.ExpectedWallet, p.ExpectedNetwork); err != nil {
		e.mu.Unlock()
		return ConfirmTradeResult{}, err
	}
	if e.s.TradeReceipts == nil {
		e.s.TradeReceipts = map[string]*TradeReceipt{}
	}
	receipt := e.s.TradeReceipts[p.RequestID]
	if receipt != nil {
		if receipt.Digest != digest {
			e.mu.Unlock()
			return ConfirmTradeResult{}, errors.New("confirmation identity cannot be reused for changed terms")
		}
		if receipt.Result.State != "pending" || e.tradeConfirming[p.RequestID] {
			result := receipt.Result
			e.mu.Unlock()
			return result, nil
		}
	} else {
		if len(e.s.TradeReceipts) >= tradeReceiptCapacity {
			e.mu.Unlock()
			return ConfirmTradeResult{}, errors.New("trade confirmation history capacity reached")
		}
		s, ok := e.tradeQuotes[p.Token]
		receipt = &TradeReceipt{Digest: digest, Snapshot: s, Result: ConfirmTradeResult{ID: p.RequestID, Kind: s.Quote.Kind, State: "pending"}}
		var invalid error
		for _, prior := range e.s.TradeReceipts {
			if prior.Snapshot.Quote.Token == p.Token {
				invalid = errors.New("quote already confirmed with another request ID; retry the original confirmation")
			}
		}
		e.s.TradeReceipts[p.RequestID] = receipt
		if !ok || s.Quote.Revision != p.Revision || s.Quote.Wallet != p.ExpectedWallet || string(s.Quote.Network) != p.ExpectedNetwork {
			invalid = errors.New("quote is unavailable or changed; review it again")
		}
		if _, used := e.s.Offers[p.RequestID]; used || e.s.Swaps[p.RequestID] != nil {
			invalid = errors.New("request ID is already in use")
		}
		if invalid == nil {
			invalid = e.validateTradeSource(s, time.Now().Unix())
		}
		if invalid != nil {
			receipt.Result.State = "rejected"
			receipt.Result.Error = invalid.Error()
		}
		if err := e.save(); err != nil {
			e.mu.Unlock()
			return ConfirmTradeResult{}, err
		}
		if receipt.Result.State == "rejected" {
			result := receipt.Result
			e.mu.Unlock()
			return result, nil
		}
	}
	if e.tradeConfirming == nil {
		e.tradeConfirming = map[string]bool{}
	}
	e.tradeConfirming[p.RequestID] = true
	s := receipt.Snapshot
	e.mu.Unlock()
	fundsRaw, _ := json.Marshal(FundsPreflightRequest{Chain: s.Quote.PaidChain, Amount: s.Quote.PaidPrincipal, Fee: s.Quote.Fees.FundingFee, Inputs: s.Quote.Funds.Inputs})
	funds, readErr := e.preflightFunds(ctx, Request{Method: "wallet.preflight", Params: fundsRaw})
	e.mu.Lock()
	defer e.mu.Unlock()
	defer delete(e.tradeConfirming, p.RequestID)
	if err := ctx.Err(); err != nil {
		return ConfirmTradeResult{}, err
	}
	if e.fatal != nil {
		return ConfirmTradeResult{}, e.fatal
	}
	reject := func(err error) (ConfirmTradeResult, error) {
		receipt.Result.State = "rejected"
		receipt.Result.Error = err.Error()
		return receipt.Result, e.save()
	}
	if readErr != nil {
		return reject(readErr)
	}
	if !funds.Sufficient || (funds.State != "proven" && funds.State != "not_applicable") {
		return reject(errors.New("funds preflight changed: " + funds.Message))
	}
	if err := e.validateTradeSource(s, time.Now().Unix()); err != nil {
		return reject(err)
	}
	commandRaw, _ := json.Marshal(s.Request)
	var err error
	if s.Request.Kind == "maker" {
		_, err = e.createOffer(ctx, commandRaw, receipt)
	} else {
		_, err = e.takeOffer(ctx, commandRaw, receipt)
	}
	if err != nil {
		if e.fatal != nil {
			return ConfirmTradeResult{}, err
		}
		return reject(err)
	}
	return receipt.Result, nil
}

func tradeRequestID(receipt *TradeReceipt) string {
	if receipt != nil {
		return receipt.Result.ID
	}
	return transport.RandomID()
}
func acceptTrade(receipt *TradeReceipt) {
	if receipt != nil {
		receipt.Result.State = "accepted"
		receipt.Result.Error = ""
	}
}
func (e *Engine) validateTradeInputs(owner string, receipt *TradeReceipt) error {
	if receipt == nil {
		return nil
	}
	if protocol.Digest(e.s.CoinReservations[owner].Inputs) != protocol.Digest(receipt.Snapshot.Quote.Funds.Inputs) {
		return errors.New("funding inputs changed; review the economics and replay readiness again")
	}
	return nil
}
