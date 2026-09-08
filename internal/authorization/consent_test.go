package authorization

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Authority, Action, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	s, err := New("helper-session", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	a := Action{Installation: "installation", Wallet: "alice", WalletKey: strings.Repeat("a", 64), Network: "regtest", Epoch: "engine-1", Method: "wallet.send", Digest: strings.Repeat("b", 64)}
	return s, a, &now
}

func TestConsentBindsEveryReviewedContextFieldAndIsOneUse(t *testing.T) {
	for _, field := range []string{"installation", "wallet", "key", "network", "epoch", "method", "digest"} {
		t.Run(field, func(t *testing.T) {
			s, a, _ := fixture(t)
			c, err := s.Prepare(a)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Consume(WithGrant(context.Background(), c.ID), a); !errors.Is(err, ErrChanged) {
				t.Fatal("unapproved challenge granted consent", err)
			}
			c, _ = s.Prepare(a)
			if err = s.Approve(c); err != nil {
				t.Fatal(err)
			}
			changed := a
			switch field {
			case "installation":
				changed.Installation = "another"
			case "wallet":
				changed.Wallet = "bob"
			case "key":
				changed.WalletKey = strings.Repeat("c", 64)
			case "network":
				changed.Network = "mainnet"
			case "epoch":
				changed.Epoch = "engine-2"
			case "method":
				changed.Method = "wallet.recovery"
			case "digest":
				changed.Digest = strings.Repeat("d", 64)
			}
			ctx := WithGrant(context.Background(), c.ID)
			if err = s.Consume(ctx, changed); !errors.Is(err, ErrChanged) {
				t.Fatal("changed action admitted", err)
			}
			if err = s.Consume(ctx, a); !errors.Is(err, ErrChanged) {
				t.Fatal("failed attempt left reusable grant", err)
			}
		})
	}
}

func TestConsentExpiryRevocationCancellationAndSessionReplacement(t *testing.T) {
	for _, mode := range []string{"expired", "revoked", "cancel", "closed", "session", "cancelled request", "changed reply"} {
		t.Run(mode, func(t *testing.T) {
			s, a, now := fixture(t)
			c, _ := s.Prepare(a)
			if mode == "changed reply" {
				changed := c
				changed.Action.Digest = strings.Repeat("e", 64)
				if err := s.Approve(changed); !errors.Is(err, ErrChanged) {
					t.Fatal(err)
				}
				if err := s.Consume(WithGrant(context.Background(), c.ID), a); !errors.Is(err, ErrChanged) {
					t.Fatal(err)
				}
				return
			}
			if err := s.Approve(c); err != nil {
				t.Fatal(err)
			}
			ctx := WithGrant(context.Background(), c.ID)
			switch mode {
			case "expired":
				*now = now.Add(Lifetime)
			case "revoked":
				s.Revoke()
			case "cancel":
				s.Cancel(c.ID)
			case "closed":
				s.Close()
			case "session":
				s, _ = New("new-session", nil)
			case "cancelled request":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := s.Consume(ctx, a); err == nil {
				t.Fatal("revoked request authorized")
			}
			if err := s.Approve(c); !errors.Is(err, ErrChanged) {
				t.Fatal("late reply revived consent", err)
			}
		})
	}
}

func TestConsentConcurrentConsumptionAndBoundedPending(t *testing.T) {
	s, a, now := fixture(t)
	c, _ := s.Prepare(a)
	if err := s.Approve(c); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Consume(WithGrant(context.Background(), c.ID), a) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("grant consumed more or less than once", successes.Load())
	}
	for range MaxPending {
		if _, err := s.Prepare(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Prepare(a); !errors.Is(err, ErrCapacity) {
		t.Fatal("pending requests unbounded", err)
	}
	*now = now.Add(Lifetime)
	if _, err := s.Prepare(a); err != nil {
		t.Fatal("expired requests blocked new consent", err)
	}
}
