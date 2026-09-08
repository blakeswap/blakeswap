package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestStrategyMalformedDurableConfigurationRejectedWithoutMutation(t *testing.T) {
	e, original := strategyFixture(t)
	original.Config.Reference = "orderbook"
	original.Config.ReferenceMakers = []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)}
	original.Config.ReferenceFreshness, original.Config.ReferenceSpreadBPS = 60, 20
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		e.s.Automations[strategyPolicyID(original.Config.ID, id)].Config = original.Config.policy(id)
	}
	if err := ValidateAutomationState(&e.s); err != nil {
		t.Fatal("valid control", err)
	}
	source, _ := json.Marshal(e.s)
	cases := []struct {
		name   string
		change func(*MakerStrategy)
	}{
		{"empty-reference", func(p *MakerStrategy) { p.Config.ReferenceMakers = nil }},
		{"duplicate-reference", func(p *MakerStrategy) { p.Config.ReferenceMakers[1] = p.Config.ReferenceMakers[0] }},
		{"own-reference", func(p *MakerStrategy) { p.Config.ReferenceMakers[0] = p.WalletKey }},
		{"invalid-reference", func(p *MakerStrategy) { p.Config.Reference = "unknown" }},
		{"invalid-key", func(p *MakerStrategy) { p.Config.ReferenceMakers[0] = "invalid" }},
		{"reference-freshness", func(p *MakerStrategy) { p.Config.ReferenceFreshness = 0 }},
		{"reference-spread", func(p *MakerStrategy) { p.Config.ReferenceSpreadBPS = 0 }},
		{"rate", func(p *MakerStrategy) { p.Config.Rate.Denominator = 0 }},
		{"rate-bounds", func(p *MakerStrategy) { p.Config.MinRate = p.Config.MaxRate }},
		{"target", func(p *MakerStrategy) { p.Config.BTC.Target = 0 }},
		{"size", func(p *MakerStrategy) { p.Config.Blake.MinOffer = p.Config.Blake.MaxOffer + 1 }},
		{"cadence", func(p *MakerStrategy) { p.Config.Cadence = 0 }},
		{"lifetime", func(p *MakerStrategy) { p.Config.Lifetime = 1 }},
		{"fee-cap", func(p *MakerStrategy) { p.Config.Blake.MaxFundingFee = 0 }},
		{"budget", func(p *MakerStrategy) { p.Config.BTCFeeBudget = 0 }},
		{"provider", func(p *MakerStrategy) { p.Config.TowerBPS = 1 }},
		{"breaker", func(p *MakerStrategy) { p.Config.MaxConsecutiveFailures = 0 }},
		{"concurrency", func(p *MakerStrategy) { p.Config.MaxConcurrent = 0 }},
		{"spread", func(p *MakerStrategy) { p.Config.MinSpreadBPS = p.Config.SpreadBPS + 1 }},
		{"revision", func(p *MakerStrategy) { p.Revision = 0 }},
		{"network", func(p *MakerStrategy) { p.Config.Network = chain.Mainnet }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s State
			if err := json.Unmarshal(source, &s); err != nil {
				t.Fatal(err)
			}
			p := s.MakerStrategies[original.Config.ID]
			tc.change(p)
			// Matching child configurations must not make malformed parent terms valid.
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				s.Automations[strategyPolicyID(p.Config.ID, id)].Config = p.Config.policy(id)
			}
			before, _ := json.Marshal(s)
			if err := ValidateAutomationState(&s); err == nil {
				t.Fatal("malformed configuration accepted")
			}
			if err := PrepareRecovery(&s, time.Now().Unix(), false); err == nil {
				t.Fatal("malformed configuration installed for recovery")
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("rejection changed imported accounting")
			}
		})
	}
}

func TestStrategyEmptyReferenceLoadRejectsAndPreviewReturnsUnavailable(t *testing.T) {
	e, p := strategyFixture(t)
	p.Config.Reference = "orderbook"
	p.Config.ReferenceFreshness, p.Config.ReferenceSpreadBPS = 60, 20
	p.Config.ReferenceMakers = nil
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		e.s.Automations[strategyPolicyID(p.Config.ID, id)].Config = p.Config.policy(id)
	}
	// Price evaluation itself remains defensive, even before a caller validates.
	if _, _, err := e.automationPrice(p.Config.policy(chain.Blake), time.Now().Unix()); err == nil {
		t.Fatal("empty reference priced")
	}
	raw, _ := json.Marshal(map[string]string{"expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network)})
	list, err := e.listStrategies(raw)
	if err != nil || len(list.Strategies) != 1 {
		t.Fatal(list, err)
	}
	for _, q := range list.Strategies[0].Quotes {
		if q.Ready || !strings.Contains(q.Reason, "quorum") {
			t.Fatal(q)
		}
	}
	before, _ := json.Marshal(e.s)
	root := t.TempDir()
	password := []byte("isolated invalid strategy state password")
	passwordPath := filepath.Join(root, "vault.password")
	if err = os.WriteFile(passwordPath, password, 0600); err != nil {
		t.Fatal(err)
	}
	vault, err := storage.Open(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(e.s); err != nil {
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: e.Config.Name, Mode: "trader", Network: chain.Regtest, DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}, Nodes: map[chain.ID]NodeConfig{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		cfg.Nodes[id] = NodeConfig{Kind: "rpc", URL: "http://127.0.0.1:1", Cookie: filepath.Join(root, "absent-cookie")}
	}
	engine, err := Open(context.Background(), cfg)
	if engine != nil {
		engine.Close()
		t.Fatal("invalid strategy opened")
	}
	if err == nil || !strings.Contains(err.Error(), "orderbook reference") {
		t.Fatal("missing validation error", err)
	}
	vault, err = storage.Open(filepath.Join(root, "state.db"), password)
	if err != nil {
		t.Fatal("load rejection retained lock", err)
	}
	defer vault.Close()
	var saved State
	if _, err = vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(saved)
	if !bytes.Equal(before, after) {
		t.Fatal("load rejection rewrote imported state")
	}
}

func TestStrategyBreakerInvalidatesOldReviewAndFreshReviewKeepsCommitments(t *testing.T) {
	e, p := strategyFixture(t)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	chargeID := transport.RandomID()
	child.Charges[chargeID] = automationCharge(child.Config, chargeID, 202000, 2000)
	child.Charges[chargeID].State = "committed"
	child.Pending = &ConfirmTradeRequest{RequestID: "pending"}
	e.s.TradeReceipts = map[string]*TradeReceipt{
		"pending":  {Digest: "pending-identity", Result: ConfirmTradeResult{ID: "pending", State: "pending"}},
		"accepted": {Digest: "accepted-identity", Result: ConfirmTradeResult{ID: "accepted", State: "accepted"}},
	}
	chargeBefore, _ := json.Marshal(child.Charges)
	acceptedBefore, _ := json.Marshal(e.s.TradeReceipts["accepted"])
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	raw, _ := json.Marshal(q)
	review, err := e.reviewStrategy(raw)
	if err != nil {
		t.Fatal(err)
	}
	q.ReviewDigest = review.ReviewDigest
	revision, childRevision := p.Revision, child.Revision
	e.strategyFailure(child, true, errors.New("replacement failed"))
	if p.Revision != revision {
		t.Fatal("non-tripping failure changed authorization revision")
	}
	e.strategyFailure(child, true, errors.New("replacement failed"))
	if !p.Tripped || p.Enabled || p.Revision != revision+1 || child.Revision != childRevision+1 || child.Pending != nil {
		t.Fatal("trip did not revoke old authority", p)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.MakerStrategies[p.Config.ID].Revision != revision+1 || !saved.MakerStrategies[p.Config.ID].Tripped {
		t.Fatal("trip revision not durable")
	}
	raw, _ = json.Marshal(q)
	if _, err = e.saveStrategy(raw); err == nil {
		t.Fatal("pre-trip review resumed strategy")
	}
	q.ExpectedRevision = p.Revision
	q.ReviewDigest = ""
	saveStrategyTest(t, e, q)
	if !p.Enabled || p.Tripped || p.Revision != revision+2 {
		t.Fatal("fresh complete review could not resume", p)
	}
	chargeAfter, _ := json.Marshal(child.Charges)
	acceptedAfter, _ := json.Marshal(e.s.TradeReceipts["accepted"])
	if !bytes.Equal(chargeBefore, chargeAfter) || !bytes.Equal(acceptedBefore, acceptedAfter) || e.s.TradeReceipts["pending"].Digest != "pending-identity" || e.s.TradeReceipts["pending"].Result.State != "rejected" {
		t.Fatal("trip/resume changed accounting or accepted receipt identity")
	}
}

type strategyEstimatedFeeBackend struct {
	*sendBackend
	rate int64
}

func (b *strategyEstimatedFeeBackend) EstimateFee(context.Context, uint32) chain.FeeEstimate {
	rate := b.rate
	if rate == 0 {
		rate = 1000
	}
	return chain.FeeEstimate{Chain: chain.Blake, State: "available", Rate: rate, Timestamp: time.Now().Unix()}
}

func TestStrategyFreshEstimatedReceiptRejectionCountsOnceAndSurvivesRestart(t *testing.T) {
	e, p := strategyFixture(t)
	e.nodes[chain.Blake] = &strategyEstimatedFeeBackend{sendBackend: e.nodes[chain.Blake].(*sendBackend)}
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	q.Config.Blake.FundingFee = 0
	q.Config.BlakeFeeBudget, q.Config.BTCFeeBudget = 20001, 20000
	q.Config.MaxConsecutiveFailures = 1
	saveStrategyTest(t, e, q)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	e.s.Automations[strategyPolicyID(p.Config.ID, chain.BTC)].NextAction = time.Now().Unix() + 3600
	revision := p.Revision
	for i := 0; i < 3; i++ {
		child.NextAction = 0
		e.runAutomations(context.Background())
	}
	if p.Enabled || !p.Tripped || p.Revision != revision+1 || p.ConsecutiveFailures != 1 || p.ReplacementFailures != 0 || len(p.Outcomes) != 1 || !p.Outcomes[0] {
		t.Fatal("exact final rejection not counted once", p)
	}
	if len(e.s.TradeReceipts) != 1 || child.Pending != nil || len(e.s.Offers) != 0 || len(child.Charges) != 0 {
		t.Fatal("rejected quote retained authority")
	}
	var receiptID string
	for id, r := range e.s.TradeReceipts {
		receiptID = id
		if r.Result.State != "rejected" || !strings.Contains(r.Result.Error, "shared per-chain fee/rescue budget exhausted") {
			t.Fatal(r.Result)
		}
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.runAutomations(context.Background())
	p = e.s.MakerStrategies[p.Config.ID]
	if !p.Tripped || p.Enabled || p.ConsecutiveFailures != 1 || len(e.s.TradeReceipts) != 1 || e.s.TradeReceipts[receiptID].Result.State != "rejected" {
		t.Fatal("restart lost rejection/breaker", p)
	}
	// A new review with room for the actual quote resumes, retaining rejection ID.
	q.Config.BlakeFeeBudget = 30000
	q.ExpectedRevision = p.Revision
	q.ReviewDigest = ""
	saveStrategyTest(t, e, q)
	child = e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	child.NextAction = 0
	e.runAutomations(context.Background())
	if len(e.s.Offers) != 1 || p.Tripped || p.ConsecutiveFailures != 0 || len(p.Outcomes) != 1 || p.Outcomes[0] || e.s.TradeReceipts[receiptID].Result.State != "rejected" {
		t.Fatal("fresh affordable quote did not execute", p, child.Decision)
	}
}

func TestStrategyFreshEstimatedReplacementRejectionWithdrawsQuoteOnce(t *testing.T) {
	e, p := strategyFixture(t)
	backend := &strategyEstimatedFeeBackend{sendBackend: e.nodes[chain.Blake].(*sendBackend), rate: 1}
	e.nodes[chain.Blake] = backend
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	q.Config.Blake.Target = 4000000
	q.Config.Blake.FundingFee = 0
	q.Config.BlakeFeeBudget, q.Config.BTCFeeBudget = 20001, 20000
	q.Config.MaxConsecutiveFailures = 20
	q.Config.MaxReplacementFailures = 1
	saveStrategyTest(t, e, q)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	e.s.Automations[strategyPolicyID(p.Config.ID, chain.BTC)].NextAction = time.Now().Unix() + 3600
	child.NextAction = 0
	e.runAutomations(context.Background())
	id := child.CurrentOfferID
	if id == "" {
		t.Fatal("initial low fee quote", child.Decision, p.Decision)
	}
	record := e.s.OrderRecords[id]
	record.Publication = "relay_acknowledged"
	e.s.OrderRecords[id] = record
	beforeReceipt, _ := json.Marshal(e.s.TradeReceipts[id])
	// More confirmed inventory changes the quote size; the new actual fee is
	// within its cap but no longer fits the remaining shared allowance.
	backend.coins = append(backend.coins, chain.UTXO{TxID: strings.Repeat("f", 64), Amount: 500000, Script: hex.EncodeToString(e.scripts[chain.Blake]), Confirmations: 2})
	if err := e.refreshChain(context.Background(), chain.Blake); err != nil {
		t.Fatal(err)
	}
	backend.rate = 1000
	for i := 0; i < 3; i++ {
		child.NextAction = 0
		e.runAutomations(context.Background())
	}
	if !p.Tripped || p.Enabled || p.ConsecutiveFailures != 1 || p.ReplacementFailures != 1 || len(p.Outcomes) != 2 || !p.Outcomes[1] {
		t.Fatal("replacement rejection not counted once", p, child.Decision)
	}
	rejected := 0
	for _, r := range e.s.TradeReceipts {
		if r.Result.State == "rejected" && strings.Contains(r.Result.Error, "shared per-chain fee/rescue budget exhausted") {
			rejected++
			if r.Snapshot.Request.OrderAction != "replace" || r.Snapshot.Request.SourceOfferID != id {
				t.Fatal("wrong source provenance", r.Snapshot.Request)
			}
		}
	}
	if rejected != 1 || child.Pending != nil || len(e.s.Offers) != 1 {
		t.Fatal("rejection produced new or retry authority", rejected)
	}
	old, err := historicalOffer(e.s.Offers[id])
	if err != nil || old.Status != "cancelled" || child.Charges[id].State != "released" {
		t.Fatal("eligible unfunded quote remained open", old, err)
	}
	afterReceipt, _ := json.Marshal(e.s.TradeReceipts[id])
	if !bytes.Equal(beforeReceipt, afterReceipt) {
		t.Fatal("replacement failure altered accepted source receipt")
	}
}
