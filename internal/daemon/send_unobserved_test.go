package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

func TestRestoredSendUnobservedBytesRetryExactlyWithoutClearingHolds(t *testing.T) {
	e, b, request := sendFixture(t)
	broadcasts := 0
	b.broadcast = func(string) (string, error) {
		broadcasts++
		return "", errors.New("initial submission not acknowledged")
	}
	raw, _ := json.Marshal(request)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	markRestored(t, e)
	send := e.s.Sends[request.ID]
	exact, fee, feeCap := send.Raw, send.Fee, send.MaxFee
	e.s.Capacity.Invalidated = map[string]bool{"send/" + send.ID: true}
	b.transaction = func(_ context.Context, txid string) (chain.Transaction, error) {
		return chain.Transaction{TxID: txid, Hex: exact}, chain.ErrTransactionUnobserved
	}
	send.LastAttempt = time.Now().Unix()
	e.advanceSends(context.Background())
	if broadcasts != 1 || send.Submitted || send.History[0].Submitted || send.Confirmations != 0 || send.State != "unknown" {
		t.Fatal("unknown index membership established submission or bypassed retry interval", broadcasts, send.public())
	}
	assertHeld := func() {
		t.Helper()
		e.reconcileArchiveHolds(nil, nil)
		e.reconcileRecovery(map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}, nil)
		if e.recoverySends[send.ID] || e.archiveSends[send.ID] || !e.s.Capacity.Invalidated["send/"+send.ID] || e.CanChangeNetwork() == nil || send.Confirmations != 0 {
			t.Fatal("unknown observation or retry cleared monitoring/recovery")
		}
	}
	assertHeld()
	// Lack of fresh wallet observation still blocks this caller even when the
	// original exact retry is eligible. No fee or signing authority is renewed.
	send.LastAttempt = time.Now().Unix() - 31
	e.chainFresh[send.Chain] = false
	e.advanceSends(context.Background())
	if broadcasts != 1 {
		t.Fatal("offline chain published")
	}
	e.chainFresh[send.Chain] = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	b.broadcast = func(raw string) (string, error) {
		broadcasts++
		var saved State
		if _, err := e.vault.Load(&saved); err != nil {
			t.Fatal(err)
		}
		prior := saved.Sends[request.ID]
		if raw != exact || prior.Raw != exact || prior.Fee != fee || prior.MaxFee != feeCap || len(prior.History) != 1 {
			t.Fatal("retry did not use the exact durable payment authority")
		}
		tx, _ := contract.Parse(raw)
		return tx.TxHash().String(), nil
	}
	e.advanceSends(context.Background())
	if broadcasts != 2 || !send.Submitted || send.Raw != exact || send.Fee != fee || send.MaxFee != feeCap {
		t.Fatal("eligible exact signed retry did not complete", broadcasts, send.public())
	}
	assertHeld()
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	send = e.s.Sends[request.ID]
	e.advanceSends(context.Background())
	if broadcasts != 2 || canChangeNetwork(saved) == nil || send.Raw != exact {
		t.Fatal("restart lost retry interval, hold or signed identity")
	}
	assertHeld()
}

func TestSendTransportAndProofFailuresStillPreventRetry(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("invalid merkle proof"), errors.New("invalid transaction history entry")} {
		e, b, request := sendFixture(t)
		broadcasts := 0
		b.broadcast = func(string) (string, error) { broadcasts++; return "", errors.New("initial rejected submission") }
		raw, _ := json.Marshal(request)
		if _, err := e.sendCoins(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		send := e.s.Sends[request.ID]
		send.LastAttempt = time.Now().Unix() - 31
		b.transaction = func(context.Context, string) (chain.Transaction, error) { return chain.Transaction{}, failure }
		e.advanceSends(context.Background())
		if broadcasts != 1 || send.Submitted || send.State != "unknown" || send.Error == "" {
			t.Fatal("transport/proof failure became retry authority", failure, broadcasts)
		}
	}
}
