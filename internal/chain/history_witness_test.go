package chain

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoryWitnessPrecedesMetadataAndStopsOnSinkFailure(t *testing.T) {
	for _, transport := range []string{"rpc", "electrum"} {
		for _, route := range []string{"receipts", "observations"} {
			for _, fail := range []bool{false, true} {
				name := transport + "/" + route
				if fail {
					name += "/sink-failure"
				}
				t.Run(name, func(t *testing.T) {
					address, record := historyReceipt(t, 1)
					var delivered, metadata atomic.Int32
					var backend interface {
						AddressHistorian
						HistoryObserver
					}
					if transport == "rpc" {
						backend = fakeRPC(t, func(method string, params []json.RawMessage) (any, *RPCError) {
							switch method {
							case "listreceivedbyaddress":
								return []map[string]any{{"address": address, "txids": []string{record.TxID}}}, nil
							case "getrawtransaction":
								return record, nil
							default:
								metadata.Add(1)
								if delivered.Load() != 1 {
									t.Error("metadata read before durable witness", method)
								}
								return nil, &RPCError{Code: -5, Message: "metadata unavailable"}
							}
						})
					} else {
						rawReturned := false
						e := electrumFixture(t, Regtest, BTC, func(method string, params []json.RawMessage) any {
							switch method {
							case "blockchain.transaction.get":
								rawReturned = true
								return record.Hex
							case "blockchain.scripthash.get_history":
								if !rawReturned {
									return []map[string]any{{"tx_hash": record.TxID, "height": 1}}
								}
							}
							metadata.Add(1)
							if delivered.Load() != 1 {
								t.Error("metadata read before durable witness", method)
							}
							return nil
						})
						e.endpoint, _ = url.Parse("tcp://history.example.invalid:50001")
						backend = e
					}
					ctx := WithSpendWitnessSink(context.Background(), func(w SpendWitness) error {
						if w.Tx.TxHash().String() != record.TxID {
							t.Fatal("wrong witness")
						}
						delivered.Add(1)
						if fail {
							return errors.New("vault unavailable")
						}
						return nil
					})
					var err error
					if route == "receipts" {
						_, err = backend.AddressHistory(ctx, address, "", 2)
					} else {
						_, err = backend.HistoryTransaction(ctx, record.TxID, 1, "prior")
					}
					if err == nil || delivered.Load() != 1 {
						t.Fatal("history lost witness before failed proof", delivered.Load(), err)
					}
					if fail && metadata.Load() != 0 {
						t.Fatal("sink failure allowed further network I/O", metadata.Load())
					}
				})
			}
		}
	}
}

type witnessedHistoryBackend struct {
	*historicalFailoverBackend
	record Transaction
	before func()
	after  int
}

func (b *witnessedHistoryBackend) HistoryTransaction(ctx context.Context, id string, _ uint32, _ string) (HistoryTransaction, error) {
	if b.before != nil {
		b.before()
	}
	if err := historyWitness(ctx, id, b.record); err != nil {
		return HistoryTransaction{}, err
	}
	b.after++
	return HistoryTransaction{Transaction: b.record}, nil
}
func TestFailoverHistoryWitnessReleasesGateForProtocolLockOrder(t *testing.T) {
	_, record := historyReceipt(t, 1)
	entered, proceed := make(chan struct{}), make(chan struct{})
	b := &witnessedHistoryBackend{historicalFailoverBackend: &historicalFailoverBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}}, record: record, before: func() { close(entered); <-proceed }}
	p := testPool(b)
	if _, err := p.Height(context.Background()); err != nil {
		t.Fatal(err)
	}
	var engineMu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	delivered := false
	witnessCtx := WithSpendWitnessSink(ctx, func(SpendWitness) error { engineMu.Lock(); defer engineMu.Unlock(); delivered = true; return nil })
	done := make(chan error, 1)
	go func() { _, err := p.HistoryTransaction(witnessCtx, record.TxID, 0, ""); done <- err }()
	<-entered
	engineMu.Lock()
	protocol := make(chan error, 1)
	go func() { _, err := p.Output(ctx, record.TxID, 0); engineMu.Unlock(); protocol <- err }()
	close(proceed)
	select {
	case err := <-protocol:
		if err != nil {
			t.Fatal("protocol blocked behind history sink", err)
		}
	case <-ctx.Done():
		t.Fatal("Engine.mu/pool.gate inversion")
	}
	select {
	case err := <-done:
		if err != nil || !delivered || b.after != 1 {
			t.Fatal(err, delivered, b.after)
		}
	case <-ctx.Done():
		t.Fatal("history did not resume")
	}
}
func TestFailoverHistoryWitnessReacquisitionCancellationAndSourceChange(t *testing.T) {
	for _, mode := range []string{"cancel", "cancel-wait", "source", "sink-failure"} {
		t.Run(mode, func(t *testing.T) {
			_, record := historyReceipt(t, 1)
			b := &witnessedHistoryBackend{historicalFailoverBackend: &historicalFailoverBackend{failoverBackend: &failoverBackend{height: 100, hash: "tip"}}, record: record}
			p := testPool(b, &failoverBackend{height: 100, hash: "tip"})
			if _, err := p.Height(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := p.Status()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			gateOwned := false
			delivered := 0
			ctx = WithSpendWitnessSink(ctx, func(SpendWitness) error {
				delivered++
				switch mode {
				case "cancel":
					cancel()
				case "cancel-wait":
					p.gate <- struct{}{}
					gateOwned = true
					cancel()
				case "source":
					p.gate <- struct{}{}
					p.mu.Lock()
					p.active = 1
					p.generation++
					p.entries[1].validated = true
					p.mu.Unlock()
					<-p.gate
				case "sink-failure":
					return errors.New("vault unavailable")
				}
				return nil
			})
			done := make(chan error, 1)
			go func() { _, err := p.HistoryTransaction(ctx, record.TxID, 0, ""); done <- err }()
			select {
			case err := <-done:
				if err == nil || delivered != 1 || b.after != 0 {
					t.Fatal("continued after lost lease/sink failure", err, delivered, b.after)
				}
			case <-time.After(time.Second):
				t.Fatal("double release or blocked reacquisition")
			}
			if gateOwned {
				select {
				case <-p.gate:
				default:
					t.Fatal("history released another operation's lease")
				}
			}
			if mode != "source" && (p.Generation() != before.Generation || !p.entries[0].validated || p.Status().Endpoints[0].Error != "") {
				t.Fatal("history callback changed endpoint health", p.Status())
			}
			select {
			case p.gate <- struct{}{}:
				<-p.gate
			default:
				t.Fatal("history leaked gate")
			}
		})
	}
}
