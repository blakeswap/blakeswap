package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// Every rate is BLAKE satoshis / BTC satoshis. No floating-point value ever
// authorizes a price, including the final integer-rounded offer amounts.
type AutomationRate struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

func (r AutomationRate) rat() *big.Rat { return new(big.Rat).SetFrac64(r.Numerator, r.Denominator) }
func (r AutomationRate) valid() bool {
	return r.Numerator > 0 && r.Denominator > 0 && r.Numerator <= contract.MaxMoney && r.Denominator <= contract.MaxMoney
}

type AutomationConfig struct {
	StrategyID         string         `json:"strategy_id,omitempty"`
	ID                 string         `json:"id"`
	Wallet             string         `json:"wallet"`
	Network            chain.Network  `json:"network"`
	Sell               chain.ID       `json:"sell"`
	SellAmount         int64          `json:"sell_amount"`
	VolumeLimit        int64          `json:"volume_limit"`
	Rate               AutomationRate `json:"rate"`
	MinRate            AutomationRate `json:"min_rate"`
	MaxRate            AutomationRate `json:"max_rate"`
	Lifetime           int64          `json:"lifetime"`
	Cadence            int64          `json:"cadence"`
	MaxOpen            int            `json:"max_open"`
	FundingFee         int64          `json:"funding_fee"` // Zero requires a fresh network estimate.
	MaxFundingFee      int64          `json:"max_funding_fee"`
	BTCFeeBudget       int64          `json:"btc_fee_budget"`
	BlakeFeeBudget     int64          `json:"blake_fee_budget"`
	TowerBPS           int64          `json:"tower_bps"`
	TowerPubKey        string         `json:"tower_pubkey"`
	Reference          string         `json:"reference"` // fixed or orderbook; no implicit oracle.
	ReferenceMakers    []string       `json:"reference_makers"`
	ReferenceFreshness int64          `json:"reference_freshness"`
	ReferenceSpreadBPS int64          `json:"reference_spread_bps"`
}
type AutomationCharge struct {
	ExposureSettled *StrategyExposureProof `json:"exposure_settled,omitempty"`
	OfferID         string                 `json:"offer_id"`
	Volume          int64                  `json:"volume"`
	BTCFees         int64                  `json:"btc_fees"`
	BlakeFees       int64                  `json:"blake_fees"`
	State           string                 `json:"state"` // reserved, committed, released; committed never reverses.
	Successor       string                 `json:"successor"`
	Uncertain       bool                   `json:"uncertain,omitempty"` // Imported reservations cannot be refunded or transferred by reauthorization.
}
type AutomationPolicy struct {
	Config            AutomationConfig             `json:"config"`
	WalletKey         string                       `json:"wallet_key"`
	Revision          uint64                       `json:"revision"`
	Enabled           bool                         `json:"enabled"`
	RestoreHold       bool                         `json:"restore_hold"`
	NextAction        int64                        `json:"next_action"`
	LastAction        int64                        `json:"last_action"`
	Decision          string                       `json:"decision"`
	CurrentOfferID    string                       `json:"current_offer_id"`
	ReferenceEvents   []string                     `json:"reference_events"`
	ReferenceObserved int64                        `json:"reference_observed"`
	Charges           map[string]*AutomationCharge `json:"charges"`
	Pending           *ConfirmTradeRequest         `json:"pending,omitempty"`
}
type AutomationUsage struct {
	ReservedVolume     int64 `json:"reserved_volume"`
	CommittedVolume    int64 `json:"committed_volume"`
	ReservedBTCFees    int64 `json:"reserved_btc_fees"`
	CommittedBTCFees   int64 `json:"committed_btc_fees"`
	ReservedBlakeFees  int64 `json:"reserved_blake_fees"`
	CommittedBlakeFees int64 `json:"committed_blake_fees"`
	OpenOffers         int   `json:"open_offers"`
}
type AutomationView struct {
	Config            AutomationConfig `json:"config"`
	Revision          uint64           `json:"revision"`
	Enabled           bool             `json:"enabled"`
	RestoreHold       bool             `json:"restore_hold"`
	NextAction        int64            `json:"next_action"`
	LastAction        int64            `json:"last_action"`
	Decision          string           `json:"decision"`
	CurrentOfferID    string           `json:"current_offer_id"`
	Publication       string           `json:"publication"`
	Usage             AutomationUsage  `json:"usage"`
	ReferenceEvents   []string         `json:"reference_events"`
	ReferenceObserved int64            `json:"reference_observed"`
}
type AutomationList struct {
	Wallet   string           `json:"wallet"`
	Network  chain.Network    `json:"network"`
	Policies []AutomationView `json:"policies"`
}
type AutomationEdit struct {
	Config                    AutomationConfig `json:"config"`
	ExpectedRevision          uint64           `json:"expected_revision"`
	Enabled                   bool             `json:"enabled"`
	ReviewDigest              string           `json:"review_digest"`
	AcknowledgeRestoredBudget bool             `json:"acknowledge_restored_budget"`
}
type AutomationReview struct {
	Config           AutomationConfig `json:"config"`
	ExpectedRevision uint64           `json:"expected_revision"`
	Enabled          bool             `json:"enabled"`
	ReviewDigest     string           `json:"review_digest"`
	Usage            AutomationUsage  `json:"usage"`
	Warning          string           `json:"warning"`
}

func (e *Engine) automationUsage(p *AutomationPolicy, except string) AutomationUsage {
	u := AutomationUsage{}
	for id, c := range p.Charges {
		if id == except && !c.Uncertain {
			continue
		}
		switch c.State {
		case "reserved":
			u.ReservedVolume += c.Volume
			u.ReservedBTCFees += c.BTCFees
			u.ReservedBlakeFees += c.BlakeFees
		case "committed":
			u.CommittedVolume += c.Volume
			u.CommittedBTCFees += c.BTCFees
			u.CommittedBlakeFees += c.BlakeFees
		}
		if e.activeOrderSwap(id) {
			u.OpenOffers++
			continue
		}
		if o, err := historicalOffer(e.s.Offers[id]); err == nil && o.Status == "open" && o.Expires > time.Now().Unix() {
			u.OpenOffers++
		}
	}
	return u
}

// Only local signed funding / explicit unfunded terminal states change budget
// accounting. Missing responses and chain reorgs never refund authorization.
func (e *Engine) reconcileAutomations() {
	for _, p := range e.s.Automations {
		for id, c := range p.Charges {
			if c.State == "committed" {
				continue
			}
			unknown, funded := false, false
			for _, s := range e.s.Swaps {
				var o protocol.Offer
				if s.Role != "maker" || json.Unmarshal([]byte(s.Request.OfferEvent.Content), &o) != nil || o.ID != id {
					continue
				}
				if s.ShortFunding != "" || s.Short.TxID != "" || s.ShortSent {
					funded = true
				}
				if !terminalSwap(s) {
					unknown = true
				}
			}
			if funded {
				c.State = "committed"
				continue
			}
			if unknown || p.RestoreHold || c.Uncertain {
				continue
			}
			// A local whole parent's durable withdrawal precedes its next-second
			// signed publication. Only a fully returned, never-spent allocation
			// can release a reservation during that publication gap.
			if e.s.ParentOrders[id] != nil {
				if e.automationParentUnfunded(id, c.Volume) {
					c.State = "released"
				}
				continue
			}
			o, err := historicalOffer(e.s.Offers[id])
			if err == nil && (o.Status == "cancelled" || (o.Status == "open" && o.Expires <= time.Now().Unix()) || (o.Status == "reserved" && e.finishedOrder(id) == "cancelled")) {
				c.State = "released"
			}
		}
	}
}

func (e *Engine) automationParentUnfunded(id string, volume int64) bool {
	p := e.s.ParentOrders[id]
	if p == nil || p.RestoreHold || p.Offer.ID != id || p.Offer.Maker != e.identity.Public().Hex() || p.Offer.SellAmount != volume || p.Economics != p.Offer.EconomicsDigest() || p.Offer.FillPolicy != (protocol.FillPolicy{Mode: protocol.FillWhole, Min: volume, Max: volume}) {
		return false
	}
	q := p.Quantities
	if !q.Closed || q.Total != volume || q.Released != volume || q.Available != 0 || q.Reserved != 0 || q.Committed != 0 || q.Filled != 0 || len(p.Fees) != 2 || len(p.Bounties) != 2 {
		return false
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		fee, feeOK := p.Fees[id]
		bounty, bountyOK := p.Bounties[id]
		if !feeOK || !bountyOK || fee.Reserved != 0 || fee.Consumed != 0 || bounty.Reserved != 0 || bounty.Consumed != 0 {
			return false
		}
	}
	return true
}

func (e *Engine) automationView(p *AutomationPolicy) AutomationView {
	config := p.Config
	if p.RestoreHold && p.WalletKey == e.identity.Public().Hex() && config.Network == e.Config.Network {
		config.Wallet = e.Config.Name
	}
	return AutomationView{Config: config, Revision: p.Revision, Enabled: p.Enabled, RestoreHold: p.RestoreHold, NextAction: p.NextAction, LastAction: p.LastAction, Decision: p.Decision, CurrentOfferID: p.CurrentOfferID, Publication: e.s.OrderRecords[p.CurrentOfferID].Publication, Usage: e.automationUsage(p, ""), ReferenceEvents: p.ReferenceEvents, ReferenceObserved: p.ReferenceObserved}
}
func (e *Engine) listAutomations(raw json.RawMessage) (AutomationList, error) {
	var q struct {
		ExpectedWallet  string `json:"expected_wallet"`
		ExpectedNetwork string `json:"expected_network"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return AutomationList{}, err
	}
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		return AutomationList{}, err
	}
	r := AutomationList{Wallet: e.Config.Name, Network: e.Config.Network, Policies: []AutomationView{}}
	for _, p := range e.s.Automations {
		r.Policies = append(r.Policies, e.automationView(p))
	}
	sort.Slice(r.Policies, func(i, j int) bool { return r.Policies[i].Config.ID < r.Policies[j].Config.ID })
	return r, nil
}
func (e *Engine) validateAutomationEdit(p AutomationEdit) error {
	return e.validateAutomationEditInternal(p, false)
}
func (e *Engine) validateAutomationEditInternal(p AutomationEdit, strategy bool) error {
	if !strategy && (p.Config.StrategyID != "" || (e.s.Automations[p.Config.ID] != nil && e.s.Automations[p.Config.ID].Config.StrategyID != "")) {
		return errors.New("edit this linked policy through its two-asset strategy authorization")
	}
	if p.Enabled {
		if err := e.recoveryTradingReady(); err != nil {
			return err
		}
	}
	c := p.Config
	if err := e.tradeBinding(c.Wallet, string(c.Network)); err != nil {
		return err
	}
	if err := validateAutomationConfig(c, e.identity.Public().Hex()); err != nil {
		return err
	}
	old := e.s.Automations[c.ID]
	if old == nil {
		if p.ExpectedRevision != 0 || len(e.s.Automations) >= 32 {
			return errors.New("unknown revision or automation capacity reached")
		}
	} else {
		if old.Revision != p.ExpectedRevision || old.Config.Sell != c.Sell || old.WalletKey != e.identity.Public().Hex() {
			return errors.New("policy revision, wallet or direction changed; reopen its authorization")
		}
		u := e.automationUsage(old, "")
		if c.VolumeLimit < u.ReservedVolume+u.CommittedVolume || c.BTCFeeBudget < u.ReservedBTCFees+u.CommittedBTCFees || c.BlakeFeeBudget < u.ReservedBlakeFees+u.CommittedBlakeFees {
			return errors.New("new limits cannot erase reserved or committed authorization")
		}
		if old.RestoreHold && p.Enabled && !p.AcknowledgeRestoredBudget {
			return errors.New("restored policy may omit later spending; explicitly review and authorize its remaining limits before enabling")
		}
	}
	_, err := automationAmounts(c, c.Rate)
	return err
}

// Intrinsic limits apply before durable strategy records enter import/load,
// as well as before a live authorization. This does not enable any policy.
func validateAutomationConfig(c AutomationConfig, walletKey string) error {
	if !protocol.Hex32(c.ID) || !c.Sell.Valid() || c.SellAmount < 100000 || c.SellAmount > 10000000000 || c.VolumeLimit < c.SellAmount || c.VolumeLimit > contract.MaxMoney {
		return errors.New("policy needs a new 32-byte ID, valid direction and exact bounded size/total sell volume")
	}
	if !c.Rate.valid() || !c.MinRate.valid() || !c.MaxRate.valid() || c.MinRate.rat().Cmp(c.MaxRate.rat()) > 0 || c.Rate.rat().Cmp(c.MinRate.rat()) < 0 || c.Rate.rat().Cmp(c.MaxRate.rat()) > 0 {
		return errors.New("fixed/minimum/maximum BLAKE-per-BTC ratios must be positive, ordered and bounded")
	}
	if c.Cadence < 60 || c.Cadence > 86400 || c.Lifetime < c.Cadence || c.Lifetime > 7*86400 || c.MaxOpen < 1 || c.MaxOpen > 8 {
		return errors.New("cadence must be 60–86400 seconds, lifetime cadence–7 days, maximum open offers 1–8")
	}
	if c.FundingFee < 0 || c.FundingFee > c.MaxFundingFee || c.MaxFundingFee < 1 || c.MaxFundingFee > feeLimits(c.Sell).Funding || c.BTCFeeBudget < 1 || c.BTCFeeBudget > contract.MaxMoney || c.BlakeFeeBudget < 1 || c.BlakeFeeBudget > contract.MaxMoney {
		return errors.New("review per-chain lifetime fee/bounty budgets and an explicit funding cap")
	}
	if c.TowerBPS < 0 || c.TowerBPS > 1000 || (c.TowerBPS > 0 && !protocol.Hex32(c.TowerPubKey)) || (c.TowerBPS == 0 && c.TowerPubKey != "") {
		return errors.New("select no protection or one pinned provider and a maximum rescue rate")
	}
	if c.Reference != "fixed" && c.Reference != "orderbook" {
		return errors.New("reference must be fixed or explicitly configured orderbook makers")
	}
	if c.Reference == "orderbook" {
		if len(c.ReferenceMakers) < 3 || len(c.ReferenceMakers) > 16 || c.ReferenceFreshness < 30 || c.ReferenceFreshness > 300 || c.ReferenceSpreadBPS < 1 || c.ReferenceSpreadBPS > 100 {
			return errors.New("orderbook reference needs 3–16 distinct chosen makers, 30–300 second freshness, and 1–100 bps maximum spread")
		}
		seen := map[string]bool{}
		for _, maker := range c.ReferenceMakers {
			if !protocol.Hex32(maker) || maker == walletKey || seen[maker] {
				return errors.New("reference makers must be distinct external identities")
			}
			seen[maker] = true
		}
	}
	_, err := automationAmounts(c, c.Rate)
	return err
}

func automationEditDigest(p AutomationEdit, key string) string {
	p.ReviewDigest = ""
	return protocol.Digest(struct {
		Edit      AutomationEdit
		WalletKey string
	}{p, key})
}
func (e *Engine) reviewAutomation(raw json.RawMessage) (AutomationReview, error) {
	var p AutomationEdit
	if err := json.Unmarshal(raw, &p); err != nil {
		return AutomationReview{}, err
	}
	if err := e.validateAutomationEdit(p); err != nil {
		return AutomationReview{}, err
	}
	u := AutomationUsage{}
	if old := e.s.Automations[p.Config.ID]; old != nil {
		u = e.automationUsage(old, "")
	}
	return AutomationReview{Config: p.Config, ExpectedRevision: p.ExpectedRevision, Enabled: p.Enabled, ReviewDigest: automationEditDigest(p, e.identity.Public().Hex()), Usage: u, Warning: "While this daemon runs, authorize future whole offers within these limits. Sell volume is gross, never replenished by refunds. Each chain fee budget includes its worst-case settlement fee and rescue bounty. Existing accepted terms remain immutable. Selected reference makers can collude; sparse, stale or conflicting data pauses repricing. Imported state may omit later spending; re-enabling expressly authorizes the displayed remaining limits and never refunds uncertain imported reservations."}, nil
}
func (e *Engine) saveAutomation(raw json.RawMessage) (AutomationView, error) {
	var p AutomationEdit
	if err := json.Unmarshal(raw, &p); err != nil {
		return AutomationView{}, err
	}
	if err := e.validateAutomationEdit(p); err != nil {
		return AutomationView{}, err
	}
	if p.ReviewDigest != automationEditDigest(p, e.identity.Public().Hex()) {
		return AutomationView{}, errors.New("review this exact policy authorization before saving")
	}
	if e.s.Automations == nil {
		e.s.Automations = map[string]*AutomationPolicy{}
	}
	old := e.s.Automations[p.Config.ID]
	newPolicy := old == nil
	if old == nil {
		old = &AutomationPolicy{WalletKey: e.identity.Public().Hex(), Charges: map[string]*AutomationCharge{}}
		e.s.Automations[p.Config.ID] = old
	}
	retireAutomationPending(&e.s, old, "policy authorization superseded; a fresh reviewed action is required")
	old.Config = p.Config
	old.Revision++
	old.Enabled = p.Enabled
	if p.Enabled && p.AcknowledgeRestoredBudget {
		old.RestoreHold = false
	}
	old.NextAction = max(old.NextAction, time.Now().Unix()+p.Config.Cadence)
	if newPolicy {
		old.NextAction = time.Now().Unix()
	}
	old.Decision = "disabled; existing obligations retained"
	if p.Enabled {
		old.Decision = "authorized; waiting for next eligible action"
	} else if old.RestoreHold {
		old.Decision = "Imported policy held: enabling requires a fresh review and acknowledgement of potentially omitted later spending."
	}
	return e.automationView(old), e.save()
}
func (e *Engine) disableAutomation(raw json.RawMessage) (AutomationView, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedWallet   string `json:"expected_wallet"`
		ExpectedNetwork  string `json:"expected_network"`
		ExpectedRevision uint64 `json:"expected_revision"`
		CancelOpen       bool   `json:"cancel_open"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return AutomationView{}, err
	}
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		return AutomationView{}, err
	}
	p := e.s.Automations[q.ID]
	if p == nil || p.Revision != q.ExpectedRevision || p.Config.StrategyID != "" {
		return AutomationView{}, errors.New("policy changed; reload before disabling")
	}
	retireAutomationPending(&e.s, p, "policy disabled; prior automatic authorization revoked")
	p.Enabled = false
	p.Revision++
	p.Decision = "disabled; existing obligations retained"
	// First durably revoke all new actions, even if cancellation later fails.
	if err := e.save(); err != nil {
		return AutomationView{}, err
	}
	if q.CancelOpen {
		for id := range p.Charges {
			event, ok := e.s.Offers[id]
			if !ok {
				continue
			}
			o, err := historicalOffer(event)
			if err != nil || o.Status != "open" || e.activeOrderSwap(id) {
				continue
			}
			data, _ := json.Marshal(map[string]string{"id": id, "expected_wallet": e.Config.Name, "expected_event_id": event.ID.Hex()})
			if _, err = e.cancelOffer(data); err != nil {
				p.Decision = "disabled; cancellation incomplete: " + err.Error()
				return e.automationView(p), e.save()
			}
		}
		p.Decision = "disabled; unreserved cancellations saved, relay acknowledgement may be pending"
	}
	return e.automationView(p), e.save()
}

func automationAmounts(c AutomationConfig, rate AutomationRate) (int64, error) {
	if !rate.valid() {
		return 0, errors.New("invalid price ratio")
	}
	r := rate.rat()
	if c.Sell == chain.Blake {
		r.Inv(r)
	}
	r.Mul(r, new(big.Rat).SetInt64(c.SellAmount))
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() < 100000 || q.Int64() > 10000000000 {
		return 0, errors.New("price produces an invalid whole-offer amount")
	}
	actual := AutomationRate{q.Int64(), c.SellAmount}
	if c.Sell == chain.Blake {
		actual = AutomationRate{c.SellAmount, q.Int64()}
	}
	if actual.rat().Cmp(c.MinRate.rat()) < 0 || actual.rat().Cmp(c.MaxRate.rat()) > 0 {
		return 0, errors.New("integer-rounded price exceeds hard authorized rate bounds")
	}
	return q.Int64(), nil
}

func (e *Engine) automationPrice(c AutomationConfig, now int64) (AutomationRate, []string, error) {
	if c.Reference == "fixed" {
		if !c.Rate.valid() {
			return AutomationRate{}, nil, errors.New("invalid fixed reference ratio")
		}
		return c.Rate, nil, nil
	}
	if c.Reference != "orderbook" || len(c.ReferenceMakers) < 3 || len(c.ReferenceMakers) > 16 {
		return AutomationRate{}, nil, errors.New("invalid orderbook reference quorum")
	}
	if !e.marketAllRelays || e.marketObservedAt > now || now-e.marketObservedAt > c.ReferenceFreshness {
		return AutomationRate{}, nil, errors.New("reference relay view is incomplete or stale")
	}
	type point struct {
		rate    AutomationRate
		event   string
		created int64
	}
	points := map[string]point{}
	for _, event := range e.s.Book {
		o, err := protocol.DecodeOffer(event, now)
		if err != nil || o.Network.Normalized() != e.Config.Network || o.Maker == e.identity.Public().Hex() || o.Sell != c.Sell || o.Status != "open" || int64(event.CreatedAt) > now || now-int64(event.CreatedAt) > c.ReferenceFreshness {
			continue
		}
		allowed := false
		for _, maker := range c.ReferenceMakers {
			if maker == o.Maker {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		r := AutomationRate{o.BuyAmount, o.SellAmount}
		if o.Sell == chain.Blake {
			r = AutomationRate{o.SellAmount, o.BuyAmount}
		}
		old, ok := points[o.Maker]
		if !ok || int64(event.CreatedAt) > old.created || (int64(event.CreatedAt) == old.created && event.ID.Hex() < old.event) {
			points[o.Maker] = point{r, event.ID.Hex(), int64(event.CreatedAt)}
		}
	}
	// Require every configured identity; silently reducing a quorum would let
	// an unavailable maker hand control to a smaller, possibly colluding subset.
	if len(points) != len(c.ReferenceMakers) {
		return AutomationRate{}, nil, errors.New("reference is sparse: every configured external maker needs a fresh signed quote")
	}
	a := []point{}
	for _, p := range points {
		a = append(a, p)
	}
	sort.Slice(a, func(i, j int) bool { return a[i].rate.rat().Cmp(a[j].rate.rat()) < 0 })
	spread := new(big.Rat).Quo(a[len(a)-1].rate.rat(), a[0].rate.rat())
	spread.Sub(spread, big.NewRat(1, 1))
	if spread.Cmp(big.NewRat(c.ReferenceSpreadBPS, 10000)) > 0 {
		return AutomationRate{}, nil, errors.New("reference makers conflict beyond the authorized spread")
	}
	events := []string{}
	for _, p := range a {
		events = append(events, p.event)
	}
	sort.Strings(events)
	return a[len(a)/2].rate, events, nil
}

func automationCharge(c AutomationConfig, offerID string, buy, funding int64) *AutomationCharge {
	// Reserve either chain's largest owner/tower settlement fee, and the
	// authorized provider bounty on each possible payout. No cross-asset sum.
	paid := funding + 20000 + protocol.Bounty(c.SellAmount, c.TowerBPS)
	incoming := int64(20000) + protocol.Bounty(buy, c.TowerBPS)
	charge := &AutomationCharge{OfferID: offerID, Volume: c.SellAmount, State: "reserved", BTCFees: paid, BlakeFees: incoming}
	if c.Sell == chain.Blake {
		charge.BTCFees, charge.BlakeFees = incoming, paid
	}
	return charge
}
func (e *Engine) automationBudget(p *AutomationPolicy, charge *AutomationCharge, source string) error {
	if prior := p.Charges[source]; prior != nil && (prior.State == "committed" || prior.Uncertain) {
		source = ""
	}
	u := e.automationUsage(p, source)
	c := p.Config
	if u.OpenOffers >= c.MaxOpen {
		return errors.New("maximum outstanding offers reached; accepted obligations must settle")
	}
	if charge.Volume > c.VolumeLimit-u.ReservedVolume-u.CommittedVolume || charge.BTCFees > c.BTCFeeBudget-u.ReservedBTCFees-u.CommittedBTCFees || charge.BlakeFees > c.BlakeFeeBudget-u.ReservedBlakeFees-u.CommittedBlakeFees {
		return errors.New("remaining authorized volume or chain fee/bounty budget is exhausted")
	}
	return nil
}
func automationFillAuthorization(sell chain.ID, quantity, buy, fee, bps int64) FillOrderFields {
	return FillOrderFields{FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: quantity, Max: quantity}, FeeBudgets: map[chain.ID]int64{sell: fee + 20000, sell.Other(): 20000}, BountyBudgets: map[chain.ID]int64{sell: protocol.Bounty(quantity, bps), sell.Other(): protocol.Bounty(buy, bps)}}
}

func (e *Engine) validateAutomationReceipt(r *TradeReceipt) error {
	if r == nil || r.AutomationID == "" {
		return nil
	}
	if err := e.recoveryTradingReady(); err != nil {
		return err
	}
	p := e.s.Automations[r.AutomationID]
	if p == nil || !p.Enabled || p.RestoreHold || p.Revision != r.AutomationRevision || p.WalletKey != e.identity.Public().Hex() || p.Pending == nil || p.Pending.RequestID != r.Result.ID {
		return errors.New("automation authorization changed or is held; future offer refused")
	}
	if len(e.Config.Relays) == 0 {
		return errors.New("automation requires a configured relay")
	}
	request := r.Snapshot.Request
	whole := automationFillAuthorization(p.Config.Sell, request.SellAmount, request.BuyAmount, request.FundingFee, p.Config.TowerBPS)
	if request.FundingFee < 1 || request.OwnerFeeCap != 20000 || request.TowerBPS != p.Config.TowerBPS || request.TowerPubKey != p.Config.TowerPubKey || request.FillPolicy != whole.FillPolicy || !maps.Equal(request.FeeBudgets, whole.FeeBudgets) || !maps.Equal(request.BountyBudgets, whole.BountyBudgets) {
		return errors.New("automatic authorization requires exact whole-child quantity, fees and protection limits")
	}
	if p.Config.StrategyID != "" {
		return e.strategyReceipt(p, r)
	}
	rate, _, err := e.automationPrice(p.Config, time.Now().Unix())
	if err != nil {
		return err
	}
	buy, err := automationAmounts(p.Config, rate)
	if err != nil {
		return err
	}
	s := r.Snapshot
	if s.Request.Sell != p.Config.Sell || s.Request.SellAmount != p.Config.SellAmount || s.Request.BuyAmount != buy || s.Request.FundingFee > p.Config.MaxFundingFee {
		return errors.New("automation economics changed while checking funds")
	}
	if source := s.Request.SourceOfferID; source != "" {
		if old := p.Charges[source]; old == nil || old.Successor != "" || old.Uncertain {
			return errors.New("automation source already has a successor or changed ownership")
		}
	}
	return e.automationBudget(p, automationCharge(p.Config, r.Result.ID, buy, s.Request.FundingFee), s.Request.SourceOfferID)
}

// Invoked by createOffer under the same lock, immediately before its only save.
func (e *Engine) acceptAutomationOffer(r *TradeReceipt) {
	if r == nil || r.AutomationID == "" {
		return
	}
	p := e.s.Automations[r.AutomationID]
	s := r.Snapshot.Request
	if old := p.Charges[s.SourceOfferID]; old != nil {
		old.Successor = r.Result.ID
		if old.State == "reserved" && !old.Uncertain {
			old.State = "released"
		}
	}
	config := p.Config
	config.SellAmount = s.SellAmount
	p.Charges[r.Result.ID] = automationCharge(config, r.Result.ID, s.BuyAmount, s.FundingFee)
	p.CurrentOfferID = r.Result.ID
	p.Pending = nil
	p.LastAction = time.Now().Unix()
	p.NextAction = p.LastAction + p.Config.Cadence
	p.Decision = "offer saved; awaiting relay acknowledgement"
}

// One action for one policy per tick. A durable pending receipt is the only
// retry identity; advancing the schedule before I/O prevents offline bursts.
func (e *Engine) runAutomations(ctx context.Context) {
	if !e.automationBusy.CompareAndSwap(false, true) {
		return
	}
	defer e.automationBusy.Store(false)
	e.mu.Lock()
	if e.fatal != nil || e.activityClosed || e.Config.Mode != "trader" || e.recoveryTradingReady() != nil {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	e.automationCancel = cancel
	e.activityReaders.Add(1)
	defer e.activityReaders.Done()
	defer cancel()
	now := time.Now().Unix()
	ids := []string{}
	for id, p := range e.s.Automations {
		if p.Enabled && !p.RestoreHold && p.NextAction <= now {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := e.s.Automations[ids[i]], e.s.Automations[ids[j]]
		if a.NextAction != b.NextAction {
			return a.NextAction < b.NextAction
		}
		return ids[i] < ids[j]
	})
	if len(ids) == 0 {
		e.mu.Unlock()
		return
	}
	p := e.s.Automations[ids[0]]
	if p.Pending != nil {
		p.NextAction = now + p.Config.Cadence
		if err := e.save(); err != nil {
			e.mu.Unlock()
			return
		}
		request := *p.Pending
		id, revision := p.Config.ID, p.Revision
		e.mu.Unlock()
		raw, _ := json.Marshal(request)
		result, err := e.confirmTrade(ctx, raw)
		e.finishAutomationAttempt(id, revision, request.RequestID, result, err)
		return
	}
	p.NextAction = now + p.Config.Cadence
	p.Decision = "checking current funds, fees and policy bounds"
	if err := e.save(); err != nil {
		e.mu.Unlock()
		return
	}
	config, revision := p.Config, p.Revision
	fail := func(err error) {
		p.Decision = err.Error()
		if p.Config.StrategyID != "" {
			e.strategyFailure(p, false, err)
		}
		_ = e.save()
		e.mu.Unlock()
	}
	if err := e.tradeBinding(config.Wallet, string(config.Network)); err != nil {
		fail(err)
		return
	}
	if len(e.Config.Relays) == 0 {
		fail(errors.New("no relay configured; policy paused"))
		return
	}
	rate, events, err := e.automationPrice(config, now)
	var strategyFields OrderActionFields
	var strategyQuote StrategyQuote
	if config.StrategyID != "" {
		strategy := e.s.MakerStrategies[config.StrategyID]
		config, strategyQuote, strategyFields, err = e.planStrategy(strategy, config.Sell, now, false)
		rate, events = strategyQuote.Rate, strategyQuote.ReferenceEvents
	}
	if err != nil {
		fail(err)
		return
	}
	buy, err := automationAmounts(config, rate)
	if err != nil {
		fail(err)
		return
	}
	request := TradeQuoteRequest{Kind: "maker", ExpectedWallet: config.Wallet, ExpectedNetwork: string(config.Network), Sell: config.Sell, SellAmount: config.SellAmount, BuyAmount: buy, Expires: now + config.Lifetime, TowerBPS: config.TowerBPS, TowerPubKey: config.TowerPubKey, FillOrderFields: FillOrderFields{FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: config.SellAmount, Max: config.SellAmount}}}
	// Prefer renewing a source without a successor. Accepted sources are never
	// replaced; an extra slot can use fresh funds while they settle.
	sources := []string{}
	for id, c := range p.Charges {
		if c.Successor == "" && !c.Uncertain && !e.activeOrderSwap(id) {
			sources = append(sources, id)
		}
	}
	sort.Strings(sources)
	for _, id := range sources {
		if config.StrategyID != "" {
			break
		}
		event, ok := e.s.Offers[id]
		if !ok {
			continue
		}
		o, decodeErr := historicalOffer(event)
		if decodeErr != nil {
			continue
		}
		action := "recreate"
		if o.Status == "open" && o.Expires > now {
			if config.Reference == "fixed" || (o.SellAmount == config.SellAmount && o.BuyAmount == buy) {
				continue
			}
			if e.s.OrderRecords[id].Publication != "relay_acknowledged" {
				fail(errors.New("waiting for previous offer publication before repricing"))
				return
			}
			action = "replace"
		}
		fields := OrderActionFields{OrderAction: action, SourceOfferID: id, SourceEventID: event.ID.Hex()}
		if _, sourceErr := e.orderSource(fields, now); sourceErr == nil {
			request.OrderActionFields = fields
			break
		}
	}
	if config.StrategyID != "" {
		request.OrderActionFields = strategyFields
		if strategyFields.OrderAction == "replace" {
			o, _ := historicalOffer(e.s.Offers[strategyFields.SourceOfferID])
			if o.SellAmount == config.SellAmount && o.BuyAmount == buy {
				p.Decision = errStrategyQuoteCurrent.Error()
				_ = e.save()
				e.mu.Unlock()
				return
			}
			if e.s.OrderRecords[o.ID].Publication != "relay_acknowledged" {
				fail(errors.New("waiting for previous strategy quote relay acknowledgement"))
				return
			}
		}
	}
	if err = e.automationBudget(p, automationCharge(config, "", buy, config.FundingFee), request.SourceOfferID); err != nil {
		fail(err)
		return
	}
	p.ReferenceEvents = events
	p.ReferenceObserved = e.marketObservedAt
	e.mu.Unlock()
	feeRaw, _ := json.Marshal(FeeQuoteRequest{ExpectedWallet: config.Wallet, Kind: "funding", Chain: config.Sell, Amount: config.SellAmount, Fee: config.FundingFee, SourceOfferID: replacementFields(request).SourceOfferID, SourceEventID: replacementFields(request).SourceEventID})
	fee, err := e.quoteFee(ctx, feeRaw)
	if err == nil && (fee.Error != "" || fee.Fee < 1 || fee.Fee > config.MaxFundingFee) {
		err = errors.New("current funding fee unavailable or above policy cap: " + fee.Error)
	}
	if err != nil {
		e.finishAutomationAttempt(config.ID, revision, "", ConfirmTradeResult{}, err)
		return
	}
	request.FeeSelection = FeeSelection{FundingFee: fee.Fee, OwnerFeeCap: 20000}
	// Automation authorizes one whole child. Derive its private limits only
	// after selecting the fresh funding fee; the policy's lifetime charges
	// remain a separate, permanent authorization ledger.
	request.FillOrderFields = automationFillAuthorization(config.Sell, config.SellAmount, buy, fee.Fee, config.TowerBPS)
	if config.FundingFee == 0 {
		request.Rate = fee.Estimate.Rate
		request.Timestamp = fee.Estimate.Timestamp
	}
	raw, _ := json.Marshal(request)
	quote, err := e.quoteTrade(ctx, raw)
	if err == nil && !quote.Ready {
		err = errors.New("automation paused: " + quote.Error)
	}
	if err != nil {
		e.finishAutomationAttempt(config.ID, revision, "", ConfirmTradeResult{}, err)
		return
	}
	e.mu.Lock()
	p = e.s.Automations[config.ID]
	if p == nil || p.Revision != revision || !p.Enabled || p.Pending != nil || e.fatal != nil {
		e.mu.Unlock()
		return
	}
	if err := e.admitWork("receipt"); err != nil {
		p.Decision = err.Error()
		_ = e.save()
		e.mu.Unlock()
		return
	}
	confirm := ConfirmTradeRequest{Token: quote.Token, Revision: quote.Revision, RequestID: transport.RandomID(), ExpectedWallet: config.Wallet, ExpectedNetwork: string(config.Network)}
	p.Pending = &confirm
	if e.s.TradeReceipts == nil {
		e.s.TradeReceipts = map[string]*TradeReceipt{}
	}
	r := &TradeReceipt{Digest: protocol.Digest(confirm), Snapshot: e.tradeQuotes[quote.Token], Result: ConfirmTradeResult{ID: confirm.RequestID, Kind: "maker", State: "pending"}, AutomationID: config.ID, AutomationRevision: revision}
	e.s.TradeReceipts[confirm.RequestID] = r
	if err = e.validateAutomationReceipt(r); err != nil {
		r.Result.State = "rejected"
		r.Result.Error = err.Error()
		p.Pending = nil
		p.Decision = err.Error()
		if p.Config.StrategyID != "" {
			e.strategyFailure(p, r.Snapshot.Request.OrderAction == "replace", err)
		}
	}
	if saveErr := e.save(); saveErr != nil {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	if err != nil {
		return
	}
	raw, _ = json.Marshal(confirm)
	result, err := e.confirmTrade(ctx, raw)
	e.finishAutomationAttempt(config.ID, revision, confirm.RequestID, result, err)
}
func (e *Engine) finishAutomationAttempt(id string, revision uint64, requestID string, result ConfirmTradeResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.s.Automations[id]
	if p == nil || e.fatal != nil {
		return
	}
	if p.Pending != nil && p.Pending.RequestID == requestID && result.State != "pending" && err == nil {
		p.Pending = nil
	}
	if p.Revision == revision {
		if p.Config.StrategyID != "" {
			failure := err
			if failure == nil && result.State == "rejected" {
				failure = errors.New(result.Error)
			}
			replacing := e.strategySource(p, time.Now().Unix()).OrderAction == "replace"
			if r := e.s.TradeReceipts[requestID]; r != nil {
				replacing = r.Snapshot.Request.OrderAction == "replace"
			}
			e.strategyFailure(p, replacing, failure)
		}
		if err != nil {
			p.Decision = fmt.Sprintf("paused: %v", err)
		} else if result.State == "rejected" {
			p.Decision = "paused: " + result.Error
		}
	}
	_ = e.save()
}

// Preserve the receipt identity and all known economic facts while revoking a
// pending automatic grant. Never reinterpret an accepted receipt as rejected.
func retireAutomationPending(s *State, p *AutomationPolicy, reason string) {
	if p.Pending == nil {
		return
	}
	if r := s.TradeReceipts[p.Pending.RequestID]; r != nil && r.Result.State == "pending" {
		r.Result.State, r.Result.Error = "rejected", reason
	}
	p.Pending = nil
}

// Called only on the private imported snapshot before any daemon starts.
func holdImportedAutomations(s *State) {
	for _, p := range s.MakerStrategies {
		p.Enabled = false
		p.RestoreHold = true
		p.Revision++
		p.Decision = "Imported strategy held; review remaining authorization and potentially omitted spending before enabling"
	}
	for _, p := range s.Automations {
		if p == nil {
			continue
		}
		for _, c := range p.Charges {
			if c != nil && c.ExposureSettled != nil {
				c.ExposureSettled.Held = true
			}
			if c != nil && c.State == "reserved" {
				c.Uncertain = true
			}
		}
		retireAutomationPending(s, p, "imported automatic authorization revoked; later economic outcomes may be absent from this snapshot")
		p.Enabled = false
		p.RestoreHold = true
		p.Revision++
		p.Decision = "Imported policy held: review remaining limits and acknowledge potentially omitted later spending before enabling."
	}
}

// Held or imported uncertain offers cannot authorize a new request, even if an
// older snapshot accidentally retains their live event. Accepted swaps keep
// their original terms and follow the existing recovery/settlement path.
func (e *Engine) automationOfferHeld(id string) bool {
	for _, p := range e.s.Automations {
		if c := p.Charges[id]; c != nil && (p.RestoreHold || c.Uncertain) {
			return true
		}
	}
	return false
}
