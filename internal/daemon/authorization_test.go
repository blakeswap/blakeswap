package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/contract"
)

func consentEngine(t *testing.T, e *Engine) {
	t.Helper()
	a, err := authorization.New("isolated helper session", nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Config.Authorization = a
	e.Config.CredentialMode = "native"
	e.Config.Installation = "isolated installation"
	e.Config.Name = "alice"
}
func approveEngine(t *testing.T, e *Engine, req Request) context.Context {
	t.Helper()
	action, err := e.AuthorizationAction(req)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := e.Config.Authorization.Prepare(action)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Config.Authorization.Approve(challenge); err != nil {
		t.Fatal(err)
	}
	return authorization.WithGrant(context.Background(), challenge.ID)
}
func TestSensitiveDaemonCommandsRequirePrivateConsent(t *testing.T) {
	e, _, _ := sendFixture(t)
	consentEngine(t, e)
	for _, method := range []string{"wallet.send", "transaction.bump", "offer.create", "swap.take", "trade.confirm", "offer.cancel", "automation.save", "automation.disable", "strategy.save", "strategy.stop", "wallet.recovery", "wallet.backup", "pause"} {
		if _, err := e.Command(context.Background(), Request{Method: method, Params: json.RawMessage(`{}`)}); !errors.Is(err, authorization.ErrRequired) {
			t.Fatalf("%s bypassed consent: %v", method, err)
		}
	}
	e.Config.Authorization = nil
	if _, err := e.Command(context.Background(), Request{Method: "wallet.recovery"}); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("native mode without authority failed open", err)
	}
}
func TestNativeConsentBindsTermsAndSignedSendContinuesAfterRevocation(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "screen-lock", true: "broker-closed"}[closed], func(t *testing.T) {
			checkSignedSendContinuation(t, closed)
		})
	}
}
func checkSignedSendContinuation(t *testing.T, closed bool) {
	e, b, p := sendFixture(t)
	consentEngine(t, e)
	broadcasts := 0
	b.broadcast = func(raw string) (string, error) { broadcasts++; return "", context.DeadlineExceeded }
	raw, _ := json.Marshal(p)
	req := Request{Method: "wallet.send", Params: raw}
	ctx := approveEngine(t, e, req)
	p.Amount--
	changed, _ := json.Marshal(p)
	if _, err := e.Command(ctx, Request{Method: "wallet.send", Params: changed}); !errors.Is(err, authorization.ErrChanged) {
		t.Fatal("changed terms used approval", err)
	}
	if _, err := e.Command(ctx, req); !errors.Is(err, authorization.ErrChanged) {
		t.Fatal("mismatched attempt retained grant", err)
	}
	if _, err := e.Command(approveEngine(t, e, req), req); err != nil {
		t.Fatal(err)
	}
	saved := e.s.Sends[p.ID]
	if saved == nil || saved.Raw == "" || broadcasts != 1 {
		t.Fatal("authorized send not durably signed")
	}
	original := saved.Raw
	if closed {
		done := make(chan struct{})
		if err := e.Config.Authorization.BindLifetime(done); err != nil {
			t.Fatal(err)
		}
		close(done)
	} else {
		e.Config.Authorization.Revoke()
	}
	if _, err := e.Command(context.Background(), req); err != nil || broadcasts != 1 {
		t.Fatal("exact signed retry required new consent", err)
	}
	b.broadcast = func(raw string) (string, error) {
		if raw != original {
			t.Fatal("revoked new consent changed saved transaction")
		}
		broadcasts++
		tx, err := contract.Parse(raw)
		if err != nil {
			return "", err
		}
		return tx.TxHash().String(), nil
	}
	saved.LastAttempt = 0
	e.advanceSends(context.Background())
	if !saved.Submitted || broadcasts != 2 {
		t.Fatal("revoked consent blocked signed settlement")
	}
	if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: changed}); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("saved ID authorized changed details", err)
	}
}
func TestActionDigestRetainsExactIntegers(t *testing.T) {
	a, err := ActionDigest(json.RawMessage(`{"fee":2,"amount":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ActionDigest(json.RawMessage(`{"amount":9007199254740993,"fee":2}`))
	if err != nil || a != b {
		t.Fatal("object order changed action", err)
	}
	c, _ := ActionDigest(json.RawMessage(`{"amount":9007199254740992,"fee":2}`))
	if c == a {
		t.Fatal("authorization rounded exact money")
	}
}

func nativePolicyEngine(t *testing.T, e *Engine) {
	name := e.Config.Name
	consentEngine(t, e)
	e.Config.Name = name
}
func nativePolicyRequest(t *testing.T, e *Engine, q AutomationEdit) Request {
	t.Helper()
	raw, _ := json.Marshal(q)
	review, err := e.reviewAutomation(raw)
	if err != nil {
		t.Fatal(err)
	}
	q.ReviewDigest = review.ReviewDigest
	raw, _ = json.Marshal(q)
	return Request{Method: "automation.save", Params: raw}
}
func nativeStrategyRequest(t *testing.T, e *Engine, q StrategyEdit) Request {
	t.Helper()
	raw, _ := json.Marshal(q)
	review, err := e.reviewStrategy(raw)
	if err != nil {
		t.Fatal(err)
	}
	q.ReviewDigest = review.ReviewDigest
	raw, _ = json.Marshal(q)
	return Request{Method: "strategy.save", Params: raw}
}
func TestNativeConsentPolicyEditPreservesRevisionAndImportedHold(t *testing.T) {
	e, p := automationFixture(t)
	nativePolicyEngine(t, e)
	e.runAutomations(context.Background())
	old := p.CurrentOfferID
	if old == "" {
		t.Fatal("existing policy did not execute within its authorization")
	}
	q := AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	q.Config.SellAmount = 120000
	req := nativePolicyRequest(t, e, q)
	grant := approveEngine(t, e, req)
	changed := q
	changed.Config.VolumeLimit++
	other := nativePolicyRequest(t, e, changed)
	if _, err := e.Command(grant, other); !errors.Is(err, authorization.ErrChanged) {
		t.Fatal("changed policy used native grant", err)
	}
	if _, err := e.Command(grant, req); !errors.Is(err, authorization.ErrChanged) {
		t.Fatal("mismatched policy grant replayed", err)
	}
	if _, err := e.Command(approveEngine(t, e, req), req); err != nil {
		t.Fatal(err)
	}
	if p.Config.SellAmount != 120000 || p.Revision != q.ExpectedRevision+1 {
		t.Fatal("reviewed policy edit not applied")
	}
	stale := nativePolicyRequest(t, e, AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true})
	staleGrant := approveEngine(t, e, stale)
	raw, _ := json.Marshal(map[string]any{"id": p.Config.ID, "expected_wallet": e.Config.Name, "expected_network": e.Config.Network, "expected_revision": p.Revision, "cancel_open": false})
	disable := Request{Method: "automation.disable", Params: raw}
	if _, err := e.Command(context.Background(), disable); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("disable bypassed consent", err)
	}
	if _, err := e.Command(approveEngine(t, e, disable), disable); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Command(staleGrant, stale); err == nil {
		t.Fatal("old policy revision reenabled disabled authority")
	}
	if p.Enabled {
		t.Fatal("stale policy enabled")
	}
	if err := PrepareRecovery(&e.s, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	p = e.s.Automations[p.Config.ID]
	before, _ := json.Marshal(p.Charges)
	held := nativePolicyRequest(t, e, AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: false, AcknowledgeRestoredBudget: true})
	if _, err := e.Command(approveEngine(t, e, held), held); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(p.Charges)
	if !p.RestoreHold || !p.Charges[old].Uncertain || !bytes.Equal(before, after) {
		t.Fatal("native consent cleared imported uncertain accounting")
	}
}
func TestNativeConsentCannotResumeStrategyWithPreTripReview(t *testing.T) {
	e, p := strategyFixture(t)
	nativePolicyEngine(t, e)
	child := e.s.Automations[strategyPolicyID(p.Config.ID, chain.Blake)]
	id := transport.RandomID()
	child.Charges[id] = automationCharge(child.Config, id, 202000, 2000)
	child.Charges[id].State = "committed"
	charges, _ := json.Marshal(child.Charges)
	q := StrategyEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true}
	req := nativeStrategyRequest(t, e, q)
	grant := approveEngine(t, e, req)
	e.strategyFailure(child, true, errors.New("synthetic replacement failure"))
	e.strategyFailure(child, true, errors.New("synthetic replacement failure"))
	if !p.Tripped || p.Enabled {
		t.Fatal("fixture did not trip breaker")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Command(grant, req); err == nil {
		t.Fatal("pre-trip native permission resumed strategy")
	}
	q.ExpectedRevision = p.Revision
	fresh := nativeStrategyRequest(t, e, q)
	cancelled := approveEngine(t, e, fresh)
	e.Config.Authorization.Revoke()
	if _, err := e.Command(cancelled, fresh); !errors.Is(err, authorization.ErrChanged) {
		t.Fatal("revoked native policy grant resumed strategy", err)
	}
	if _, err := e.Command(approveEngine(t, e, fresh), fresh); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(child.Charges)
	if !p.Enabled || p.Tripped || !bytes.Equal(charges, after) {
		t.Fatal("fresh reviewed native resumption changed permanent commitments")
	}
}
func TestNativeRevocationRetainsPreviouslyBoundedAutomation(t *testing.T) {
	e, p := automationFixture(t)
	nativePolicyEngine(t, e)
	done := make(chan struct{})
	if err := e.Config.Authorization.BindLifetime(done); err != nil {
		t.Fatal(err)
	}
	close(done)
	e.runAutomations(context.Background())
	if p.CurrentOfferID == "" || len(p.Charges) != 1 {
		t.Fatal("private owner loss blocked already-reviewed bounded policy", p.Decision)
	}
	first := expirePolicyOffer(t, e, p)
	e.runAutomations(context.Background())
	if p.CurrentOfferID == first || p.Charges[first].Successor != p.CurrentOfferID {
		t.Fatal("renewal required new OS permission", p.Decision)
	}
	if use := e.automationUsage(p, ""); use.ReservedVolume != 100000 || use.CommittedVolume != 0 {
		t.Fatal("revocation changed bounded economic authority", use)
	}
	req := nativePolicyRequest(t, e, AutomationEdit{Config: p.Config, ExpectedRevision: p.Revision, Enabled: true})
	if _, err := e.Command(context.Background(), req); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("old policy authorized a new public edit", err)
	}
}
