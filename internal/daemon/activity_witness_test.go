package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/btcsuite/btcd/wire"
)

func TestActivityNativeWitnessPersistsBeforeFailedMetadata(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, route := range []string{"receipts", "observations"} {
			t.Run(role+"/"+route, func(t *testing.T) {
				e, s, _, secret := isolatedFixture(t, role)
				incoming := s.Short
				if role == "maker" {
					incoming = s.Long
				}
				key, _ := e.swapKey(incoming.Chain, s.ID)
				claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
				if err != nil {
					t.Fatal(err)
				}
				s.SelfClaim = contract.Hex(claim)
				s.SelfClaims = []string{s.SelfClaim}
				e.s.TowerJobs = map[string]*TowerJob{"watch": {Job: protocol.Job{Observe: &incoming}}}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				var metadata atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Method string
						Params []json.RawMessage
					}
					json.NewDecoder(r.Body).Decode(&request)
					var result any
					var rpcErr *chain.RPCError
					switch request.Method {
					case "listreceivedbyaddress":
						result = []map[string]any{{"address": e.addresses[incoming.Chain], "txids": []string{claim.TxHash().String()}}}
					case "getrawtransaction":
						var id string
						json.Unmarshal(request.Params[0], &id)
						if id != claim.TxHash().String() {
							rpcErr = &chain.RPCError{Code: -5, Message: "not found"}
						} else {
							result = chain.Transaction{TxID: id, Hex: contract.Hex(claim), BlockHash: "proof-fails"}
						}
					default:
						metadata.Add(1)
						var saved State
						if _, err := e.vault.Load(&saved); err != nil {
							t.Error(err)
						} else if !saved.Swaps[s.ID].SecretObserved || !saved.Swaps[s.ID].IncomingClaimSeen || saved.TowerJobs["watch"].Secret != hex.EncodeToString(secret) {
							t.Error("metadata started before immutable facts were durable")
						}
						rpcErr = &chain.RPCError{Code: -5, Message: "metadata unavailable"}
					}
					json.NewEncoder(w).Encode(map[string]any{"result": result, "error": rpcErr})
				}))
				defer server.Close()
				cookie := filepath.Join(t.TempDir(), "cookie")
				if err := os.WriteFile(cookie, []byte("test:test"), 0600); err != nil {
					t.Fatal(err)
				}
				rpc, err := chain.New(incoming.Chain, server.URL, cookie)
				if err != nil {
					t.Fatal(err)
				}
				if route == "receipts" {
					e.watch[incoming.Chain] = rpc
					e.indexActivityChain(context.Background(), incoming.Chain)
				} else {
					e.nodes[incoming.Chain] = rpc
					e.observeActivityChain(context.Background(), incoming.Chain)
				}
				if metadata.Load() == 0 || !s.SecretObserved || !s.IncomingClaimSeen || s.LongConfirmations != 0 || s.ShortConfirmations != 0 {
					t.Fatal("history did not retain facts independently of failed canonicality", metadata.Load(), s)
				}
			})
		}
	}
}

func TestActivityWitnessRejectsClosedOrChangedWalletAndStopsOnSaveFailure(t *testing.T) {
	for _, mode := range []string{"closed", "wallet", "network", "save-failure"} {
		t.Run(mode, func(t *testing.T) {
			e, s, _, secret := isolatedFixture(t, "maker")
			incoming := s.Long
			key, _ := e.swapKey(incoming.Chain, s.ID)
			claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			e.mu.Lock()
			ctx := e.activityWitnessContext(context.Background(), incoming.Chain)
			e.mu.Unlock()
			switch mode {
			case "closed":
				for id, backend := range e.nodes {
					e.nodes[id] = activityBackend{Backend: backend}
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
			case "wallet":
				e.Config.Name = "other"
			case "network":
				e.Config.Network = chain.Mainnet
			case "save-failure":
				e.vault.Close()
			}
			var metadata atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Method string }
				json.NewDecoder(r.Body).Decode(&request)
				if request.Method != "getrawtransaction" {
					metadata.Add(1)
				}
				json.NewEncoder(w).Encode(map[string]any{"result": chain.Transaction{TxID: claim.TxHash().String(), Hex: contract.Hex(claim), BlockHash: "proof"}, "error": nil})
			}))
			defer server.Close()
			cookie := filepath.Join(t.TempDir(), "cookie")
			if err := os.WriteFile(cookie, []byte("test:test"), 0600); err != nil {
				t.Fatal(err)
			}
			rpc, err := chain.New(incoming.Chain, server.URL, cookie)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rpc.HistoryTransaction(ctx, claim.TxHash().String(), 1, "prior")
			if err == nil || metadata.Load() != 0 {
				t.Fatal("invalid sink continued canonical reads", err, metadata.Load())
			}
			if mode == "save-failure" {
				if e.fatal == nil {
					t.Fatal("durability failure did not stop engine")
				}
			} else if s.SecretObserved || s.IncomingClaimSeen {
				t.Fatal("late witness mutated closed/other wallet")
			}
		})
	}
}

func TestActivityClosingReaderPersistsWitnessBeforeVaultClose(t *testing.T) {
	e, s, _, secret := isolatedFixture(t, "maker")
	incoming := s.Long
	key, _ := e.swapKey(incoming.Chain, s.ID)
	claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	e.vault.Close()
	path := filepath.Join(t.TempDir(), "history.db")
	e.vault, err = storage.Open(path, []byte("history-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	for id, backend := range e.nodes {
		e.nodes[id] = activityBackend{Backend: backend, observe: func(context.Context, string, uint32, string) (chain.HistoryTransaction, error) {
			return chain.HistoryTransaction{}, errors.New("unavailable")
		}}
	}
	decoded := make(chan struct{})
	delivered := make(chan error, 1)
	var retainedSink func(chain.SpendWitness) error
	witness := chain.SpendWitness{Outpoint: chain.OutpointKey(incoming.TxID, incoming.Vout), Tx: claim}
	e.watch[incoming.Chain] = activityBackend{history: func(ctx context.Context, _ string, _ string, _ int) (chain.AddressHistoryPage, error) {
		// The reader has decoded the public claim, but its synchronous sink has not
		// acquired the engine lock yet. Close wins and cancels this registered read.
		e.mu.Lock()
		retainedSink = e.activityWitnessSink(ctx, incoming.Chain)
		e.mu.Unlock()
		close(decoded)
		<-ctx.Done()
		delivered <- retainedSink(witness)
		return chain.AddressHistoryPage{Source: "source", Complete: true, Transactions: []chain.Transaction{{TxID: claim.TxHash().String(), Hex: contract.Hex(claim), Confirmations: 2}}}, ctx.Err()
	}}
	done := make(chan struct{})
	go func() { e.refreshActivity(context.Background()); close(done) }()
	select {
	case <-decoded:
	case <-time.After(time.Second):
		t.Fatal("reader did not decode witness")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not join reader")
	}
	if err = <-delivered; err != nil {
		t.Fatal("closing reader lost immutable witness", err)
	}
	if err = retainedSink(witness); err == nil {
		t.Fatal("drained callback wrote after vault closure")
	}
	reopened, err := storage.Open(path, []byte("history-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var saved State
	if _, err = reopened.Load(&saved); err != nil {
		t.Fatal(err)
	}
	got := saved.Swaps[s.ID]
	if !got.SecretObserved || !got.IncomingClaimSeen || got.Secret != hex.EncodeToString(secret) {
		t.Fatal("reopen lost closing reader's refund guard")
	}
	if got.LongConfirmations != 0 || got.ShortConfirmations != 0 {
		t.Fatal("closing witness granted canonical proof")
	}
	for _, record := range saved.Activities {
		if record.TxID == claim.TxHash().String() {
			t.Fatal("late canonical history accepted during close")
		}
	}
}

func TestActivityFirstWitnessDeliveryRetainsLaterInputsBeforeCancellation(t *testing.T) {
	e, s, _, secret := isolatedFixture(t, "maker")
	incoming := s.Long
	key, _ := e.swapKey(incoming.Chain, s.ID)
	claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	// Canonicality is deliberately irrelevant here: the matching public preimage
	// remains known even when this multi-input transaction cannot be confirmed.
	unrelated := wire.NewTxIn(&wire.OutPoint{Index: 3}, nil, nil)
	claim.TxIn = append([]*wire.TxIn{unrelated}, claim.TxIn...)
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	sink := e.activityWitnessSink(ctx, incoming.Chain)
	e.mu.Unlock()
	cancel() // The lease will refuse further deliveries after this one returns.
	if err = sink(chain.SpendWitness{Outpoint: chain.OutpointKey(unrelated.PreviousOutPoint.Hash.String(), 3), Tx: claim}); err != nil {
		t.Fatal(err)
	}
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	got := saved.Swaps[s.ID]
	if !got.SecretObserved || !got.IncomingClaimSeen {
		t.Fatal("later decoded input was lost before gate reacquisition")
	}
	if got.LongConfirmations != 0 || got.ShortConfirmations != 0 {
		t.Fatal("witness granted confirmation authority")
	}
}
