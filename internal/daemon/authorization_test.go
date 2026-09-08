package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
