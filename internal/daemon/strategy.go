package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// A strategy is an authorization over two ordinary T11 executors. Charges,
// signed funding, reservations, receipts and successor IDs have one owner: the
// child policy. Strategy accounting never estimates consumption from history.
type StrategySide struct {
	Target         int64 `json:"target"`
	MinimumReserve int64 `json:"minimum_reserve"`
	MaxExposure    int64 `json:"max_exposure"`
	MinOffer       int64 `json:"min_offer"`
	MaxOffer       int64 `json:"max_offer"`
	VolumeLimit    int64 `json:"volume_limit"`
	FundingFee     int64 `json:"funding_fee"`
	MaxFundingFee  int64 `json:"max_funding_fee"`
}
type StrategyConfig struct {
	ID                     string         `json:"id"`
	Wallet                 string         `json:"wallet"`
	Network                chain.Network  `json:"network"`
	BTC                    StrategySide   `json:"btc"`
	Blake                  StrategySide   `json:"blake"`
	Rate                   AutomationRate `json:"rate"`
	MinRate                AutomationRate `json:"min_rate"`
	MaxRate                AutomationRate `json:"max_rate"`
	SpreadBPS              int64          `json:"spread_bps"`
	MinSpreadBPS           int64          `json:"min_spread_bps"`
	MaxSpreadBPS           int64          `json:"max_spread_bps"`
	SkewBPS                int64          `json:"skew_bps"`
	Lifetime               int64          `json:"lifetime"`
	Cadence                int64          `json:"cadence"`
	MaxConcurrent          int            `json:"max_concurrent"`
	BTCFeeBudget           int64          `json:"btc_fee_budget"`
	BlakeFeeBudget         int64          `json:"blake_fee_budget"`
	TowerBPS               int64          `json:"tower_bps"`
	TowerPubKey            string         `json:"tower_pubkey"`
	Reference              string         `json:"reference"`
	ReferenceMakers        []string       `json:"reference_makers"`
	ReferenceFreshness     int64          `json:"reference_freshness"`
	ReferenceSpreadBPS     int64          `json:"reference_spread_bps"`
	MaxConsecutiveFailures int            `json:"max_consecutive_failures"`
	MaxReplacementFailures int            `json:"max_replacement_failures"`
	FailureRateBPS         int64          `json:"failure_rate_bps"`
}
type MakerStrategy struct {
	Config              StrategyConfig `json:"config"`
	WalletKey           string         `json:"wallet_key"`
	Revision            uint64         `json:"revision"`
	Enabled             bool           `json:"enabled"`
	RestoreHold         bool           `json:"restore_hold"`
	Tripped             bool           `json:"tripped"`
	Decision            string         `json:"decision"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	ReplacementFailures int            `json:"replacement_failures"`
	Outcomes            []bool         `json:"outcomes"` // Last 20 attempted actions, true is failure.
}
type StrategyEdit struct {
	Config                    StrategyConfig `json:"config"`
	ExpectedRevision          uint64         `json:"expected_revision"`
	Enabled                   bool           `json:"enabled"`
	AcknowledgeRestoredBudget bool           `json:"acknowledge_restored_budget"`
	ReviewDigest              string         `json:"review_digest"`
}
type StrategyQuote struct {
	Sell            chain.ID       `json:"sell"`
	SellAmount      int64          `json:"sell_amount"`
	BuyAmount       int64          `json:"buy_amount"`
	Rate            AutomationRate `json:"rate"`
	SourceOfferID   string         `json:"source_offer_id"`
	Ready           bool           `json:"ready"`
	Reason          string         `json:"reason"`
	ReferenceEvents []string       `json:"reference_events"`
	BTCFees         int64          `json:"btc_fees"`
	BlakeFees       int64          `json:"blake_fees"`
}
type StrategyInventory struct {
	Funds           ChainBalance `json:"funds"`
	Fresh           bool         `json:"fresh"`
	Exposure        int64        `json:"exposure"`
	ReservedVolume  int64        `json:"reserved_volume"`
	CommittedVolume int64        `json:"committed_volume"`
	ReservedFees    int64        `json:"reserved_fees"`
	CommittedFees   int64        `json:"committed_fees"`
	ConfirmedVolume int64        `json:"confirmed_volume"`
	KnownFees       int64        `json:"known_fees"`
	KnownBounties   int64        `json:"known_bounties"`
	UnknownFees     int          `json:"unknown_fees"`
}
type StrategyView struct {
	ReportIncluded      bool                           `json:"report_included"`
	Config              StrategyConfig                 `json:"config"`
	Revision            uint64                         `json:"revision"`
	Enabled             bool                           `json:"enabled"`
	RestoreHold         bool                           `json:"restore_hold"`
	Tripped             bool                           `json:"tripped"`
	Decision            string                         `json:"decision"`
	ConsecutiveFailures int                            `json:"consecutive_failures"`
	ReplacementFailures int                            `json:"replacement_failures"`
	BTCPolicyID         string                         `json:"btc_policy_id"`
	BlakePolicyID       string                         `json:"blake_policy_id"`
	Inventory           map[chain.ID]StrategyInventory `json:"inventory"`
	Quotes              []StrategyQuote                `json:"quotes"`
	ActiveQuotes        int                            `json:"active_quotes"`
	ActiveSwaps         int                            `json:"active_swaps"`
	ObservedAt          int64                          `json:"observed_at"`
}
type StrategyList struct {
	Wallet     string         `json:"wallet"`
	Network    chain.Network  `json:"network"`
	Strategies []StrategyView `json:"strategies"`
}
type StrategyReview struct {
	Config           StrategyConfig `json:"config"`
	ExpectedRevision uint64         `json:"expected_revision"`
	Enabled          bool           `json:"enabled"`
	ReviewDigest     string         `json:"review_digest"`
	Preview          StrategyView   `json:"preview"`
	Warning          string         `json:"warning"`
}

func strategyPolicyID(id string, sell chain.ID) string {
	return protocol.Digest([]string{"maker-strategy-v1", id, string(sell)})
}
func (c StrategyConfig) side(id chain.ID) StrategySide {
	if id == chain.BTC {
		return c.BTC
	}
	return c.Blake
}
func (c StrategyConfig) policy(sell chain.ID) AutomationConfig {
	s := c.side(sell)
	return AutomationConfig{ID: strategyPolicyID(c.ID, sell), StrategyID: c.ID, Wallet: c.Wallet, Network: c.Network, Sell: sell, SellAmount: s.MaxOffer, VolumeLimit: s.VolumeLimit,
		Rate: c.Rate, MinRate: c.MinRate, MaxRate: c.MaxRate, Lifetime: c.Lifetime, Cadence: c.Cadence, MaxOpen: c.MaxConcurrent,
		FundingFee: s.FundingFee, MaxFundingFee: s.MaxFundingFee, BTCFeeBudget: c.BTCFeeBudget, BlakeFeeBudget: c.BlakeFeeBudget,
		TowerBPS: c.TowerBPS, TowerPubKey: c.TowerPubKey, Reference: c.Reference, ReferenceMakers: c.ReferenceMakers, ReferenceFreshness: c.ReferenceFreshness, ReferenceSpreadBPS: c.ReferenceSpreadBPS}
}
func (e *Engine) strategyUsage(c StrategyConfig, except string) (map[chain.ID]StrategyInventory, int, int) {
	funds := e.chainBalances(e.publicCoins())
	u := map[chain.ID]StrategyInventory{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		p := e.s.Automations[strategyPolicyID(c.ID, id)]
		v := StrategyInventory{Funds: funds[id], Fresh: e.strategyChainFresh(id, time.Now().Unix())}
		if p != nil {
			a := e.automationUsage(p, except)
			v.ReservedVolume, v.CommittedVolume = a.ReservedVolume, a.CommittedVolume
		}
		u[id] = v
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if p := e.s.Automations[strategyPolicyID(c.ID, id)]; p != nil {
			a := e.automationUsage(p, except)
			b, k := u[chain.BTC], u[chain.Blake]
			b.ReservedFees += a.ReservedBTCFees
			b.CommittedFees += a.CommittedBTCFees
			k.ReservedFees += a.ReservedBlakeFees
			k.CommittedFees += a.CommittedBlakeFees
			u[chain.BTC], u[chain.Blake] = b, k
		}
	}
	quotes, swaps := 0, 0
	seen := map[string]bool{}
	for _, s := range e.s.Swaps {
		o, err := historicalOffer(s.Request.OfferEvent)
		if err != nil {
			continue
		}
		if s.Role == "maker" && o.Maker == e.identity.Public().Hex() {
			seen[o.ID] = true
		}
		if strategySettled(s, e.Config.Network.Confirmations()) && ((s.ShortFunding == "" && s.LongFunding == "" && s.Short.TxID == "" && s.Long.TxID == "") || e.strategyVerifiedSwaps[s.ID]) {
			continue
		}
		asset, amount := o.Sell, o.SellAmount
		if s.Role == "taker" {
			asset, amount = o.Sell.Other(), o.BuyAmount
		}
		v := u[asset]
		v.Exposure += amount
		u[asset] = v
		swaps++
	}
	for id, event := range e.s.Offers {
		if id == except || seen[id] {
			continue
		}
		o, err := historicalOffer(event)
		if err != nil {
			continue
		}
		if o.Status != "reserved" && (o.Status != "open" || o.Expires <= time.Now().Unix()) {
			continue
		}
		seen[id] = true
		v := u[o.Sell]
		v.Exposure += o.SellAmount
		u[o.Sell] = v
		quotes++
	}
	// Quarantine/absence is not evidence that old authorization was unfunded.
	// The same durable charge occupies one exposure slot until a represented
	// obligation or explicit positive settlement proof accounts for it.
	for _, p := range e.s.Automations {
		for id, c := range p.Charges {
			if seen[id] || c.State == "released" || (id == except && !c.Uncertain) {
				continue
			}
			if c.ExposureSettled != nil && !c.ExposureSettled.Held {
				continue
			}
			seen[id] = true
			v := u[p.Config.Sell]
			v.Exposure += c.Volume
			u[p.Config.Sell] = v
			swaps++
		}
	}
	return u, quotes, swaps
}
func strategySettled(s *Swap, confirmations int) bool {
	if !terminalSwap(s) {
		return false
	}
	if s.ShortFunding == "" && s.LongFunding == "" && s.Short.TxID == "" && s.Long.TxID == "" {
		return true
	}
	return s.LongSpend != "" && s.ShortSpend != "" && s.LongConfirmations >= confirmations && s.ShortConfirmations >= confirmations
}
func (e *Engine) strategyChainFresh(id chain.ID, now int64) bool {
	return e.fresh(id) && e.chainObserved[id] > 0 && e.chainObserved[id] <= now && now-e.chainObserved[id] <= 90
}

func (e *Engine) validateStrategyEdit(q StrategyEdit) error {
	c := q.Config
	if err := e.tradeBinding(c.Wallet, string(c.Network)); err != nil {
		return err
	}
	if err := validateStrategyConfig(c, e.identity.Public().Hex()); err != nil {
		return err
	}
	old := e.s.MakerStrategies[c.ID]
	if old == nil {
		if q.ExpectedRevision != 0 || len(e.s.MakerStrategies) >= 1 {
			return errors.New("one durable two-asset strategy per wallet; edit its existing authorization")
		}
	} else if old.Revision != q.ExpectedRevision || old.WalletKey != e.identity.Public().Hex() {
		return errors.New("strategy changed; reopen current wallet authorization")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		child := e.s.Automations[strategyPolicyID(c.ID, id)]
		revision := uint64(0)
		if child != nil {
			if old == nil || child.Config.StrategyID != c.ID {
				return errors.New("strategy child identity already used")
			}
			revision = child.Revision
		}
		if err := e.validateAutomationEditInternal(AutomationEdit{Config: c.policy(id), ExpectedRevision: revision, Enabled: q.Enabled, AcknowledgeRestoredBudget: q.AcknowledgeRestoredBudget}, true); err != nil {
			return err
		}
	}
	u, _, _ := e.strategyUsage(c, "")
	if c.BTCFeeBudget < u[chain.BTC].ReservedFees+u[chain.BTC].CommittedFees || c.BlakeFeeBudget < u[chain.Blake].ReservedFees+u[chain.Blake].CommittedFees {
		return errors.New("shared fee limits cannot erase either direction's reserved or committed charges")
	}
	if old != nil && old.RestoreHold && q.Enabled && !q.AcknowledgeRestoredBudget {
		return errors.New("imported strategy requires explicit review of potentially omitted later spending")
	}
	return nil
}

// Validate the complete reviewed configuration without reading runtime state or
// authorizing imported records. Preview may evaluate disabled policies, so every
// arithmetic/reference invariant must already hold before they are installed.
func validateStrategyConfig(c StrategyConfig, walletKey string) error {
	if c.Wallet == "" || c.Network == "" || !c.Network.Valid() || !protocol.Hex32(walletKey) {
		return errors.New("strategy requires a valid wallet identity and explicit network")
	}
	if !protocol.Hex32(c.ID) {
		return errors.New("strategy requires a new 32-byte ID")
	}
	if c.MinSpreadBPS < 1 || c.MaxSpreadBPS > 2000 || c.MinSpreadBPS > c.SpreadBPS || c.SpreadBPS > c.MaxSpreadBPS || c.SkewBPS < 0 || c.SkewBPS > c.MaxSpreadBPS-c.MinSpreadBPS {
		return errors.New("review ordered 1–2000 bps spread bounds and a bounded inventory skew")
	}
	if c.MaxConsecutiveFailures < 1 || c.MaxConsecutiveFailures > 20 || c.MaxReplacementFailures < 1 || c.MaxReplacementFailures > 20 || c.FailureRateBPS < 1 || c.FailureRateBPS > 10000 {
		return errors.New("review 1–20 consecutive/replacement failures and a 1–10000 bps failure-rate breaker")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		s := c.side(id)
		if s.MinimumReserve < 0 || s.Target <= s.MinimumReserve || s.Target > contract.MaxMoney || s.MaxExposure < s.MaxOffer || s.MaxExposure > contract.MaxMoney || s.MinOffer < 100000 || s.MinOffer > s.MaxOffer {
			return errors.New("each asset needs ordered target/reserve, exposure and whole-offer limits")
		}
		if err := validateAutomationConfig(c.policy(id), walletKey); err != nil {
			return err
		}
	}
	return nil
}

func strategyDigest(q StrategyEdit, key string) string {
	q.ReviewDigest = ""
	return protocol.Digest(struct {
		Edit      StrategyEdit
		WalletKey string
	}{q, key})
}
func (e *Engine) reviewStrategy(raw json.RawMessage) (StrategyReview, error) {
	var q StrategyEdit
	if err := json.Unmarshal(raw, &q); err != nil {
		return StrategyReview{}, err
	}
	if err := e.validateStrategyEdit(q); err != nil {
		return StrategyReview{}, err
	}
	p := MakerStrategy{Config: q.Config, Enabled: q.Enabled, WalletKey: e.identity.Public().Hex()}
	if old := e.s.MakerStrategies[q.Config.ID]; old != nil {
		p = *old
		p.Config = q.Config
		p.Enabled = q.Enabled
	}
	// Preview the reviewed prospective authority without changing durable
	// holds, breaker state or charges. Save repeats all validation.
	if q.Enabled {
		p.Tripped = false
		if q.AcknowledgeRestoredBudget {
			p.RestoreHold = false
		}
	}
	return StrategyReview{Config: q.Config, ExpectedRevision: q.ExpectedRevision, Enabled: q.Enabled, ReviewDigest: strategyDigest(q, e.identity.Public().Hex()), Preview: e.strategyView(&p), Warning: "Authorize both maker directions within the exact displayed limits. Gross signed-funding consumption never resets after refunds, reorgs, stop or restart. Each chain has one shared lifetime fee/rescue allowance. Preview uses confirmed coins and excludes pending receipts and locked principal; coin granularity can prevent a quote. All wallet offers and unsettled swaps count toward exposure and concurrency. Selected makers may collude. No profitability or fiat valuation is implied. Stop cancels only unreserved quotes; accepted swaps still settle. Imported reservations remain uncertain even after fresh authorization."}, nil
}
func (e *Engine) saveStrategy(raw json.RawMessage) (StrategyView, error) {
	var q StrategyEdit
	if err := json.Unmarshal(raw, &q); err != nil {
		return StrategyView{}, err
	}
	if err := e.validateStrategyEdit(q); err != nil {
		return StrategyView{}, err
	}
	if q.ReviewDigest != strategyDigest(q, e.identity.Public().Hex()) {
		return StrategyView{}, errors.New("review this exact two-asset authorization before saving")
	}
	if e.s.MakerStrategies == nil {
		e.s.MakerStrategies = map[string]*MakerStrategy{}
	}
	p := e.s.MakerStrategies[q.Config.ID]
	if p == nil {
		p = &MakerStrategy{WalletKey: e.identity.Public().Hex()}
		e.s.MakerStrategies[q.Config.ID] = p
	}
	p.Config = q.Config
	p.Revision++
	p.Enabled = q.Enabled
	if q.Enabled {
		p.Tripped = false
		p.ConsecutiveFailures = 0
		p.ReplacementFailures = 0
		p.Outcomes = nil
		if q.AcknowledgeRestoredBudget {
			p.RestoreHold = false
		}
	}
	p.Decision = "paused; accepted obligations retained"
	if q.Enabled {
		p.Decision = "authorized; waiting for fresh inventory and an eligible check"
	}
	if e.s.Automations == nil {
		e.s.Automations = map[string]*AutomationPolicy{}
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		c := q.Config.policy(id)
		child := e.s.Automations[c.ID]
		fresh := child == nil
		if fresh {
			child = &AutomationPolicy{WalletKey: p.WalletKey, Charges: map[string]*AutomationCharge{}}
			e.s.Automations[c.ID] = child
		}
		retireAutomationPending(&e.s, child, "strategy authorization changed; prior unsigned grant revoked")
		child.Config = c
		child.Revision++
		child.Enabled = q.Enabled
		if q.Enabled && q.AcknowledgeRestoredBudget {
			child.RestoreHold = false
		}
		child.NextAction = max(child.NextAction, time.Now().Unix()+c.Cadence)
		if fresh {
			child.NextAction = time.Now().Unix()
		}
		child.Decision = p.Decision
	}
	if err := e.save(); err != nil {
		return StrategyView{}, err
	}
	// Revoke still-unreserved old quotes after edits, including decreases in
	// size/exposure. Persisted child revisions have already revoked all retries.
	if err := e.cancelStrategyQuotes(p); err != nil {
		return e.strategyView(p), err
	}
	return e.strategyView(p), nil
}
func (e *Engine) stopStrategy(raw json.RawMessage) (StrategyView, error) {
	var q struct {
		ID               string `json:"id"`
		ExpectedWallet   string `json:"expected_wallet"`
		ExpectedNetwork  string `json:"expected_network"`
		ExpectedRevision uint64 `json:"expected_revision"`
		Stop             bool   `json:"stop"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return StrategyView{}, err
	}
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		return StrategyView{}, err
	}
	p := e.s.MakerStrategies[q.ID]
	if p == nil || p.Revision != q.ExpectedRevision {
		return StrategyView{}, errors.New("strategy changed; reload before pausing")
	}
	p.Enabled = false
	p.Revision++
	p.Decision = "paused; no new offers; accepted swaps continue"
	if q.Stop {
		p.Decision = "stopped; no new offers; accepted swaps continue"
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		child := e.s.Automations[strategyPolicyID(p.Config.ID, id)]
		child.Enabled = false
		child.Revision++
		retireAutomationPending(&e.s, child, p.Decision)
	}
	if err := e.save(); err != nil {
		return StrategyView{}, err
	}
	err := e.cancelStrategyQuotes(p)
	return e.strategyView(p), err
}
func (e *Engine) cancelStrategyQuotes(p *MakerStrategy) error {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		child := e.s.Automations[strategyPolicyID(p.Config.ID, id)]
		if child == nil {
			continue
		}
		for offer := range child.Charges {
			event, ok := e.s.Offers[offer]
			if !ok || e.activeOrderSwap(offer) {
				continue
			}
			o, err := historicalOffer(event)
			if err != nil || o.Status != "open" {
				continue
			}
			raw, _ := json.Marshal(map[string]string{"id": offer, "expected_wallet": e.Config.Name, "expected_event_id": event.ID.Hex()})
			if _, err = e.cancelOffer(raw); err != nil {
				p.Decision = "future actions revoked; open quote cancellation incomplete: " + err.Error()
				_ = e.save()
				return err
			}
		}
	}
	return nil
}
func (e *Engine) listStrategies(raw json.RawMessage) (StrategyList, error) {
	var q struct {
		ExpectedWallet  string `json:"expected_wallet"`
		ExpectedNetwork string `json:"expected_network"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return StrategyList{}, err
	}
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		return StrategyList{}, err
	}
	r := StrategyList{Wallet: e.Config.Name, Network: e.Config.Network, Strategies: []StrategyView{}}
	for _, p := range e.s.MakerStrategies {
		r.Strategies = append(r.Strategies, e.strategyView(p))
	}
	sort.Slice(r.Strategies, func(i, j int) bool { return r.Strategies[i].Config.ID < r.Strategies[j].Config.ID })
	return r, nil
}

func (e *Engine) strategyRate(c StrategyConfig, sell chain.ID, now int64) (AutomationRate, []string, error) {
	rate, events, err := e.automationPrice(c.policy(sell), now)
	if err != nil {
		return AutomationRate{}, nil, err
	}
	funds := e.chainBalances(e.publicCoins())
	side, peer := c.side(sell), c.side(sell.Other())
	imbalance := new(big.Rat).Sub(big.NewRat(funds[sell].TotalConfirmed, side.Target), big.NewRat(funds[sell.Other()].TotalConfirmed, peer.Target))
	if imbalance.Cmp(big.NewRat(1, 1)) > 0 {
		imbalance = big.NewRat(1, 1)
	}
	if imbalance.Cmp(big.NewRat(-1, 1)) < 0 {
		imbalance = big.NewRat(-1, 1)
	}
	imbalance.Mul(imbalance, big.NewRat(c.SkewBPS, 1))
	skew := new(big.Int).Quo(imbalance.Num(), imbalance.Denom()).Int64()
	spread := max(c.MinSpreadBPS, min(c.MaxSpreadBPS, c.SpreadBPS-skew))
	factor := 10000 + spread
	if sell == chain.Blake {
		factor = 10000 - spread
	}
	r := new(big.Rat).Mul(rate.rat(), big.NewRat(factor, 10000))
	if !r.Num().IsInt64() || !r.Denom().IsInt64() {
		return AutomationRate{}, nil, errors.New("spread-adjusted exact rate exceeds supported integer ratio")
	}
	result := AutomationRate{r.Num().Int64(), r.Denom().Int64()}
	if !result.valid() || r.Cmp(c.MinRate.rat()) < 0 || r.Cmp(c.MaxRate.rat()) > 0 {
		return AutomationRate{}, nil, errors.New("reference/skew price exceeds hard user bounds")
	}
	return result, events, nil
}
func (e *Engine) strategyHealth(p *MakerStrategy, now int64) error {
	if !p.Enabled || p.RestoreHold || p.Tripped {
		return errors.New("strategy disabled, imported or circuit breaker held")
	}
	if p.WalletKey != e.identity.Public().Hex() {
		return errors.New("strategy wallet identity changed")
	}
	if err := e.recoveryTradingReady(); err != nil {
		return err
	}
	if e.s.Paused {
		return errors.New("wallet trading paused")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if !e.strategyChainFresh(id, now) {
			return errors.New("both chain observations must be current before new market making")
		}
	}
	if len(e.Config.Relays) == 0 || !e.marketAllRelays || e.marketObservedAt <= 0 || e.marketObservedAt > now || now-e.marketObservedAt > 90 {
		return errors.New("relay view unavailable or stale; quotes paused")
	}
	return nil
}
func (e *Engine) strategyAvailable(id chain.ID, source string) int64 {
	owner := ""
	if source != "" {
		owner = "offer/" + source
	}
	reserved := e.reservedCoins(id, owner)
	var total int64
	for _, coin := range e.knownCoins(id) {
		if !reserved[chain.OutpointKey(coin.TxID, coin.Vout)] && coin.Confirmations >= e.Config.Network.Confirmations() {
			total += int64(coin.Amount)
		}
	}
	return total
}
func (e *Engine) strategyRisk(p *MakerStrategy, sell chain.ID, amount, buy, fee int64, source string) error {
	if err := e.strategyHealth(p, time.Now().Unix()); err != nil {
		return err
	}
	side := p.Config.side(sell)
	if amount < side.MinOffer || amount > side.MaxOffer || fee < 1 || fee > side.MaxFundingFee {
		return errors.New("offer size or current funding fee exceeds strategy limits")
	}
	u, quotes, swaps := e.strategyUsage(p.Config, source)
	v := u[sell]
	if quotes+swaps >= p.Config.MaxConcurrent || amount > side.MaxExposure-v.Exposure {
		return errors.New("wallet-wide exposure/concurrent obligation limit reached")
	}
	if amount > side.VolumeLimit-v.ReservedVolume-v.CommittedVolume {
		return errors.New("gross direction budget exhausted")
	}
	c := p.Config.policy(sell)
	c.SellAmount = amount
	charge := automationCharge(c, "", buy, fee)
	if charge.BTCFees > p.Config.BTCFeeBudget-u[chain.BTC].ReservedFees-u[chain.BTC].CommittedFees || charge.BlakeFees > p.Config.BlakeFeeBudget-u[chain.Blake].ReservedFees-u[chain.Blake].CommittedFees {
		return errors.New("shared per-chain fee/rescue budget exhausted")
	}
	owner := "strategy-preview"
	if source != "" {
		owner = "offer/" + source
	}
	candidate, err := e.reservationCandidate(owner, sell, amount+fee)
	if err != nil {
		return err
	}
	points := map[string]bool{}
	for _, point := range candidate.Inputs {
		points[pointKey(point)] = true
	}
	var locked int64
	for _, coin := range e.knownCoins(sell) {
		if points[chain.OutpointKey(coin.TxID, coin.Vout)] {
			locked += int64(coin.Amount)
		}
	}
	if e.strategyAvailable(sell, source)-locked < side.MinimumReserve {
		return errors.New("selected whole inputs would violate the unlocked reserve; split or add confirmed coins")
	}
	if e.chainBalances(e.publicCoins())[sell.Other()].UnlockedConfirmed < p.Config.side(sell.Other()).MinimumReserve {
		return errors.New("other asset is below its unlocked minimum reserve")
	}
	return nil
}

var errStrategyQuoteCurrent = errors.New("current quote already matches deterministic inventory plan")

func (e *Engine) strategySource(p *AutomationPolicy, now int64) OrderActionFields {
	ids := []string{}
	for id, c := range p.Charges {
		if c.Successor == "" && !c.Uncertain && !e.activeOrderSwap(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// Prefer the live quote to avoid advertising two versions of one side.
	for _, live := range []bool{true, false} {
		for _, id := range ids {
			event, ok := e.s.Offers[id]
			if !ok {
				continue
			}
			o, err := historicalOffer(event)
			if err != nil {
				continue
			}
			open := o.Status == "open" && o.Expires > now
			if open != live {
				continue
			}
			action := "recreate"
			if open {
				action = "replace"
			}
			f := OrderActionFields{OrderAction: action, SourceOfferID: id, SourceEventID: event.ID.Hex()}
			if _, err = e.orderSource(f, now); err == nil {
				return f
			}
		}
	}
	return OrderActionFields{}
}
func (e *Engine) strategyPlan(p *MakerStrategy, sell chain.ID, now int64) (AutomationConfig, StrategyQuote, OrderActionFields, error) {
	return e.planStrategy(p, sell, now, true)
}
func (e *Engine) planStrategy(p *MakerStrategy, sell chain.ID, now int64, preview bool) (AutomationConfig, StrategyQuote, OrderActionFields, error) {
	c := p.Config.policy(sell)
	q := StrategyQuote{Sell: sell}
	fields := OrderActionFields{}
	if child := e.s.Automations[c.ID]; child != nil {
		fields = e.strategySource(child, now)
	}
	source := ""
	if fields.OrderAction == "replace" {
		source = fields.SourceOfferID
	}
	q.SourceOfferID = fields.SourceOfferID
	rate, events, err := e.strategyRate(p.Config, sell, now)
	if err != nil {
		return c, q, fields, err
	}
	q.Rate, q.ReferenceEvents = rate, events
	u, _, _ := e.strategyUsage(p.Config, source)
	s := p.Config.side(sell)
	// Scale linearly toward target, clamp to reviewed whole-offer bounds. No
	// cross-asset valuation or unconfirmed receipt enters this arithmetic.
	size := new(big.Int).Mul(big.NewInt(s.MaxOffer), big.NewInt(e.strategyAvailable(sell, source)))
	size.Quo(size, big.NewInt(s.Target))
	if size.Cmp(big.NewInt(s.MaxOffer)) > 0 {
		size.SetInt64(s.MaxOffer)
	}
	amount := max(s.MinOffer, size.Int64())
	if source != "" {
		// A replacement transfers the current parent's exact remaining whole
		// quantity. Inventory can reprice it, but cannot resize that custody.
		parent := e.s.ParentOrders[source]
		if parent == nil || parent.Offer.Sell != sell || parent.Offer.FillPolicy.Mode != protocol.FillWhole {
			return c, q, fields, errors.New("strategy replacement lacks its exact whole parent")
		}
		amount = parent.Quantities.Available
	} else {
		amount = min(amount, s.MaxExposure-u[sell].Exposure, s.VolumeLimit-u[sell].ReservedVolume-u[sell].CommittedVolume)
	}
	if amount < s.MinOffer {
		return c, q, fields, errors.New("inventory/exposure/gross budget cannot support the minimum whole offer")
	}
	c.SellAmount = amount
	buy, err := automationAmounts(c, rate)
	if err != nil {
		return c, q, fields, err
	}
	q.SellAmount, q.BuyAmount = amount, buy
	// Preview conservatively assumes the funding cap. Execution obtains a fresh
	// estimate and repeats every invariant at confirmation and maker acceptance.
	charge := automationCharge(c, "", buy, c.MaxFundingFee)
	q.BTCFees, q.BlakeFees = charge.BTCFees, charge.BlakeFees
	fee := c.FundingFee
	if fee == 0 {
		fee = 1
	} // An estimate is obtained before any authorization.
	if preview {
		fee = c.MaxFundingFee
	}
	if err = e.strategyRisk(p, sell, amount, buy, fee, source); err != nil {
		return c, q, fields, err
	}
	q.Ready = true
	q.Reason = "confirmed inventory scaled toward target; exact spread/skew and shared authorization bounds satisfied"
	if source != "" {
		q.Reason = "existing whole quantity retained; current price and shared authorization bounds satisfied"
	}
	return c, q, fields, nil
}
func (e *Engine) strategyView(p *MakerStrategy) StrategyView {
	c := p.Config
	if p.RestoreHold && p.WalletKey == e.identity.Public().Hex() {
		c.Wallet = e.Config.Name
	}
	u, quotes, swaps := e.strategyUsage(c, "")
	v := StrategyView{Config: c, Revision: p.Revision, Enabled: p.Enabled, RestoreHold: p.RestoreHold, Tripped: p.Tripped, Decision: p.Decision, ConsecutiveFailures: p.ConsecutiveFailures, ReplacementFailures: p.ReplacementFailures, BTCPolicyID: strategyPolicyID(c.ID, chain.BTC), BlakePolicyID: strategyPolicyID(c.ID, chain.Blake), Inventory: u, ActiveQuotes: quotes, ActiveSwaps: swaps, ObservedAt: time.Now().Unix(), Quotes: []StrategyQuote{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		_, q, _, err := e.strategyPlan(p, id, time.Now().Unix())
		if err != nil {
			q.Ready = false
			q.Reason = err.Error()
		}
		v.Quotes = append(v.Quotes, q)
	}
	return v
}

func (e *Engine) strategyFailure(child *AutomationPolicy, replacement bool, err error) {
	p := e.s.MakerStrategies[child.Config.StrategyID]
	if p == nil {
		return
	}
	failed := err != nil
	p.Outcomes = append(p.Outcomes, failed)
	if len(p.Outcomes) > 20 {
		p.Outcomes = p.Outcomes[len(p.Outcomes)-20:]
	}
	if failed {
		p.ConsecutiveFailures++
		if replacement {
			p.ReplacementFailures++
		}
		p.Decision = err.Error()
	} else {
		p.ConsecutiveFailures = 0
		p.ReplacementFailures = 0
		p.Decision = "offer saved within inventory limits; relay acknowledgement pending"
	}
	failures := 0
	for _, bad := range p.Outcomes {
		if bad {
			failures++
		}
	}
	if !p.Tripped && (p.ConsecutiveFailures >= p.Config.MaxConsecutiveFailures || p.ReplacementFailures >= p.Config.MaxReplacementFailures || (len(p.Outcomes) >= 5 && int64(failures*10000) >= p.Config.FailureRateBPS*int64(len(p.Outcomes)))) {
		// Invalidate every review/report/stop identity issued before this trip.
		// A fresh complete review is required to grant future authority again.
		p.Revision++
		p.Tripped = true
		p.Enabled = false
		p.Decision = "circuit breaker: " + p.Decision + "; review authorization to resume"
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			a := e.s.Automations[strategyPolicyID(p.Config.ID, id)]
			a.Enabled = false
			a.Revision++
			retireAutomationPending(&e.s, a, p.Decision)
		}
	}
	if failed {
		_ = e.cancelStrategyQuotes(p)
	}
}

// This guard applies only before accepting a NEW request. Duplicate requests
// and accepted swaps keep their immutable settlement authority after stop.
func (e *Engine) strategyOfferHeld(o protocol.Offer) bool {
	for _, p := range e.s.MakerStrategies {
		child := e.s.Automations[strategyPolicyID(p.Config.ID, o.Sell)]
		if child == nil || child.Charges[o.ID] == nil {
			continue
		}
		rate, _, err := e.strategyRate(p.Config, o.Sell, time.Now().Unix())
		if err != nil {
			return true
		}
		c := child.Config
		c.SellAmount = o.SellAmount
		buy, err := automationAmounts(c, rate)
		if err != nil || buy != o.BuyAmount {
			return true
		}
		return e.strategyRisk(p, o.Sell, o.SellAmount, o.BuyAmount, e.fundingFee("offer/"+o.ID), o.ID) != nil
	}
	return false
}

func (e *Engine) strategyReceipt(p *AutomationPolicy, r *TradeReceipt) error {
	s := e.s.MakerStrategies[p.Config.StrategyID]
	if s == nil {
		return errors.New("missing strategy authorization")
	}
	q := r.Snapshot.Request
	if q.Sell != p.Config.Sell {
		return errors.New("strategy direction changed")
	}
	if q.SourceOfferID != "" {
		old := p.Charges[q.SourceOfferID]
		if old == nil || old.Uncertain || old.Successor != "" {
			return errors.New("strategy source ownership changed")
		}
	}
	c := p.Config
	c.SellAmount = q.SellAmount
	rate, _, err := e.strategyRate(s.Config, c.Sell, time.Now().Unix())
	if err != nil {
		return err
	}
	buy, err := automationAmounts(c, rate)
	if err != nil {
		return err
	}
	if buy != q.BuyAmount {
		return errors.New("inventory/reference price changed during review")
	}
	source := ""
	if q.OrderAction == "replace" {
		source = q.SourceOfferID
	}
	return e.strategyRisk(s, q.Sell, q.SellAmount, q.BuyAmount, q.FundingFee, source)
}

func validateStrategyState(s *State) error {
	if len(s.MakerStrategies) > 1 {
		return errors.New("invalid durable maker strategy count")
	}
	for id, p := range s.MakerStrategies {
		if p == nil || p.Config.ID != id || p.Revision == 0 || len(p.Outcomes) > 20 || p.Config.Network != s.Network.Normalized() {
			return errors.New("invalid durable maker strategy")
		}
		if err := validateStrategyConfig(p.Config, p.WalletKey); err != nil {
			return fmt.Errorf("invalid durable maker strategy: %w", err)
		}
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			c := s.Automations[strategyPolicyID(id, sell)]
			if c == nil || c.Config.StrategyID != id || c.Config.Sell != sell || c.WalletKey != p.WalletKey || protocol.Digest(c.Config) != protocol.Digest(p.Config.policy(sell)) {
				return errors.New("invalid maker strategy child accounting link")
			}
		}
	}
	for _, p := range s.Automations {
		if p.Config.StrategyID != "" && (s.MakerStrategies[p.Config.StrategyID] == nil || p.Config.ID != strategyPolicyID(p.Config.StrategyID, p.Config.Sell)) {
			return fmt.Errorf("missing durable strategy for policy %s", p.Config.ID)
		}
	}
	return nil
}
