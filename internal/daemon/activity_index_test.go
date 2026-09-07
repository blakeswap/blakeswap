package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func TestActivityHistoryCapacityDoesNotAdvanceOrPartiallyRecordTransaction(t *testing.T) {
	e, _, _ := sendFixture(t)
	e.s.Activities = map[string]Activity{}
	for i := 0; i < maxActivityRecords-1; i++ {
		id := "existing/" + strconv.Itoa(i)
		e.s.Activities[id] = Activity{ID: id}
	}
	entry := e.receiveBook[chain.Blake][0]
	tx := activityTransaction(t, "", entry.script, 100000, entry.script, 200000)
	e.watch[chain.Blake] = activityBackend{history: func(context.Context, string, string, int) (chain.AddressHistoryPage, error) {
		return chain.AddressHistoryPage{Source: "source", Transactions: []chain.Transaction{tx}, Complete: true}, nil
	}}
	e.indexActivityChain(context.Background(), chain.Blake)
	index := e.s.ActivityIndexes[chain.Blake]
	if index.Error == "" || index.Address != 0 || index.After != "" || index.CompletedPass != 0 || len(e.s.Activities) != maxActivityRecords-1 || len(e.s.ActivityReceipts) != 0 {
		t.Fatal("capacity loss advanced history or partially applied its transaction", index, len(e.s.Activities), len(e.s.ActivityReceipts))
	}
}

func TestActivityLateObservationCannotUpdateNewSourceGeneration(t *testing.T) {
	id := strings.Repeat("a", 64)
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{Activities: map[string]Activity{
		"send/request": {ID: "send/request", Chain: chain.Blake, TxID: id, Variants: []string{id}, Status: "broadcast"},
	}}, nodes: map[chain.ID]chain.Backend{}}
	e.nodes[chain.Blake] = activityGenerationBackend{generation: 2, activityBackend: activityBackend{observe: func(context.Context, string, uint32, string) (chain.HistoryTransaction, error) {
		return chain.HistoryTransaction{Transaction: chain.Transaction{TxID: id, Confirmations: 10}, Source: "old-source", Generation: 1}, nil
	}}}
	e.observeActivityChain(context.Background(), chain.Blake)
	got := e.s.Activities["send/request"]
	if got.Status != "broadcast" || len(got.Observations) != 0 || got.Source != "" || e.s.ActivityRevision != 0 {
		t.Fatal("old endpoint observation changed current history", got)
	}
}

func TestActivityReopenDoesNotReusePersistedBackendGeneration(t *testing.T) {
	id := strings.Repeat("a", 64)
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{Activities: map[string]Activity{}, ActivityIndexes: map[chain.ID]ActivityIndex{chain.Blake: {Address: 4, After: id, CompletedPass: time.Now().Unix(), Source: "old", Generation: 1}}}, nodes: map[chain.ID]chain.Backend{}}
	e.putActivity(Activity{ID: "send/request", Chain: chain.Blake, TxID: id, Variants: []string{id}, Status: "confirmed", Observations: []ActivityObservation{{TxID: id, Status: "confirmed", Confirmations: 6, Height: 10, BlockHash: "old-block", ObservedAt: time.Now().Unix(), Source: "old", Generation: 1}}}, true)
	encoded, _ := json.Marshal(e.s)
	if err := json.Unmarshal(encoded, &e.s); err != nil {
		t.Fatal(err)
	}
	e.nodes[chain.Blake] = activityGenerationBackend{generation: 1, activityBackend: activityBackend{observe: func(context.Context, string, uint32, string) (chain.HistoryTransaction, error) {
		return chain.HistoryTransaction{Transaction: chain.Transaction{TxID: id, Confirmations: 2, Height: 20, BlockHash: "new-block"}, Source: "new", Generation: 1}, nil
	}}}
	e.invalidateActivitySession()
	got := e.s.Activities["send/request"]
	if got.Status != "unknown" || got.Confirmations != 0 || got.ObservedAt != 0 || len(got.History) != 1 || got.History[0].Status != "confirmed" || got.Observations[0].BlockHash != "old-block" {
		t.Fatal("reopen reused old confirmation or discarded evidence", got)
	}
	if index := e.s.ActivityIndexes[chain.Blake]; index.CompletedPass != 0 || index.Address != 0 || index.After != "" {
		t.Fatal("reopen reused old coverage cursor", index)
	}
	e.observeActivityChain(context.Background(), chain.Blake)
	got = e.s.Activities["send/request"]
	if got.Status != "confirmed" || got.BlockHash != "new-block" || got.Source != "new" {
		t.Fatal("fresh verification did not restore current outcome", got)
	}
}

func TestActivityPreparedClaimAttemptAndObservationAreDistinct(t *testing.T) {
	tx := activityTransaction(t, "", []byte{0x51}, 100000, nil, 0)
	s := &Swap{ID: "swap", Role: "maker", SelfClaim: tx.Hex, Stage: "accepted"}
	s.Request.OfferEvent.Content = `{"id":"order","sell":"btc","sell_amount":200000,"buy_amount":102000}`
	s.Long.Chain, s.Long.Amount = chain.Blake, 102000
	s.Short.Chain, s.Short.Amount = chain.BTC, 200000
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{ActivityVersion: 1, Swaps: map[string]*Swap{s.ID: s}}}
	key := "swap/" + s.ID + "/claim"
	e.syncActivity()
	if a := e.s.Activities[key]; a.Status != "prepared" || len(a.History) != 0 {
		t.Fatal("unsubmitted signed bytes became a broadcast", a)
	}
	// The production path saves this timestamp before I/O. A crash here leaves
	// an ambiguous attempt, not evidence that a node received the transaction.
	s.ClaimLastAttempt = time.Now().Unix()
	e.syncActivity()
	if a := e.s.Activities[key]; a.Status != "attempted" || len(a.History) != 1 || a.History[0].Status != "prepared" {
		t.Fatal("attempt lost its ambiguity or fabricated prior broadcast", a)
	}
	s.LongSpend = tx.TxID
	e.syncActivity()
	if a := e.s.Activities[key]; a.Status != "observed" {
		t.Fatal("scanner observation not distinguished from submission attempt", a)
	}
	a := e.s.Activities[key]
	a.Observations = []ActivityObservation{observationFor(tx, "source", 1, 1)}
	e.putActivity(a, true)
	if a := e.s.Activities[key]; a.Status != "confirmed" {
		t.Fatal("canonical verification did not override local submission status", a)
	}
}

type activityBackend struct {
	chain.Backend
	history func(context.Context, string, string, int) (chain.AddressHistoryPage, error)
	observe func(context.Context, string, uint32, string) (chain.HistoryTransaction, error)
}

func (b activityBackend) Close() error { return nil }

type activityGenerationBackend struct {
	activityBackend
	generation uint64
}

func (b activityGenerationBackend) Generation() uint64 { return b.generation }

func (b activityBackend) AddressHistory(ctx context.Context, address, after string, limit int) (chain.AddressHistoryPage, error) {
	return b.history(ctx, address, after, limit)
}
func (b activityBackend) HistoryTransaction(ctx context.Context, id string, height uint32, block string) (chain.HistoryTransaction, error) {
	return b.observe(ctx, id, height, block)
}

func activityTransaction(t *testing.T, previous string, script []byte, amount int64, otherScript []byte, otherAmount int64) chain.Transaction {
	t.Helper()
	tx := wire.NewMsgTx(2)
	point := wire.OutPoint{Index: ^uint32(0)}
	if previous != "" {
		hash, err := chainhash.NewHashFromStr(previous)
		if err != nil {
			t.Fatal(err)
		}
		point = wire.OutPoint{Hash: *hash}
	}
	tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	tx.AddTxOut(wire.NewTxOut(amount, script))
	if otherScript != nil {
		tx.AddTxOut(wire.NewTxOut(otherAmount, otherScript))
	}
	return chain.Transaction{TxID: tx.TxHash().String(), Hex: contract.Hex(tx), Confirmations: 2, Height: 10, BlockHash: strings.Repeat("a", 64), BlockTime: 1234567890}
}

func TestActivitySpentRotatedReceiptAndSendChangeAreLinkedWithoutDoubleCounting(t *testing.T) {
	oldScript := append([]byte{0, 20}, make([]byte, 20)...)
	newScript := append([]byte{0, 20}, append([]byte{1}, make([]byte, 19)...)...)
	external := append([]byte{0, 20}, append([]byte{2}, make([]byte, 19)...)...)
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{Activities: map[string]Activity{}}, receiveBook: map[chain.ID][]receiveAddress{chain.Blake: {{address: "old", script: oldScript}, {address: "new", script: newScript}}}}
	deposit := activityTransaction(t, "", oldScript, 500000, nil, 0)
	send := activityTransaction(t, deposit.TxID, newScript, 200000, external, 299000)
	// Deposit has already been spent and the address has rotated. No UTXO read
	// participates in reconstructing either receipt.
	if err := e.recordActivityReceipt(chain.Blake, deposit, "source", 1); err != nil {
		t.Fatal(err)
	}
	e.putActivity(Activity{ID: "send/request", GroupID: "send/request", Kind: "send", Chain: chain.Blake, Direction: "outgoing", Movement: true, Amount: 300000, Principal: 299000, Fee: 1000, FeeKnown: true, FeePayer: "wallet", SendID: "request", Address: "external", TxID: send.TxID, Variants: []string{send.TxID}, Status: "broadcast"}, true)
	if err := e.recordActivityReceipt(chain.Blake, send, "source", 1); err != nil {
		t.Fatal(err)
	}
	e.reconcileActivityReceipts()
	var incoming, outgoing int64
	for _, a := range e.s.Activities {
		if a.Movement {
			if a.Direction == "incoming" {
				incoming += a.Amount
			}
			if a.Direction == "outgoing" {
				outgoing += a.Amount
			}
		}
		if a.Kind == "receive" && a.TxID == send.TxID && (a.Classification != "change" || a.Movement || a.GroupID != "send/request" || a.SendID != "request") {
			t.Fatal(a)
		}
		if a.Kind == "receive" && (a.CreatedAt != 0 || a.CreatedSource != "unknown" || a.BlockTime != 1234567890) {
			t.Fatal("invented creation time", a)
		}
	}
	if incoming != 500000 || outgoing != 300000 || len(e.s.Activities) != 3 {
		t.Fatal(incoming, outgoing, len(e.s.Activities))
	}
	encoded, _ := json.Marshal(e.s)
	var restored State
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	e.s = restored
	if err := e.recordActivityReceipt(chain.Blake, deposit, "source", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.recordActivityReceipt(chain.Blake, send, "source", 1); err != nil {
		t.Fatal(err)
	}
	e.reconcileActivityReceipts()
	if len(e.s.Activities) != 3 {
		t.Fatal("restart/replay duplicated payment")
	}
}

func TestActivityReplacementReorgRetainsLineageAndEarlierVariantCanConfirm(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	old := ActivityObservation{Sequence: 1, TxID: a, Status: "confirmed", Confirmations: 2, Height: 10, BlockHash: "old-block", ObservedAt: time.Now().Unix(), Source: "source", Generation: 1}
	e := &Engine{Config: Config{Name: "alice", Network: chain.Regtest}, s: State{ActivityObservationSequence: 1, Activities: map[string]Activity{}}, nodes: map[chain.ID]chain.Backend{}}
	e.putActivity(Activity{ID: "send/request", GroupID: "send/request", Kind: "send", Chain: chain.Blake, Direction: "outgoing", Movement: true, TxID: a, Amount: 102000, Principal: 100000, Fee: 2000, FeeKnown: true, Variants: []string{a, b}, VariantAmounts: []ActivityVariant{{TxID: a, Amount: 102000, Principal: 100000, Fee: 2000, FeeKnown: true}, {TxID: b, Amount: 106000, Principal: 100000, Fee: 6000, FeeKnown: true}}, Observations: []ActivityObservation{old}}, true)
	confirming := b
	e.nodes[chain.Blake] = activityBackend{observe: func(ctx context.Context, id string, height uint32, block string) (chain.HistoryTransaction, error) {
		if id == confirming {
			return chain.HistoryTransaction{Transaction: chain.Transaction{TxID: id, Confirmations: 2, Height: 11, BlockHash: "current-block"}, Source: "source", Generation: 2}, nil
		}
		return chain.HistoryTransaction{Source: "source", Generation: 2, PreviousBlockChanged: true}, errors.New("not found")
	}}
	e.observeActivityChain(context.Background(), chain.Blake)
	got := e.s.Activities["send/request"]
	if got.Status != "confirmed" || got.TxID != b || got.Fee != 6000 || got.Amount != 106000 || len(got.History) == 0 {
		t.Fatal(got)
	}
	confirming = a
	e.observeActivityChain(context.Background(), chain.Blake)
	got = e.s.Activities["send/request"]
	if got.Status != "confirmed" || got.TxID != a || got.Fee != 2000 || len(got.Variants) != 2 {
		t.Fatal("earlier signed variant was forgotten", got)
	}
	confirming = ""
	e.observeActivityChain(context.Background(), chain.Blake)
	got = e.s.Activities["send/request"]
	if got.Status == "confirmed" || len(got.History) < 2 {
		t.Fatal("reorg left a final outcome", got)
	}
}

func TestActivityHistoryReadsAreBoundedAndDoNotHoldEngineLock(t *testing.T) {
	e, _, _ := sendFixture(t)
	entered := make(chan struct{})
	e.watch[chain.Blake] = activityBackend{Backend: e.watch[chain.Blake], history: func(ctx context.Context, address, after string, limit int) (chain.AddressHistoryPage, error) {
		close(entered)
		<-ctx.Done()
		return chain.AddressHistoryPage{}, ctx.Err()
	}}
	finished := make(chan struct{})
	go func() { e.indexActivityChain(context.Background(), chain.Blake); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("history read never started")
	}
	status := make(chan struct{})
	go func() { e.Status(); close(status) }()
	select {
	case <-status:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("history held the engine lock")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("history exceeded bounded read")
	}
	if e.s.ActivityIndexes[chain.Blake].Error == "" {
		t.Fatal("history failure was hidden")
	}
}

func TestActivityCloseCancelsJoinsAndRejectsLateHistoryResponse(t *testing.T) {
	e, _, _ := sendFixture(t)
	for id, backend := range e.nodes {
		e.nodes[id] = activityBackend{Backend: backend}
	}
	entered := make(chan struct{})
	late := activityTransaction(t, "", e.scripts[chain.Blake], 100000, nil, 0)
	e.watch[chain.Blake] = activityBackend{Backend: e.watch[chain.Blake], history: func(ctx context.Context, address, after string, limit int) (chain.AddressHistoryPage, error) {
		close(entered)
		<-ctx.Done()
		return chain.AddressHistoryPage{Transactions: []chain.Transaction{late}, Complete: true, Source: "old"}, nil
	}}
	done := make(chan struct{})
	go func() { e.refreshActivity(context.Background()); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("history did not start")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not join history read")
	}
	for _, a := range e.s.Activities {
		if a.TxID == late.TxID {
			t.Fatal("closed engine accepted late history")
		}
	}
}

func TestActivityRejectsChangedWalletNetworkAndSourceGeneration(t *testing.T) {
	for _, change := range []string{"wallet", "network", "source"} {
		t.Run(change, func(t *testing.T) {
			e, _, _ := sendFixture(t)
			late := activityTransaction(t, "", e.scripts[chain.Blake], 100000, nil, 0)
			e.watch[chain.Blake] = activityBackend{Backend: e.watch[chain.Blake], history: func(ctx context.Context, address, after string, limit int) (chain.AddressHistoryPage, error) {
				e.mu.Lock()
				if change == "wallet" {
					e.Config.Name = "different"
				}
				if change == "network" {
					e.Config.Network = chain.Mainnet
				}
				e.mu.Unlock()
				return chain.AddressHistoryPage{Transactions: []chain.Transaction{late}, Complete: true, Source: "old", Generation: 1}, nil
			}}
			if change == "source" {
				e.nodes[chain.Blake] = activityGenerationBackend{activityBackend: activityBackend{Backend: e.nodes[chain.Blake]}, generation: 2}
			}
			e.indexActivityChain(context.Background(), chain.Blake)
			for _, a := range e.s.Activities {
				if a.TxID == late.TxID {
					t.Fatal("stale binding response applied")
				}
			}
		})
	}
}
