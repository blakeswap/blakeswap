package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"

	"github.com/blakeswap/blakeswap/internal/transport"
)

func automationFixture(t *testing.T) (*Engine, *AutomationPolicy) {
	t.Helper()
	e, _ := tradeFixture(t, "maker")
	e.s.Version, e.s.Network = StateVersion, chain.Regtest
	e.Config.Relays = []string{"ws://127.0.0.1:1"} // No external service or publication.
	c := AutomationConfig{ID: transport.RandomID(), Wallet: e.Config.Name, Network: e.Config.Network, Sell: chain.Blake, SellAmount: 100000, VolumeLimit: 300000, Rate: AutomationRate{1, 2}, MinRate: AutomationRate{1, 4}, MaxRate: AutomationRate{1, 1}, Lifetime: 120, Cadence: 60, MaxOpen: 1, FundingFee: 2000, MaxFundingFee: 4000, BTCFeeBudget: 100000, BlakeFeeBudget: 100000, Reference: "fixed"}
	p := savePolicy(t, e, AutomationEdit{Config: c, Enabled: true})
	e.s.Automations[p.Config.ID].NextAction = 0
	return e, e.s.Automations[p.Config.ID]
}
func savePolicy(t *testing.T, e *Engine, p AutomationEdit) AutomationView {
	t.Helper()
	raw, _ := json.Marshal(p)
	review, err := e.reviewAutomation(raw)
	if err != nil {
		t.Fatal(err)
	}
	p.ReviewDigest = review.ReviewDigest
	raw, _ = json.Marshal(p)
	v, err := e.saveAutomation(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Advance the expiry transition's explicit clock, without rewriting any signed
// economics or an earlier committed parent. The cancellation is an actual
// parent withdrawal with a later signed tombstone, not a relay-only snapshot.
func expirePolicyParent(t *testing.T, e *Engine, p *AutomationPolicy) string {
	t.Helper()
	id := p.CurrentOfferID
	parent := e.s.ParentOrders[id]
	if parent == nil {
		t.Fatal("missing current parent")
	}
	before := parent.Economics
	if err := e.expireParents(parent.Offer.Expires); err != nil {
		t.Fatal(err)
	}
	if parent.Economics != before || !parent.Quantities.Closed || parent.Quantities.Withdrawn != parent.Quantities.Total {
		t.Fatal("expiry changed economics or failed to withdraw the unfilled parent")
	}
	p.NextAction = 0
	e.reconcileReservations()
	return id
}
func expirePolicyOffer(t *testing.T, e *Engine, p *AutomationPolicy) string {
	t.Helper()
	id := expirePolicyParent(t, e, p)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return id
}

// Exercise acceptance and signed funding with the original exact parent inputs
// and fee caps. No chain outcome is inferred from this private signing fixture.
func fundPolicyOffer(t *testing.T, e *Engine, id string) *Swap {
	t.Helper()
	from, message := automationOrderRequest(t, e, e.s.Offers[id])
	if err := e.handle(from, message); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[message.SwapID]
	if s == nil || e.s.FillRecords[s.ID] == nil {
		t.Fatal("accepted child custody missing")
	}
	// A same-second acceptance remains pending publication; wait for its normal
	// next-second signed revision before presenting the parent as reserved.
	parent := e.s.ParentOrders[id]
	wait := time.Until(time.Unix(parent.LastSignedAt+1, 0))
	if wait > 2*time.Second {
		t.Fatal("unexpected parent publication deadline")
	}
	if wait > 0 {
		time.Sleep(wait)
	}
	if err := e.publishPendingParents(); err != nil {
		t.Fatal(err)
	}
	tx, err := e.fundReserved(context.Background(), s.Short, "swap/"+s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.commitMakerFill(s, tx); err != nil {
		t.Fatal(err)
	}
	s.ShortFunding, s.Short.TxID = contract.Hex(tx), tx.TxHash().String()
	if err := e.prepare(s, s.Short); err != nil {
		t.Fatal(err)
	}
	if !e.s.FillRecords[s.ID].Allocation.EverCommitted || s.ShortFunding == "" || len(s.SelfRefunds) == 0 {
		t.Fatal("funding authority was not persisted")
	}
	return s
}

func TestAutomationFixedRenewalAtomicReceiptRestartAndNoBurst(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	if p.CurrentOfferID == "" || p.Pending != nil || len(p.Charges) != 1 || p.Charges[p.CurrentOfferID].State != "reserved" {
		t.Fatal("no authorized offer", p.Decision, p.Pending)
	}
	old := expirePolicyOffer(t, e, p)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.runAutomations(context.Background()) }()
	}
	wg.Wait()
	if len(e.s.Offers) != 2 || p.CurrentOfferID == old || p.Charges[old].Successor != p.CurrentOfferID || p.Charges[old].State != "released" {
		t.Fatal("not exactly one successor", p.Decision, p.Charges)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	p = saved.Automations[p.Config.ID]
	if saved.TradeReceipts[p.CurrentOfferID].Result.State != "accepted" || len(saved.CoinReservations) != 1 || p.Pending != nil || p.NextAction <= time.Now().Unix() {
		t.Fatal("handoff not atomic", p)
	}
	e.s = saved
	e.tradeQuotes = nil
	e.tradeConfirming = nil
	for i := 0; i < 5; i++ {
		e.runAutomations(context.Background())
	}
	if len(e.s.Offers) != 2 {
		t.Fatal("restart produced a burst")
	}
	if u := e.automationUsage(p, ""); u.ReservedVolume != 100000 || u.CommittedVolume != 0 || u.ReservedBTCFees != 20000 || u.ReservedBlakeFees != 22000 {
		t.Fatal("wrong per-chain authorization", u)
	}
}
func TestAutomationBudgetDoesNotRefundSignedFundingOrUnknownObligations(t *testing.T) {
	for _, mode := range []string{"signed", "missing-reserved"} {
		t.Run(mode, func(t *testing.T) {
			e, p := automationFixture(t)
			e.runAutomations(context.Background())
			id := p.CurrentOfferID
			if mode == "signed" {
				s := fundPolicyOffer(t, e, id)
				// Cached outcome text cannot refund may-signed authority.
				s.Stage = "refunded"
			} else {
				from, message := automationOrderRequest(t, e, e.s.Offers[id])
				if err := e.handle(from, message); err != nil {
					t.Fatal(err)
				}
				if e.s.Swaps[message.SwapID] == nil {
					t.Fatal("unknown peer funding lost accepted custody")
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			want := "reserved"
			if mode == "signed" {
				want = "committed"
			}
			if p.Charges[id].State != want {
				t.Fatal("refunded uncertain/signed charge", p.Charges[id])
			}
			p.Config.VolumeLimit = 100000
			if err := e.automationBudget(p, automationCharge(p.Config, "new", 200000, 2000), id); mode == "signed" && err == nil {
				t.Fatal("historical recreation reused committed volume")
			}
		})
	}
}
func TestAutomationPolicyReviewAndDisablePreserveExistingTerms(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	old := e.s.Offers[p.CurrentOfferID]
	edit := AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	edit.Config.SellAmount = 120000
	v := savePolicy(t, e, edit)
	if e.s.Offers[p.CurrentOfferID].ID != old.ID || p.Config.SellAmount != 120000 || v.Revision != 2 {
		t.Fatal("policy edit changed signed offer")
	}
	edit.ExpectedRevision = p.Revision
	edit.Config.VolumeLimit = 99999
	raw, _ := json.Marshal(edit)
	if _, err := e.reviewAutomation(raw); err == nil {
		t.Fatal("policy cap erased reserved authorization")
	}
	data, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network), "expected_revision": p.Revision, "cancel_open": true})
	disabled, err := e.disableAutomation(data)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || !e.s.ParentOrders[p.CurrentOfferID].Quantities.Closed || p.Charges[p.CurrentOfferID].State != "released" {
		t.Fatal("disable did not durably cancel intention", disabled)
	}
	p.NextAction = 0
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 1 {
		t.Fatal("disabled policy created an offer")
	}
}
func TestAutomationTakeVersusRenewalAndChangedAuthorization(t *testing.T) {
	for _, mode := range []string{"take", "disable", "edit", "network", "funds"} {
		t.Run(mode, func(t *testing.T) {
			e, p := automationFixture(t)
			e.runAutomations(context.Background())
			old := e.s.Offers[p.CurrentOfferID]
			// Construct a persisted automatic replacement at the same handoff used
			// by the runner, then change the authority before its external preflight.
			request := TradeQuoteRequest{FillOrderFields: automationWholeFields(chain.Blake, 100000, 200000, 2000, 0), Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Sell: chain.Blake, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 120, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}, OrderActionFields: OrderActionFields{OrderAction: "replace", SourceOfferID: p.CurrentOfferID, SourceEventID: old.ID.Hex()}}
			q := requestQuote(t, e, request)
			confirm := confirmation(q)
			p.Pending = &confirm
			r := &TradeReceipt{Digest: protocol.Digest(confirm), Snapshot: e.tradeQuotes[q.Token], Result: ConfirmTradeResult{ID: confirm.RequestID, Kind: "maker", State: "pending"}, AutomationID: p.Config.ID, AutomationRevision: p.Revision}
			e.s.TradeReceipts[confirm.RequestID] = r
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "take":
				from, message := automationOrderRequest(t, e, old)
				if err := e.handle(from, message); err != nil {
					t.Fatal(err)
				}
			case "disable":
				raw, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network), "expected_revision": p.Revision})
				if _, err := e.disableAutomation(raw); err != nil {
					t.Fatal(err)
				}
			case "edit":
				edit := AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
				edit.Config.SellAmount = 150000
				savePolicy(t, e, edit)
			case "network":
				e.Config.Network = chain.Mainnet
			case "funds":
				e.s.CoinReservations["competing"] = e.s.CoinReservations["offer/"+p.CurrentOfferID]
			}
			raw, _ := json.Marshal(confirm)
			result, err := e.confirmTrade(context.Background(), raw)
			if err == nil && result.State != "rejected" {
				t.Fatal("changed authorization accepted", result)
			}
			if len(e.s.Offers) != 1 || len(p.Charges) != 1 {
				t.Fatal("stale authorization created successor")
			}
			if (mode == "disable" || mode == "edit") && (p.Pending != nil || r.Digest != protocol.Digest(confirm) || r.Result.State != "rejected") {
				t.Fatal("policy edit retained stale grant or erased receipt identity")
			}
		})
	}
}
func TestAutomationExactReferenceRejectsSparseStaleAndConflictingQuotes(t *testing.T) {
	e, p := automationFixture(t)
	p.Config.Reference = "orderbook"
	p.Config.ReferenceFreshness = 120
	p.Config.ReferenceSpreadBPS = 100
	keys := []nostr.SecretKey{nostr.Generate(), nostr.Generate(), nostr.Generate()}
	for _, key := range keys {
		p.Config.ReferenceMakers = append(p.Config.ReferenceMakers, key.Public().Hex())
	}
	now := time.Now().Unix()
	e.marketAllRelays = true
	e.marketObservedAt = now
	post := func(key nostr.SecretKey, sell, buy int64, at int64) {
		o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: sell, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: sell, Max: sell}, ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: chain.Blake, SellAmount: sell, BuyAmount: buy, Expires: now + 3600, Status: "open"}
		content, _ := o.PublicJSON()
		event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Timestamp(at), Tags: nostr.Tags{{"d", o.ID}, {"t", e.Config.Network.Namespace()}}, Content: string(content)}
		if err := transport.Sign(&event, key); err != nil {
			t.Fatal(err)
		}
		e.s.Book[o.Maker+":"+o.ID] = event
	}
	for i, key := range keys {
		post(key, 100000+int64(i), 200000, now)
	}
	post(e.identity, 10000000000, 100000, now) // Own outlier must not enter the quorum.
	rate, events, err := e.automationPrice(p.Config, now)
	if err != nil || len(events) != 3 || rate.Numerator != 100001 {
		t.Fatal("bad exact reference", rate, events, err)
	}
	if _, err = automationAmounts(p.Config, rate); err != nil {
		t.Fatal(err)
	}
	e.marketObservedAt = now - 121
	if _, _, err = e.automationPrice(p.Config, now); err == nil {
		t.Fatal("stale relay view accepted")
	}
	e.marketObservedAt = now
	p.Config.ReferenceMakers = append(p.Config.ReferenceMakers, nostr.Generate().Public().Hex())
	if _, _, err = e.automationPrice(p.Config, now); err == nil {
		t.Fatal("sparse quorum accepted")
	}
	p.Config.ReferenceMakers = p.Config.ReferenceMakers[:3]
	post(keys[0], 200000, 200000, now+1)
	if _, _, err = e.automationPrice(p.Config, now+1); err == nil {
		t.Fatal("conflicting reference accepted")
	}
	c := p.Config
	c.Sell = chain.BTC
	c.SellAmount = 100001
	c.MinRate = AutomationRate{1, 2}
	c.MaxRate = AutomationRate{1, 2}
	if _, err = automationAmounts(c, AutomationRate{1, 2}); err == nil {
		t.Fatal("rounded offer escaped exact maximum bound")
	}
}
func TestAutomationUnavailableFeesProviderAndFundsPause(t *testing.T) {
	for _, mode := range []string{"estimate", "provider", "budget", "relay"} {
		t.Run(mode, func(t *testing.T) {
			e, p := automationFixture(t)
			switch mode {
			case "estimate":
				p.Config.FundingFee = 0
			case "provider":
				p.Config.TowerBPS = 50
				p.Config.TowerPubKey = strings.Repeat("ab", 32)
			case "budget":
				p.Config.BTCFeeBudget = 1
			case "relay":
				e.Config.Relays = nil
			}
			e.runAutomations(context.Background())
			if len(e.s.Offers) != 0 || p.Decision == "" || p.NextAction <= time.Now().Unix() {
				t.Fatal("unavailable policy did not pause", mode, p.Decision)
			}
		})
	}
}

// A restarted process acquires the saved vault into a new Engine. In particular
// it cannot retain a closed old process's disposable validation index.
func reopenAutomationFixture(t *testing.T, source *Engine, path string) *Engine {
	t.Helper()
	v, saved, err := openCurrentStateVault(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	e := &Engine{Config: source.Config, s: saved, vault: v, keys: source.keys, identity: source.identity,
		nodes: maps.Clone(source.nodes), watch: maps.Clone(source.watch), addresses: maps.Clone(source.addresses), scripts: maps.Clone(source.scripts),
		heights: maps.Clone(source.heights), clocks: maps.Clone(source.clocks), balances: map[chain.ID]int64{},
		chainFresh: maps.Clone(source.chainFresh), chainObserved: maps.Clone(source.chainObserved), chainGeneration: maps.Clone(source.chainGeneration), chainErrors: map[chain.ID]string{},
		fillValidation: captureFillValidation(&saved, nil)}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if err := e.loadReceiveAddresses(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAutomationPendingCommitFaultPreservesOneIdentityAndCharge(t *testing.T) {
	e, p := automationFixture(t)
	request := TradeQuoteRequest{FillOrderFields: automationWholeFields(chain.Blake, 100000, 200000, 2000, 0), Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Sell: chain.Blake, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 120, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}}
	q := requestQuote(t, e, request)
	confirm := confirmation(q)
	p.Pending = &confirm
	if e.s.TradeReceipts == nil {
		e.s.TradeReceipts = map[string]*TradeReceipt{}
	}
	e.s.TradeReceipts[confirm.RequestID] = &TradeReceipt{Digest: protocol.Digest(confirm), Snapshot: e.tradeQuotes[q.Token], Result: ConfirmTradeResult{ID: confirm.RequestID, Kind: "maker", State: "pending"}, AutomationID: p.Config.ID, AutomationRevision: p.Revision}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "automation-restart.db")
	if err := e.vault.Backup(path); err != nil {
		t.Fatal(err)
	}
	e.vault.Close()
	e = reopenAutomationFixture(t, e, path)
	p = e.s.Automations[p.Config.ID]
	backend := e.nodes[chain.Blake]
	e.nodes[chain.Blake] = &replacementCrashBackend{Backend: backend, before: func() {
		if err := e.vault.Close(); err != nil {
			t.Fatal(err)
		}
	}}
	raw, _ := json.Marshal(confirm)
	if _, err := e.confirmTrade(context.Background(), raw); err == nil || e.fatal == nil {
		t.Fatal("failed commit did not halt execution", err)
	}
	if len(e.s.Offers) != 1 || len(p.Charges) != 1 {
		t.Fatal("fault missed atomic creation boundary")
	}
	e.nodes[chain.Blake] = backend
	e = reopenAutomationFixture(t, e, path)
	saved := e.s
	old := saved.Automations[p.Config.ID]
	if len(saved.Offers) != 0 || len(old.Charges) != 0 || old.Pending.RequestID != confirm.RequestID || saved.TradeReceipts[confirm.RequestID].Result.State != "pending" {
		t.Fatal("partial policy charge or successor persisted")
	}

	e.s.Automations[p.Config.ID].NextAction = 0
	e.runAutomations(context.Background())
	current := e.s.Automations[p.Config.ID]
	if len(e.s.Offers) != 1 || len(current.Charges) != 1 || current.CurrentOfferID != confirm.RequestID || current.Pending != nil {
		t.Fatal("restart lost stable authorization", current)
	}
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 1 {
		t.Fatal("duplicate successor after retry")
	}
}

func TestAutomationLastFillBudgetAndRestoredHold(t *testing.T) {
	e, p := automationFixture(t)
	p.Config.VolumeLimit = 100000
	p.Config.BTCFeeBudget = 20000
	p.Config.BlakeFeeBudget = 22000
	e.runAutomations(context.Background())
	if p.CurrentOfferID == "" {
		t.Fatal("exact remaining budget did not fit selected fee", p.Decision)
	}
	id := p.CurrentOfferID
	s := fundPolicyOffer(t, e, id)
	// Advisory terminal text does not erase permanently signed spending.
	s.Stage = "refunded"
	e.reconcileAutomations()
	p.NextAction = 0
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 1 || !strings.Contains(p.Decision, "exhausted") {
		t.Fatal("last fill did not exhaust gross cap", p.Decision)
	}
	p.RestoreHold = true
	p.NextAction = 0
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 1 {
		t.Fatal("restored hold executed")
	}
	raw, _ := json.Marshal(AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true})
	if _, err := e.reviewAutomation(raw); err == nil {
		t.Fatal("restored policy resumed without explicit spending acknowledgement")
	}
	if p.Charges[id].State != "committed" {
		t.Fatal("hold erased recorded spending")
	}
}

func TestAutomationRepriceWaitsForPublicationAndPreservesTombstone(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	old := p.CurrentOfferID
	e.Config.Relays = []string{discoveryPublicationRelay(t)}
	if err := e.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Config.Reference = "orderbook"
	p.Config.ReferenceFreshness = 120
	p.Config.ReferenceSpreadBPS = 100
	p.Revision++
	keys := []nostr.SecretKey{nostr.Generate(), nostr.Generate(), nostr.Generate()}
	for _, key := range keys {
		p.Config.ReferenceMakers = append(p.Config.ReferenceMakers, key.Public().Hex())
	}
	post := func(buy int64) {
		for _, key := range keys {
			o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 100000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 100000, Max: 100000}, ID: transport.RandomID(), Network: e.Config.Network, Maker: key.Public().Hex(), Sell: chain.Blake, SellAmount: 100000, BuyAmount: buy, Expires: time.Now().Unix() + 3600, Status: "open"}
			content, _ := o.PublicJSON()
			event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", e.Config.Network.Namespace()}}, Content: string(content)}
			if err := transport.Sign(&event, key); err != nil {
				t.Fatal(err)
			}
			e.s.Book[o.Maker+":"+o.ID] = event
		}
	}
	post(250000)
	e.marketAllRelays = true
	e.marketObservedAt = time.Now().Unix()
	p.NextAction = 0
	e.runAutomations(context.Background())
	next := p.CurrentOfferID
	if next == old || !e.s.ParentOrders[old].Quantities.Closed || e.s.OrderRecords[next].Replaces != old || p.Charges[old].Successor != next || len(e.s.CoinReservations) != 1 {
		t.Fatal("reprice bypassed atomic replacement", p.Decision)
	}
	// A failed relay write cannot be mistaken for successful cancellation/new
	// publication, and further price changes cannot produce an unpublished chain.
	e.Config.Relays = []string{"ws://127.0.0.1:1"}
	if err := e.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.s.OrderRecords[next].Publication != "local_committed" {
		t.Fatal("failed relay acknowledged")
	}
	for key, event := range e.s.Book {
		if event.PubKey != e.identity.Public() {
			delete(e.s.Book, key)
		}
	}
	post(300000)
	p.NextAction = 0
	e.runAutomations(context.Background())
	if p.CurrentOfferID != next || len(e.s.Offers) != 2 || !strings.Contains(p.Decision, "publication") {
		t.Fatal("unacknowledged reprice churned", p.Decision)
	}
	e.Config.Relays = []string{discoveryPublicationRelay(t)}
	for _, d := range e.s.Outbox {
		d.LastAttempt = 0
	}
	if err := e.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.s.Offers) != 2 || e.s.OrderRecords[old].Publication != "relay_acknowledged" || e.s.OrderRecords[next].Publication != "relay_acknowledged" {
		t.Fatal("duplicate acknowledgements changed authority")
	}
}
