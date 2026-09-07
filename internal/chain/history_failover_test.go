package chain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type historicalFailoverBackend struct {
	*failoverBackend
	page         AddressHistoryPage
	observation  HistoryTransaction
	stall        bool
	historyReads int
}

func (b *historicalFailoverBackend) Observe(_ context.Context, _ string, addresses []string) (Backend, error) {
	b.observed = append([]string(nil), addresses...)
	return b, nil
}
func (b *historicalFailoverBackend) AddressHistory(ctx context.Context, address, after string, limit int) (AddressHistoryPage, error) {
	b.historyReads++
	if b.stall {
		<-ctx.Done()
	}
	return b.page, nil // Exercise a late successful response after cancellation.
}
func (b *historicalFailoverBackend) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (HistoryTransaction, error) {
	b.historyReads++
	if b.stall {
		<-ctx.Done()
	}
	return b.observation, nil
}
func TestFailoverHistoryUsesAdmittedSourceWithoutHealthChurn(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		t.Run(string(id), func(t *testing.T) {
			tx := Transaction{TxID: strings.Repeat("a", 64), Confirmations: 2, Height: 10, BlockHash: "block", BlockTime: 12345}
			backend := &historicalFailoverBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}, page: AddressHistoryPage{Transactions: []Transaction{tx}, Next: "cursor", Complete: false, Source: "untrusted upstream label", Generation: 999}, observation: HistoryTransaction{Transaction: tx, PreviousBlockChanged: true}}
			p := testPool(&failoverBackend{check: errors.New("offline")}, backend)
			p.id = id
			p.entries[1].health.Kind = "rpc"
			p.entries[1].health.URL = "https://user:private@example.invalid/wallet/token"
			if _, err := p.Height(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Observe(context.Background(), "wallet", []string{"address"}); err != nil {
				t.Fatal(err)
			}
			before := p.Status()
			checks := backend.checks
			page, err := p.AddressHistory(context.Background(), "address", "", 2)
			source := historySource("rpc", p.entries[1].health.URL)
			if err != nil || len(page.Transactions) != 1 || page.Next != "cursor" || page.Complete || page.Source != source || page.Generation != before.Generation || strings.Contains(page.Source, "private") {
				t.Fatal(page, err)
			}
			observation, err := p.HistoryTransaction(context.Background(), tx.TxID, 10, "prior")
			if err != nil || observation.Transaction != tx || observation.Source != source || observation.Generation != before.Generation || !observation.PreviousBlockChanged {
				t.Fatal(observation, err)
			}
			backend.stall = true
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			page, err = p.AddressHistory(ctx, "address", "", 2)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || len(page.Transactions) != 0 || page.Complete {
				t.Fatal("late history success escaped its deadline", page, err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
			observation, err = p.HistoryTransaction(ctx, tx.TxID, 10, "prior")
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || observation.Transaction.TxID != "" || observation.PreviousBlockChanged {
				t.Fatal("late transaction proof escaped its deadline", observation, err)
			}
			if !reflect.DeepEqual(before, p.Status()) || !p.entries[1].validated || backend.checks != checks {
				t.Fatal("advisory timeout changed admission/health/generation", before, p.Status(), backend.checks, checks)
			}
			backend.stall = false
			if out, err := p.Output(context.Background(), tx.TxID, 0); err != nil || out == nil || out.Value != 123 || p.Generation() != before.Generation || backend.checks != checks {
				t.Fatal("history timeout forced healthy settlement through re-admission", out, err, p.Status())
			}
		})
	}
}
func TestFailoverHistoryNeverImportsOrSelectsEndpoints(t *testing.T) {
	backend := &historicalFailoverBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}}
	p := testPool(backend)
	if _, err := p.AddressHistory(context.Background(), "address", "", 2); err == nil || backend.checks != 0 {
		t.Fatal("history admitted an endpoint", err)
	}
	if _, err := p.HistoryTransaction(context.Background(), "tx", 1, "block"); err == nil || backend.checks != 0 {
		t.Fatal("observer admitted an endpoint", err)
	}
	if _, err := p.Height(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := p.Status()
	if _, err := p.AddressHistory(context.Background(), "address", "", 2); err == nil || len(backend.observed) != 0 {
		t.Fatal("history silently imported a watch wallet", err)
	}
	p.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	_, err := p.HistoryTransaction(ctx, "tx", 1, "block")
	cancel()
	<-p.gate
	if !errors.Is(err, context.DeadlineExceeded) || backend.historyReads != 0 || !reflect.DeepEqual(before, p.Status()) {
		t.Fatal("waiting for history gate changed pool state", err, p.Status())
	}
}
