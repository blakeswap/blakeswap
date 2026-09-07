package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestAutomationImportedPolicyRequiresNewAuthorization(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	old := p.CurrentOfferID
	event := e.s.Offers[old]
	acceptedBefore, _ := json.Marshal(e.s.TradeReceipts[old])
	request := TradeQuoteRequest{Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Sell: chain.Blake, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 120, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}, OrderActionFields: OrderActionFields{OrderAction: "replace", SourceOfferID: old, SourceEventID: event.ID.Hex()}}
	quote := requestQuote(t, e, request)
	pending := confirmation(quote)
	p.Pending = &pending
	e.s.TradeReceipts[pending.RequestID] = &TradeReceipt{Digest: protocol.Digest(pending), Snapshot: e.tradeQuotes[quote.Token], Result: ConfirmTradeResult{ID: pending.RequestID, Kind: "maker", State: "pending"}, AutomationID: p.Config.ID, AutomationRevision: p.Revision}
	originalRevision := p.Revision
	markRestored(t, e)
	if p.Enabled || !p.RestoreHold || p.Revision <= originalRevision || p.Pending != nil || !p.Charges[old].Uncertain || p.Charges[old].State != "reserved" {
		t.Fatal("import retained authority or lost accounting", p)
	}
	if len(e.s.Offers) != 0 || len(e.s.Outbox) != 0 || e.s.Recovery.Offers[old].ID != event.ID {
		t.Fatal("import retained live offer/publication authority")
	}
	acceptedAfter, _ := json.Marshal(e.s.TradeReceipts[old])
	if !bytes.Equal(acceptedBefore, acceptedAfter) || e.s.TradeReceipts[pending.RequestID].Digest != protocol.Digest(pending) {
		t.Fatal("import changed an accepted receipt or pending identity")
	}
	raw, _ := json.Marshal(pending)
	if result, err := e.confirmTrade(context.Background(), raw); err != nil || result.State != "rejected" {
		t.Fatal("old automatic grant resumed", result, err)
	}
	p.NextAction = 0
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 0 {
		t.Fatal("held scheduler resumed")
	}
	edit := AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true, AcknowledgeRestoredBudget: true}
	raw, _ = json.Marshal(edit)
	if _, err := e.reviewAutomation(raw); err == nil {
		t.Fatal("enabled before current chain recovery")
	}
	readyManagedRecovery(t, e)
	if p.Enabled || !p.RestoreHold || !p.Charges[old].Uncertain {
		t.Fatal("chain recovery erased policy consent/accounting")
	}
	from, message := orderRequest(t, e, event)
	if err := e.handle(from, message); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Swaps) != 0 {
		t.Fatal("quarantined order accepted new swap")
	}
	e.Config.Name = "recovered-profile"
	view := e.automationView(p)
	if view.Config.Wallet != e.Config.Name || p.Config.Wallet == e.Config.Name {
		t.Fatal("import did not expose safe reviewed profile rebinding")
	}
	edit = AutomationEdit{Config: view.Config, ExpectedRevision: p.Revision, Enabled: false, AcknowledgeRestoredBudget: true}
	savePolicy(t, e, edit)
	if !p.RestoreHold || p.Charges[old].State != "reserved" || !p.Charges[old].Uncertain {
		t.Fatal("disabled save erased hold/uncertain charge")
	}
	edit.ExpectedRevision = p.Revision
	edit.Enabled = true
	edit.AcknowledgeRestoredBudget = false
	raw, _ = json.Marshal(edit)
	if _, err := e.reviewAutomation(raw); err == nil {
		t.Fatal("resumed without a new acknowledgement")
	}
	edit.AcknowledgeRestoredBudget = true
	savePolicy(t, e, edit)
	p.NextAction = 0
	e.runAutomations(context.Background())
	next := p.CurrentOfferID
	if next == old || next == pending.RequestID || len(e.s.Offers) != 1 || e.s.Offers[old].ID == event.ID {
		t.Fatal("fresh authorization reused old authority", p.Decision)
	}
	if p.Charges[old].State != "reserved" || !p.Charges[old].Uncertain || p.Charges[old].Successor != "" || e.automationUsage(p, "").ReservedVolume != 200000 {
		t.Fatal("fresh action transferred unknown historical budget")
	}
	acceptedAfter, _ = json.Marshal(e.s.TradeReceipts[old])
	if !bytes.Equal(acceptedBefore, acceptedAfter) {
		t.Fatal("fresh authorization changed accepted receipt")
	}
	markRestored(t, e)
	if p.Enabled || !p.RestoreHold || !p.Charges[next].Uncertain || len(p.Charges) != 2 || p.Pending != nil {
		t.Fatal("reimport reset budget or authority")
	}
}

func TestAutomationDisabledAcknowledgementRetainsImportedBudget(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	id := expirePolicyOffer(t, e, p)
	p.Charges[id].State = "reserved" // Snapshot was taken before expiry was known.
	holdImportedAutomations(&e.s)
	savePolicy(t, e, AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: false, AcknowledgeRestoredBudget: true})
	if !p.RestoreHold || p.Charges[id].State != "reserved" || !p.Charges[id].Uncertain {
		t.Fatal("disabled save erased imported reservation")
	}
	savePolicy(t, e, AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true, AcknowledgeRestoredBudget: true})
	if p.RestoreHold || p.Charges[id].State != "reserved" || !p.Charges[id].Uncertain {
		t.Fatal("explicit future authorization refunded unknown prior outcome")
	}
	p.Config.VolumeLimit = p.Config.SellAmount
	if err := e.automationBudget(p, automationCharge(p.Config, "next", 200000, 2000), id); err == nil {
		t.Fatal("replacement excluded imported uncertainty from cap")
	}
}

func TestAutomationHeldOfferCannotAcceptNewRequest(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	event := e.s.Offers[p.CurrentOfferID]
	p.RestoreHold = true
	p.Enabled = false
	from, message := orderRequest(t, e, event)
	if err := e.handle(from, message); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Swaps) != 0 {
		t.Fatal("held policy's old signed offer accepted a new swap")
	}
}

func TestAutomationBackupPreservesAuthorizationAndUncertainty(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	id := p.CurrentOfferID
	p.Charges[id].Successor = "recorded-successor"
	holdImportedAutomations(&e.s)
	snapshot, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	before, err := BackupFingerprint(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	saved := snapshot.Automations[p.Config.ID]
	if !saved.RestoreHold || saved.Enabled || !saved.Charges[id].Uncertain || saved.Charges[id].Successor != "recorded-successor" {
		t.Fatal("backup omitted authorization facts")
	}
	saved.Charges[id].Uncertain = false
	changed, err := BackupFingerprint(snapshot)
	if err != nil || changed == before {
		t.Fatal("backup fingerprint ignored charge uncertainty", err)
	}
	if !p.Charges[id].Uncertain {
		t.Fatal("snapshot mutation changed live authorization")
	}
	saved.Charges[id].Uncertain = true
	saved.Charges[id].Successor = "different-successor"
	changed, err = BackupFingerprint(snapshot)
	if err != nil || changed == before {
		t.Fatal("backup fingerprint ignored successor identity", err)
	}
}

func TestAutomationBackupFingerprintIgnoresPollingRetainsAuthority(t *testing.T) {
	e, p := automationFixture(t)
	e.runAutomations(context.Background())
	id := p.CurrentOfferID
	if id == "" {
		t.Fatal(p.Decision)
	}
	p.NextAction = 0
	before, err := BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.RecordBackup(before, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	e.runAutomations(context.Background())
	if p.CurrentOfferID != id || p.Pending != nil || len(p.Charges) != 1 {
		t.Fatal("no-op probe created authority")
	}
	fresh, err := StateBackupFreshness(e.s)
	if err != nil || fresh.StateChanged {
		t.Fatal("no-op policy tick marked backup stale", fresh, err)
	}
	p.NextAction++
	p.Decision = "another observation"
	p.ReferenceObserved++
	p.ReferenceEvents = []string{"polled-reference"}
	after, err := BackupFingerprint(e.s)
	if err != nil || after != before {
		t.Fatal("polling fields changed fingerprint", err)
	}
	snapshot, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	saved := snapshot.Automations[p.Config.ID]
	if saved.NextAction != p.NextAction || saved.Decision != p.Decision || saved.ReferenceObserved != p.ReferenceObserved || len(saved.ReferenceEvents) != 1 {
		t.Fatal("full archive dropped polling state")
	}
	raw, _ := json.Marshal(snapshot)
	cases := map[string]func(*State, *AutomationPolicy){
		"config":        func(s *State, p *AutomationPolicy) { p.Config.Cadence++ },
		"revision":      func(s *State, p *AutomationPolicy) { p.Revision++ },
		"enabled":       func(s *State, p *AutomationPolicy) { p.Enabled = false },
		"hold":          func(s *State, p *AutomationPolicy) { p.RestoreHold = true },
		"identity":      func(s *State, p *AutomationPolicy) { p.WalletKey = "changed" },
		"pending":       func(s *State, p *AutomationPolicy) { p.Pending = &ConfirmTradeRequest{RequestID: "new-grant"} },
		"charge":        func(s *State, p *AutomationPolicy) { p.Charges[id].Volume++ },
		"uncertainty":   func(s *State, p *AutomationPolicy) { p.Charges[id].Uncertain = true },
		"successor":     func(s *State, p *AutomationPolicy) { p.Charges[id].Successor = "next" },
		"current-offer": func(s *State, p *AutomationPolicy) { p.CurrentOfferID = "next" },
		"action-time":   func(s *State, p *AutomationPolicy) { p.LastAction++ },
		"receipt":       func(s *State, p *AutomationPolicy) { s.TradeReceipts[id].Digest = "changed" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var state State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			change(&state, state.Automations[p.Config.ID])
			fingerprint, err := BackupFingerprint(state)
			if err != nil || fingerprint == before {
				t.Fatal("meaningful authority omitted from fingerprint", err)
			}
		})
	}
}
