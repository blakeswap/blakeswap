package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/big"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

const tradeQuoteLifetime int64 = 120
const tradeQuoteCapacity = 64

type TradeQuoteRequest struct {
	FeeSelection
	OrderActionFields
	FillOrderFields
	FillTakeFields
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
	OrderActionFields
	FillOrderFields
	FillTakeFields
	Available         int64          `json:"available"`
	TotalSellAmount   int64          `json:"total_sell_amount"`
	FundingReserve    int64          `json:"funding_reserve"`
	ExampleFill       *FillPreview   `json:"example_fill,omitempty"`
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
	AutomationID       string             `json:"automation_id,omitempty"`
	AutomationRevision uint64             `json:"automation_revision,omitempty"`
	Digest             string             `json:"digest"`
	Snapshot           TradeQuoteSnapshot `json:"snapshot"`
	Result             ConfirmTradeResult `json:"result"`
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
	p.FeeBudgets, p.BountyBudgets = maps.Clone(p.FeeBudgets), maps.Clone(p.BountyBudgets)
	if err := e.recoveryTradingReady(); err != nil {
		return s, err
	}
	if err := e.tradeBinding(p.ExpectedWallet, p.ExpectedNetwork); err != nil {
		return s, err
	}
	if p.Kind != "maker" && p.Kind != "taker" {
		return s, errors.New("trade quote kind must be maker or taker")
	}
	if p.Kind != "maker" && (p.OrderAction != "" || p.SourceOfferID != "" || p.SourceEventID != "") {
		return s, errors.New("order management requires a maker quote")
	}
	source, err := e.orderSource(p.OrderActionFields, now)
	if err != nil {
		return s, err
	}
	if p.FundingFee < 1 || (p.OwnerFeeCap != 0 && p.OwnerFeeCap != 20000) || p.Rate < 0 {
		return s, errors.New("review an explicit bounded funding and owner fee policy")
	}
	o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: p.SellAmount, FillPolicy: p.FillPolicy, Network: e.Config.Network, ID: transport.RandomID(), Maker: e.identity.Public().Hex(), Sell: p.Sell, SellAmount: p.SellAmount, BuyAmount: p.BuyAmount, Expires: p.Expires, Status: "open"}
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
		// The exact signed parent and revision bind economics. Public take
		// callers may omit these redundant maker fields; a supplied view must
		// still match exactly and cannot substitute a second price or asset.
		if p.Sell != "" && p.Sell != o.Sell || p.SellAmount != 0 && p.SellAmount != o.SellAmount || p.BuyAmount != 0 && p.BuyAmount != o.BuyAmount {
			return s, errors.New("order amounts changed; refresh the market before reviewing")
		}
		if p.ParentRevision != o.Revision {
			return s, errors.New("parent revision changed; review the exact current fill")
		}
		if _, err := o.FillPolicy.Quote(o.SellAmount, o.BuyAmount, o.Available, p.Quantity); err != nil {
			return s, err
		}
		if len(p.FeeBudgets) != 0 || len(p.BountyBudgets) != 0 {
			return s, errors.New("parent authorization budgets belong only to maker reviews")
		}
		if p.FillPolicy.Mode != "" && p.FillPolicy != o.FillPolicy {
			return s, errors.New("parent fill bounds changed")
		}
		p.FillPolicy = o.FillPolicy
		eventID = event.ID.Hex()
	} else if o.Expires == 0 {
		o.Expires = now + 24*3600
	}
	if err := o.Validate(now); err != nil {
		return s, err
	}
	if p.Kind == "maker" && (p.Quantity != 0 || p.ParentRevision != 0) {
		return s, errors.New("parent creation cannot include an existing child quantity or revision")
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
	q.FillOrderFields = FillOrderFields{FillPolicy: o.FillPolicy, FeeBudgets: maps.Clone(p.FeeBudgets), BountyBudgets: maps.Clone(p.BountyBudgets)}
	q.FillTakeFields = p.FillTakeFields
	q.Available, q.TotalSellAmount = o.Available, o.SellAmount
	q.OrderActionFields = p.OrderActionFields
	q.Expires = min(q.Expires, o.Expires)
	if p.OrderAction == "replace" {
		q.Expires = min(q.Expires, source.Expires)
	}
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
		amounts, err := o.FillPolicy.Quote(o.SellAmount, o.BuyAmount, o.Available, p.Quantity)
		if err != nil {
			return s, err
		}
		if err := protocol.ValidateRescueAmounts(tower.BPS, amounts.Buy); err != nil {
			return s, err
		}
		q.PaidChain, q.PaidPrincipal, q.ReceivedChain, q.ReceivedPrincipal = o.Sell.Other(), amounts.Buy, o.Sell, amounts.Sell
	}
	if p.FundingFee > feeLimits(q.PaidChain).Funding {
		return s, errors.New("funding fee exceeds the selected chain cap")
	}
	q.FundingReserve = p.FundingFee
	if p.Kind == "maker" {
		protected := o
		protected.TowerBPS = tower.BPS
		if tower.BPS > 0 {
			protected.Tower = &tower
		}
		parent, err := newParentOrder(protected, p.FeeSelection, p.FillOrderFields, now)
		if err != nil {
			return s, err
		}
		if p.OrderAction == "replace" {
			current := e.s.ParentOrders[source.ID]
			if current == nil {
				return s, errors.New("source parent authorization unavailable")
			}
			if _, err := current.planReplacement(parent); err != nil {
				return s, err
			}
		}
		q.FundingReserve, err = parent.fundingReserve(o.Available)
		if err != nil {
			return s, err
		}
	}
	q.PaidTotal = q.PaidPrincipal + q.FundingReserve
	ratio := new(big.Rat).SetFrac64(q.ReceivedPrincipal, q.PaidPrincipal)
	q.RateNumerator, q.RateDenominator, q.RateDisplay = ratio.Num().Int64(), ratio.Denom().Int64(), ratio.FloatString(8)
	ownerMax := int64(2000)
	if p.OwnerFeeCap > 0 {
		ownerMax = p.OwnerFeeCap
	}
	refundMax := max(ownerMax, protocol.RescueFees[len(protocol.RescueFees)-1])
	if p.Kind == "taker" {
		// These are this taker's exact one-child authorizations, derived from
		// local policy rather than copied from the remote parent's private caps.
		// The taker tower can refund its paid leg but cannot make its first
		// revelation or claim its incoming leg.
		q.FeeBudgets = map[chain.ID]int64{q.PaidChain: p.FundingFee + refundMax, q.ReceivedChain: ownerMax}
		q.BountyBudgets = map[chain.ID]int64{q.PaidChain: protocol.Bounty(q.PaidPrincipal, tower.BPS), q.ReceivedChain: 0}
	}
	add := func(kind string, id chain.ID, principal, maximum, bps int64) {
		bounty := protocol.Bounty(principal, bps)
		q.Outcomes = append(q.Outcomes, TradeOutcome{Kind: kind, Chain: id, Principal: principal, FeeMin: 2000, FeeMax: maximum, Bounty: bounty, NetMin: principal - maximum - bounty, NetMax: principal - 2000 - bounty})
	}
	add("owner_claim", q.ReceivedChain, q.ReceivedPrincipal, ownerMax, 0)
	add("owner_refund", q.PaidChain, q.PaidPrincipal, refundMax, 0)
	if tower.BPS > 0 {
		q.TowerCoverage = "refund only; owner must reveal first"
		if p.Kind == "maker" {
			q.TowerCoverage = "delayed incoming claim and own refund"
			add("tower_claim", q.ReceivedChain, q.ReceivedPrincipal, 20000, tower.BPS)
		}
		add("tower_refund", q.PaidChain, q.PaidPrincipal, 20000, tower.BPS)
	}
	if p.Kind == "maker" && o.Mode == protocol.FillPartial {
		example, err := o.FillPolicy.Suggested(o.SellAmount, o.BuyAmount, o.Available)
		if err != nil {
			return s, err
		}
		outcomes := make([]TradeOutcome, 0, len(q.Outcomes))
		for _, outcome := range q.Outcomes {
			principal := example.Sell
			if outcome.Chain == o.Sell.Other() {
				principal = example.Buy
			}
			bps := int64(0)
			if outcome.Kind == "tower_claim" || outcome.Kind == "tower_refund" {
				bps = tower.BPS
			}
			bounty := protocol.Bounty(principal, bps)
			outcome.Principal, outcome.Bounty = principal, bounty
			outcome.NetMin, outcome.NetMax = principal-outcome.FeeMax-bounty, principal-outcome.FeeMin-bounty
			outcomes = append(outcomes, outcome)
		}
		q.ExampleFill = &FillPreview{Quantity: example.Sell, BuyAmount: example.Buy, FundingFee: p.FundingFee, OwnerFeeCap: p.OwnerFeeCap, Outcomes: outcomes}
		q.Outcomes = nil
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
	if err := e.recoveryTradingReady(); err != nil {
		return err
	}
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
	if _, err := e.orderSource(p.OrderActionFields, now); err != nil {
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
		if o.Revision != p.ParentRevision || o.FillPolicy != p.FillPolicy {
			return errors.New("reviewed parent revision or fill bounds changed")
		}
		if amounts, err := o.FillPolicy.Quote(o.SellAmount, o.BuyAmount, o.Available, p.Quantity); err != nil || amounts.Sell != q.ReceivedPrincipal || amounts.Buy != q.PaidPrincipal {
			return errors.New("reviewed child quantity or rounded amount changed")
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
	allow := replacementFields(p)
	feeRaw, _ := json.Marshal(FeeQuoteRequest{Kind: "funding", Chain: s.Quote.PaidChain, Amount: s.Quote.PaidTotal - p.FundingFee, Fee: p.FundingFee, ExpectedWallet: p.ExpectedWallet, SourceOfferID: allow.SourceOfferID, SourceEventID: allow.SourceEventID})
	fee, err := e.quoteFee(ctx, feeRaw)
	if err != nil {
		s.Quote.Error = err.Error()
		return s.Quote, nil
	}
	s.Quote.FundingSize = fee.VSize
	fundsRaw, _ := json.Marshal(FundsPreflightRequest{Chain: s.Quote.PaidChain, Amount: s.Quote.PaidTotal - p.FundingFee, Fee: p.FundingFee, Inputs: fee.Inputs})
	funds, err := e.preflightFundsForOrder(ctx, Request{Method: "wallet.preflight", Params: fundsRaw}, allow)
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
	result := s.Quote
	result.FeeBudgets = maps.Clone(s.Quote.FeeBudgets)
	result.BountyBudgets = maps.Clone(s.Quote.BountyBudgets)
	return result, nil
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
	if receipt == nil {
		var archived TradeReceipt
		if found, err := e.archivedValue("trade_receipts", p.RequestID, &archived); err != nil {
			e.mu.Unlock()
			return ConfirmTradeResult{}, err
		} else if found {
			e.mu.Unlock()
			if archived.Digest != digest {
				return ConfirmTradeResult{}, errors.New("confirmation identity cannot be reused for changed terms")
			}
			return archived.Result, nil
		}
	}
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
		if err := e.admitWork("receipt"); err != nil {
			e.mu.Unlock()
			return ConfirmTradeResult{}, err
		}
		s, ok := e.tradeQuotes[p.Token]
		receipt = &TradeReceipt{Digest: digest, Snapshot: s, Result: ConfirmTradeResult{ID: p.RequestID, Kind: s.Quote.Kind, State: "pending"}}
		var invalid error
		priorToken := e.s.TradeTokens[p.Token]
		if priorToken == "" {
			if _, err := e.archivedValue("trade_tokens", p.Token, &priorToken); err != nil {
				e.mu.Unlock()
				return ConfirmTradeResult{}, err
			}
		}
		if priorToken != "" && priorToken != p.RequestID {
			invalid = errors.New("quote already confirmed with another request ID; retry the original confirmation")
		}
		for _, prior := range e.s.TradeReceipts {
			if prior.Snapshot.Quote.Token == p.Token {
				invalid = errors.New("quote already confirmed with another request ID; retry the original confirmation")
			}
		}
		e.s.TradeReceipts[p.RequestID] = receipt
		if protocol.Hex32(s.Quote.Token) && priorToken == "" {
			if e.s.TradeTokens == nil {
				e.s.TradeTokens = map[string]string{}
			}
			e.s.TradeTokens[s.Quote.Token] = p.RequestID
		}
		if !ok || s.Quote.Revision != p.Revision || s.Quote.Wallet != p.ExpectedWallet || string(s.Quote.Network) != p.ExpectedNetwork {
			invalid = errors.New("quote is unavailable or changed; review it again")
		}
		_, usedOffer := e.s.Offers[p.RequestID]
		_, usedHistory := e.s.OrderRecords[p.RequestID]
		usedRecovery := false
		if e.s.Recovery != nil {
			_, usedRecovery = e.s.Recovery.Offers[p.RequestID]
		}
		archivedIdentity := false
		for _, kind := range []string{"offers", "order_records", "quarantined_offers", "swaps"} {
			_, found, err := e.archiveRecord(kind, p.RequestID)
			if err != nil {
				e.mu.Unlock()
				return ConfirmTradeResult{}, err
			}
			archivedIdentity = archivedIdentity || found
		}
		if usedOffer || usedHistory || usedRecovery || archivedIdentity || e.s.Swaps[p.RequestID] != nil {
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
	fundsRaw, _ := json.Marshal(FundsPreflightRequest{Chain: s.Quote.PaidChain, Amount: s.Quote.PaidTotal - s.Quote.Fees.FundingFee, Fee: s.Quote.Fees.FundingFee, Inputs: s.Quote.Funds.Inputs})
	funds, readErr := e.preflightFundsForOrder(ctx, Request{Method: "wallet.preflight", Params: fundsRaw}, replacementFields(s.Request))
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
	reservation := e.s.CoinReservations[owner]
	reviewed := receipt.Snapshot.Quote.Funds.Inputs
	changed := errors.New("funding inputs changed; review the economics and replay readiness again")
	if reservation.Chain != receipt.Snapshot.Quote.PaidChain || len(reviewed) == 0 || len(reservation.Inputs) != len(reviewed) {
		return changed
	}
	// Backends may reorder unchanged UTXOs during the confirmation refresh.
	// Authorization binds the exact set, without substituting, adding, dropping
	// or duplicating an input. Preserve both stored orders and all later proofs.
	remaining := make(map[CoinOutpoint]bool, len(reviewed))
	for _, point := range reviewed {
		if remaining[point] {
			return changed
		}
		remaining[point] = true
	}
	for _, point := range reservation.Inputs {
		if !remaining[point] {
			return changed
		}
		delete(remaining, point)
	}
	return nil
}
