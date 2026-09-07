package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func strategyFixture(t *testing.T) (*Engine, *MakerStrategy) {
	t.Helper()
	e, _ := tradeFixture(t, "maker")
	e.s.CoinReservations = map[string]CoinReservation{}
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		var b *receiveBackend
		if send, ok := e.nodes[id].(*sendBackend); ok {
			b = send.receiveBackend
		} else {
			b = e.nodes[id].(*receiveBackend)
		}
		b.coins = []chain.UTXO{}
		for n := 0; n < 4; n++ {
			b.coins = append(b.coins, chain.UTXO{TxID: strings.Repeat(string(rune('a'+i)), 63) + string(rune('0'+n)), Amount: 500000, Script: hex.EncodeToString(e.scripts[id]), Confirmations: 2})
		}
		if err := e.refreshChain(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		e.chainObserved[id] = time.Now().Unix()
	}
	e.Config.Relays = []string{"ws://127.0.0.1:1"}
	e.marketAllRelays = true
	e.marketObservedAt = time.Now().Unix()
	side := StrategySide{Target: 2000000, MinimumReserve: 500000, MaxExposure: 1000000, MinOffer: 100000, MaxOffer: 200000, VolumeLimit: 1000000, FundingFee: 2000, MaxFundingFee: 4000}
	c := StrategyConfig{ID: transport.RandomID(), Wallet: e.Config.Name, Network: e.Config.Network, BTC: side, Blake: side, Rate: AutomationRate{1, 1}, MinRate: AutomationRate{1, 2}, MaxRate: AutomationRate{2, 1}, SpreadBPS: 100, MinSpreadBPS: 50, MaxSpreadBPS: 200, SkewBPS: 50, Lifetime: 120, Cadence: 60, MaxConcurrent: 4, BTCFeeBudget: 300000, BlakeFeeBudget: 300000, Reference: "fixed", MaxConsecutiveFailures: 3, MaxReplacementFailures: 2, FailureRateBPS: 8000}
	q := StrategyEdit{Config: c, Enabled: true}
	saveStrategyTest(t, e, q)
	return e, e.s.MakerStrategies[c.ID]
}
func saveStrategyTest(t *testing.T, e *Engine, q StrategyEdit) StrategyView {
	t.Helper()
	raw, _ := json.Marshal(q)
	review, err := e.reviewStrategy(raw)
	if err != nil {
		t.Fatal(err)
	}
	q.ReviewDigest = review.ReviewDigest
	raw, _ = json.Marshal(q)
	v, err := e.saveStrategy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStrategyExactTwoSidePreviewAndReservations(t *testing.T) {
	e, p := strategyFixture(t)
	before, _ := json.Marshal(e.s)
	v := e.strategyView(p)
	after, _ := json.Marshal(e.s)
	if string(before) != string(after) || len(v.Quotes) != 2 {
		t.Fatal("preview mutated authority")
	}
	for _, q := range v.Quotes {
		if !q.Ready || q.SellAmount != 200000 {
			t.Fatal(q)
		}
	}
	if v.Quotes[0].BuyAmount != 202000 || v.Quotes[1].BuyAmount != 202021 {
		t.Fatal("wrong integer spread", v.Quotes)
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		child := e.s.Automations[strategyPolicyID(p.Config.ID, id)]
		raw, _ := json.Marshal(AutomationEdit{Config: child.Config, ExpectedRevision: child.Revision})
		if _, err := e.reviewAutomation(raw); err == nil {
			t.Fatal("standalone child edit bypassed shared strategy")
		}
	}
	// A manual reservation consumes the same available coins. Whole-input
	// locking, not predicted unconfirmed change, must retain the hard reserve.
	coins := e.knownCoins(chain.Blake)
	e.s.CoinReservations["offer/manual"] = CoinReservation{Chain: chain.Blake, Inputs: []CoinOutpoint{{coins[0].TxID, 0}, {coins[1].TxID, 0}, {coins[2].TxID, 0}}}
	_, q, _, err := e.strategyPlan(p, chain.Blake, time.Now().Unix())
	if err == nil || q.Ready {
		t.Fatal("manual inputs spent reserve", q)
	}
}
func TestStrategyOpposingChargesNeverOverrunSharedFees(t *testing.T) {
	e, p := strategyFixture(t)
	p.Config.BTCFeeBudget = 60000
	p.Config.BlakeFeeBudget = 60000
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		e.s.Automations[strategyPolicyID(p.Config.ID, id)].Config = p.Config.policy(id)
	}
	// Opposing requests serialize under the same mutex and each completed offer
	// reserves its cross-chain allowance before the other can confirm.
	var wg sync.WaitGroup
	accepted := 0
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		wg.Add(1)
		go func(sell chain.ID) {
			defer wg.Done()
			e.mu.Lock()
			defer e.mu.Unlock()
			for n := 0; n < 5; n++ {
				if e.strategyRisk(p, sell, 200000, 202000, 2000, "") != nil {
					continue
				}
				child := e.s.Automations[strategyPolicyID(p.Config.ID, sell)]
				id := transport.RandomID()
				child.Charges[id] = automationCharge(child.Config, id, 202000, 2000)
				accepted++
			}
		}(sell)
	}
	wg.Wait()
	u, _, _ := e.strategyUsage(p.Config, "")
	if accepted != 2 || u[chain.BTC].ReservedFees > 60000 || u[chain.Blake].ReservedFees > 60000 {
		t.Fatal(accepted, u)
	}
	// Permanently charged signed funding cannot be released by refunds/reorgs.
	for _, child := range e.s.Automations {
		for id, c := range child.Charges {
			c.State = "committed"
			e.s.Swaps[id] = &Swap{ID: id, Role: "maker", Stage: "refunded", ShortFunding: "saved"}
		}
	}
	e.reconcileAutomations()
	u, _, _ = e.strategyUsage(p.Config, "")
	if u[chain.BTC].CommittedFees+u[chain.Blake].CommittedFees != 84000 {
		t.Fatal(u)
	}
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: false}
	q.Config.BTCFeeBudget = 1
	raw, _ := json.Marshal(q)
	if _, err := e.reviewStrategy(raw); err == nil {
		t.Fatal("edited away consumed fee budget")
	}
}
func TestStrategyPendingReceiptsAndUnknownClaimsAreNotInventory(t *testing.T) {
	e, p := strategyFixture(t)
	b := e.nodes[chain.Blake].(*sendBackend)
	for i := range b.coins {
		b.coins[i].Confirmations = 0
	}
	if err := e.refreshChain(context.Background(), chain.Blake); err != nil {
		t.Fatal(err)
	}
	e.htlcBalances = map[chain.ID]int64{chain.Blake: 100000000}
	e.htlcAvailable = map[chain.ID]bool{chain.Blake: true}
	if _, _, _, err := e.strategyPlan(p, chain.Blake, time.Now().Unix()); err == nil {
		t.Fatal("unconfirmed payout/locked claim became spendable")
	}
	v := e.strategyView(p)
	if v.Inventory[chain.Blake].Funds.Unconfirmed != 2000000 || v.Inventory[chain.Blake].Funds.UnlockedConfirmed != 0 {
		t.Fatal(v.Inventory)
	}
}
func TestStrategyBreakerStopRestartAndImportKeepAccounting(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	id := transport.RandomID()
	child.Charges[id] = automationCharge(child.Config, id, 202000, 2000)
	child.Pending = &ConfirmTradeRequest{RequestID: transport.RandomID()}
	e.s.TradeReceipts = map[string]*TradeReceipt{child.Pending.RequestID: {Result: ConfirmTradeResult{ID: child.Pending.RequestID, State: "pending"}}}
	e.strategyFailure(child, true, errors.New("replacement failed"))
	e.strategyFailure(child, true, errors.New("replacement failed"))
	if !p.Tripped || p.Enabled || child.Enabled || child.Pending != nil {
		t.Fatal("breaker did not revoke new authority", p)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	for n := 0; n < 3; n++ {
		e.runAutomations(context.Background())
	}
	if len(e.s.Offers) != 0 {
		t.Fatal("restart burst")
	}
	if err := PrepareRecovery(&e.s, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	p = e.s.MakerStrategies[p.Config.ID]
	child = e.s.Automations[child.Config.ID]
	if !p.RestoreHold || !child.RestoreHold || !child.Charges[id].Uncertain || child.Charges[id].State != "reserved" {
		t.Fatal("import reset accounting")
	}
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: false, AcknowledgeRestoredBudget: true}
	saveStrategyTest(t, e, q)
	if !p.RestoreHold || !child.Charges[id].Uncertain {
		t.Fatal("disabled edit released imported authority")
	}
}
func TestStrategyReferenceOutageAndSkewBounds(t *testing.T) {
	e, p := strategyFixture(t)
	e.marketAllRelays = false
	if _, _, _, err := e.strategyPlan(p, chain.Blake, time.Now().Unix()); err == nil {
		t.Fatal("relay outage accepted")
	}
	e.marketAllRelays = true
	e.chainObserved[chain.BTC] = time.Now().Unix() - 91
	if _, _, _, err := e.strategyPlan(p, chain.Blake, time.Now().Unix()); err == nil {
		t.Fatal("stale peer inventory accepted")
	}
	e.chainObserved[chain.BTC] = time.Now().Unix()
	p.Config.Reference = "orderbook"
	p.Config.ReferenceFreshness = 120
	p.Config.ReferenceSpreadBPS = 100
	for i := 0; i < 3; i++ {
		key := nostr.Generate()
		p.Config.ReferenceMakers = append(p.Config.ReferenceMakers, key.Public().Hex())
		o := protocol.Offer{ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: chain.Blake, SellAmount: 200000, BuyAmount: 200000, Status: "open", Expires: time.Now().Unix() + 120}
		if i == 2 {
			o.BuyAmount = 100000
		}
		raw, _ := o.PublicJSON()
		event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
		if err := transport.Sign(&event, key); err != nil {
			t.Fatal(err)
		}
		e.s.Book[o.Maker+":"+o.ID] = event
	}
	if _, _, _, err := e.strategyPlan(p, chain.Blake, time.Now().Unix()); err == nil {
		t.Fatal("manipulated conflicting makers accepted")
	}
}

func TestStrategyExecutorChargesActualSizeAndDoesNotDuplicate(t *testing.T) {
	e, p := strategyFixture(t)
	btc := e.s.Automations[strategyPolicyID(p.Config.ID, chain.BTC)]
	btc.NextAction = time.Now().Unix() + 3600
	blake := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	blake.NextAction = 0
	e.runAutomations(context.Background())
	if blake.CurrentOfferID == "" {
		t.Fatal(blake.Decision, p.Decision)
	}
	id := blake.CurrentOfferID
	o, err := historicalOffer(e.s.Offers[id])
	if err != nil {
		t.Fatal(err)
	}
	if blake.Charges[id].Volume != o.SellAmount || len(e.s.CoinReservations) != 1 {
		t.Fatal("non-atomic charge or reservation")
	}
	for n := 0; n < 4; n++ {
		blake.NextAction = 0
		e.runAutomations(context.Background())
	}
	if len(e.s.Offers) != 1 {
		t.Fatal("duplicate quotes", blake.Decision)
	}
	q, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network), "expected_revision": p.Revision, "stop": true})
	if _, err := e.stopStrategy(q); err != nil {
		t.Fatal(err)
	}
	if !e.strategyOfferHeld(o) {
		t.Fatal("stopped offer retained new acceptance authority")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.MakerStrategies[p.Config.ID].Enabled || saved.TradeReceipts[id].Result.State != "accepted" {
		t.Fatal("stop changed historical receipt")
	}
}

func TestStrategyMonitoringRetainsParentAuthorityDuringChildPause(t *testing.T) {
	e, p := strategyFixture(t)
	for _, child := range e.s.Automations {
		child.Enabled = false
	}
	e.chainFresh[chain.BTC] = false
	e.chainFresh[chain.Blake] = false
	found := false
	for _, a := range e.walletActions(time.Now().Unix()).Actions {
		if a.Kind == "strategy" && a.ObjectID == p.Config.ID && a.RequiresMonitoring {
			found = true
		}
	}
	if !found {
		t.Fatal("enabled parent disappeared while child policies paused")
	}
	p.Enabled = false
	for _, a := range e.walletActions(time.Now().Unix()).Actions {
		if a.Kind == "strategy" && a.RequiresMonitoring {
			t.Fatal("disabled empty parent still requires monitoring")
		}
	}
}
