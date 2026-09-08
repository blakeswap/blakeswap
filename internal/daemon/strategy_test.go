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
	e.s.Version, e.s.Network = StateVersion, chain.Regtest
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
		o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 200000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 200000, Max: 200000}, ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: chain.Blake, SellAmount: 200000, BuyAmount: 200000, Status: "open", Expires: time.Now().Unix() + 120}
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

func TestStrategyUnknownImportedChargesRetainExposureSlots(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	p.Config.Blake.VolumeLimit = 5000000
	child.Config = p.Config.policy(chain.Blake)
	id := transport.RandomID()
	child.Charges[id] = &AutomationCharge{OfferID: id, State: "reserved", Volume: 900000, BTCFees: 20000, BlakeFees: 22000, Uncertain: true}
	// Explicitly resumed imported allowance still knows of an uncertain possible
	// obligation, despite quarantine removing its live event and reservation.
	if err := e.strategyRisk(p, chain.Blake, 200000, 202000, 2000, ""); err == nil {
		t.Fatal("uncertain imported exposure disappeared with the live offer")
	}
}

func TestStrategyExactRemainingManualFeeBudgetExecutes(t *testing.T) {
	e, p := strategyFixture(t)
	edit := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	edit.Config.BlakeFeeBudget = 22000
	edit.Config.BTCFeeBudget = 20000
	saveStrategyTest(t, e, edit)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	e.s.Automations[strategyPolicyID(p.Config.ID, chain.BTC)].NextAction = time.Now().Unix() + 3600
	child.NextAction = 0
	if err := e.strategyRisk(p, chain.Blake, 200000, 202021, 2000, ""); err != nil {
		t.Fatal("reviewed exact fee does not fit", err)
	}
	e.runAutomations(context.Background())
	if child.CurrentOfferID == "" {
		t.Fatalf("exact selected 2000-sat fee fits budgets but executor refuses: child=%q parent=%q", child.Decision, p.Decision)
	}
}

func TestStrategyExposurePositiveProofAndReorgDoNotRefundCharges(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	id := transport.RandomID()
	o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 200000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 200000, Max: 200000}, ID: id, Network: e.Config.Network, Maker: e.identity.Public().Hex(), Sell: chain.Blake, SellAmount: 200000, BuyAmount: 202000, Status: "open", Expires: time.Now().Unix() + 120}
	event, err := e.signOffer(o, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := &Swap{ID: transport.RandomID(), Role: "maker", Request: protocol.Request{OfferEvent: event}, Stage: "refunded", ShortFunding: "durable signed funding", LongFunding: "peer funding", ShortSpend: strings.Repeat("c", 64), LongSpend: strings.Repeat("d", 64), ShortConfirmations: 2, LongConfirmations: 2}
	s.Short.TxID = strings.Repeat("a", 64)
	s.Long.TxID = strings.Repeat("b", 64)
	e.s.Swaps[s.ID] = s
	child.Charges[id] = automationCharge(child.Config, id, 202000, 2000)
	child.Charges[id].State = "committed"
	u, _, n := e.strategyUsage(p.Config, "")
	if u[chain.Blake].Exposure != 200000 || n != 1 {
		t.Fatal("cached outcome prematurely cleared exposure", u, n)
	}
	e.recordStrategyExposure(s)
	e.reconcileStrategyExposure()
	u, _, n = e.strategyUsage(p.Config, "")
	if u[chain.Blake].Exposure != 0 || n != 0 || u[chain.Blake].CommittedVolume != 200000 {
		t.Fatal("positive outcome changed consumption", u, n)
	}
	proof := *child.Charges[id].ExposureSettled
	delete(e.s.Swaps, s.ID) // Simulate positively settled archival, never plain absence.
	u, _, n = e.strategyUsage(p.Config, "")
	if u[chain.Blake].Exposure != 0 || n != 0 {
		t.Fatal("stored settlement identity lost at archive boundary")
	}
	e.s.Swaps[s.ID] = s
	s.ShortConfirmations = 0
	s.Stage = "awaiting chain confirmations"
	e.reconcileStrategyExposure()
	if !child.Charges[id].ExposureSettled.Held {
		t.Fatal("contradicted proof remained usable")
	}
	u, _, n = e.strategyUsage(p.Config, "")
	if u[chain.Blake].Exposure != 200000 || n != 1 || u[chain.Blake].CommittedVolume != 200000 {
		t.Fatal("reorg erased budget or exposure", u, n)
	}
	child.Charges[id].ExposureSettled = &proof
	holdImportedAutomations(&e.s)
	if !child.Charges[id].ExposureSettled.Held || child.Charges[id].ExposureSettled.SwapID != s.ID {
		t.Fatal("import reused or deleted historical proof")
	}
}

func TestStrategyReportUsesActualConfirmedActivityAndIsAdvisory(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	order := transport.RandomID()
	child.Charges[order] = automationCharge(child.Config, order, 202000, 6500)
	child.Charges[order].State = "committed"
	o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 200000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 200000, Max: 200000}, ID: order, Network: e.Config.Network, Maker: e.identity.Public().Hex(), Sell: chain.Blake, SellAmount: 200000, BuyAmount: 202000, Status: "open", Expires: time.Now().Unix() + 120}
	event, err := e.signOffer(o, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := automationChildRequest(t, e, event)
	swapID := request.ID
	e.s.Swaps[swapID] = &Swap{ID: swapID, Role: "maker", Request: request, Stage: "completed", ShortSpend: "peer-claim", LongSpend: "claim", ShortConfirmations: 2, LongConfirmations: 2}
	e.s.Swaps[swapID].Short.TxID = "funding"
	e.s.Swaps[swapID].Long.TxID = "peer-funding"
	e.strategyVerifiedSwaps = map[string]bool{swapID: true}
	// A separate positively observed refund is still a fee, never trade volume.
	child.Charges[order].ExposureSettled = &StrategyExposureProof{SwapID: "refunded", Sell: chain.Blake, FundingTxID: "refund-funding", SpendTxID: "refund", PeerSpendTxID: "peer-refund", CheckedAt: time.Now().Unix()}
	now := time.Now().Unix()
	for _, a := range []Activity{
		{ID: "fund", Kind: "swap_funding", Chain: chain.Blake, OrderID: order, SwapID: swapID, TxID: "funding", Principal: 200000, Fee: 6500, FeeKnown: true, FeePayer: "wallet", Movement: true, Status: "confirmed", Confirmations: 2, ObservedAt: now},
		{ID: "claim", Kind: "swap_claim", Chain: chain.BTC, OrderID: order, SwapID: swapID, TxID: "claim", Principal: 202000, Amount: 196000, Fee: 6000, FeeKnown: true, FeePayer: "wallet", Movement: true, Status: "confirmed", Confirmations: 2, ObservedAt: now},
		{ID: "refund", Kind: "swap_refund", Chain: chain.Blake, OrderID: order, SwapID: "refunded", TxID: "refund", Principal: 200000, Fee: 2000, FeeKnown: true, FeePayer: "wallet", Movement: true, Status: "confirmed", Confirmations: 2, ObservedAt: now},
		{ID: "pending", Kind: "swap_claim", Chain: chain.BTC, OrderID: order, SwapID: "pending", TxID: "pending", Fee: 20000, FeeKnown: true, FeePayer: "wallet", Movement: true, Status: "mempool", ObservedAt: now},
	} {
		e.s.Activities[a.ID] = a
	}
	// A foreign maker's canonical trade deliberately reuses the same order ID.
	// It must not be charged to this strategy's report even though this wallet
	// was its taker, or if it happened after the strategy's own trade.
	foreignSwapID := transport.RandomID()
	foreign := e.s.Activities["fund"]
	foreign.ID, foreign.SwapID, foreign.TxID = "foreign-fund", foreignSwapID, "foreign-funding"
	e.s.Activities[foreign.ID] = foreign
	foreign = e.s.Activities["claim"]
	foreign.ID, foreign.SwapID, foreign.TxID = "foreign-claim", foreignSwapID, "foreign-claim"
	e.s.Activities[foreign.ID] = foreign
	other := nostr.Generate()
	o.Maker = other.Public().Hex()
	public, _ := o.PublicJSON()
	foreignEvent := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", order}, {"t", e.Config.Network.Namespace()}}, Content: string(public)}
	if err := transport.Sign(&foreignEvent, other); err != nil {
		t.Fatal(err)
	}
	foreignRequest := automationChildRequest(t, e, foreignEvent)
	foreignRequest.ID = foreignSwapID
	e.s.Swaps[foreignSwapID] = &Swap{ID: foreignSwapID, Role: "taker", Request: foreignRequest, Stage: "completed"}
	raw, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network), "expected_revision": p.Revision})
	// Install the explicit accounting fixture as a committed checkpoint. A
	// report no longer assembles uncommitted Engine maps from a different view.
	if err := e.vault.Save(e.s); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(e.s)
	v, err := e.strategyReport(context.Background(), raw)
	after, _ := json.Marshal(e.s)
	if err != nil || !v.ReportIncluded || v.Inventory[chain.Blake].KnownFees != 8500 || v.Inventory[chain.BTC].KnownFees != 6000 || v.Inventory[chain.Blake].ConfirmedVolume != 200000 || v.Inventory[chain.BTC].ConfirmedVolume != 0 {
		t.Fatal(v, err)
	}
	if string(before) != string(after) {
		t.Fatal("report changed accounting")
	}
	if e.strategyView(p).ReportIncluded {
		t.Fatal("ordinary list/review ran lifetime report")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.strategyReport(cancelled, raw); err == nil {
		t.Fatal("partial cancelled report returned")
	}
	a := e.s.Activities["claim"]
	a.Status = "unknown"
	a.Confirmations = 0
	e.s.Activities["claim"] = a
	if err := e.vault.Save(e.s); err != nil {
		t.Fatal(err)
	}
	v, err = e.strategyReport(context.Background(), raw)
	if err != nil || v.Inventory[chain.Blake].ConfirmedVolume != 0 || v.Inventory[chain.Blake].CommittedVolume != 200000 {
		t.Fatal("reorg report restored authorization", v, err)
	}
}

func TestStrategyExternalOrderIDCannotHideLocalUncertainExposure(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	id := transport.RandomID()
	child.Charges[id] = &AutomationCharge{OfferID: id, State: "reserved", Volume: 200000, Uncertain: true}
	other := nostr.Generate()
	o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 200000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 200000, Max: 200000}, ID: id, Network: e.Config.Network, Maker: other.Public().Hex(), Sell: chain.BTC, SellAmount: 200000, BuyAmount: 200000, Status: "open", Expires: time.Now().Unix() + 120}
	raw, _ := o.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", id}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, other); err != nil {
		t.Fatal(err)
	}
	e.s.Swaps["rejected-other"] = &Swap{ID: "rejected-other", Role: "taker", Request: protocol.Request{OfferEvent: event}, Stage: "rejected"}
	u, _, n := e.strategyUsage(p.Config, "")
	if u[chain.Blake].Exposure != 200000 || n != 1 {
		t.Fatal("external maker ID collided with local authority", u, n)
	}
}

func TestStrategyNewAuthorizationPreviewUsesCurrentIdentityWithoutSaving(t *testing.T) {
	e, p := strategyFixture(t)
	config := p.Config
	e.s.MakerStrategies = nil
	delete(e.s.Automations, strategyPolicyID(config.ID, chain.BTC))
	delete(e.s.Automations, strategyPolicyID(config.ID, chain.Blake))
	before, _ := json.Marshal(e.s)
	raw, _ := json.Marshal(StrategyEdit{Config: config, Enabled: true})
	r, err := e.reviewStrategy(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r.Preview.Quotes {
		if !q.Ready {
			t.Fatal("new reviewed authority did not preview its current wallet", q)
		}
	}
	after, _ := json.Marshal(e.s)
	if string(before) != string(after) {
		t.Fatal("prospective preview changed durable authority")
	}
}
