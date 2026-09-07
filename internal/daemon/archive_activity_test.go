package daemon

import (
	"encoding/hex"
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"testing"
	"time"
)

func TestArchiveReceiptClassificationUsesRetainedParentAndOwnedInputs(t *testing.T) {
	e, _ := receiveEngine(t)
	parent := Activity{ID: "send/parent", GroupID: "send/parent", Kind: "send", Chain: chain.BTC, Status: "confirmed", Confirmations: 200, Variants: []string{"parent-tx"}, TxID: "parent-tx", Address: "recipient", Amount: 1000, Principal: 1000}
	owned := Activity{ID: "receive/old", GroupID: "receive/old", Kind: "receive", Chain: chain.BTC, Status: "confirmed", Confirmations: 200, Variants: []string{"owned-tx"}, TxID: "owned-tx", Outpoints: []CoinOutpoint{{TxID: "owned-tx", Vout: 0}}, Amount: 5000, Principal: 5000}
	for _, a := range []*Activity{&parent, &owned} {
		a.Version = 1
		a.Network = chain.Regtest
		a.Observations = []ActivityObservation{{TxID: a.TxID, Status: "confirmed", Confirmations: 200, Height: 1, BlockHash: "test-canonical-tip", ObservedAt: time.Now().Unix(), Source: "fixture"}}
	}
	e.s.Activities = map[string]Activity{parent.ID: parent, owned.ID: owned}
	e.indexActivityIdentity(parent)
	e.indexActivityIdentity(owned)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{chain.BTC: {Height: 200, Hash: "test-canonical-tip"}}
	remaining := 64
	if err := e.compactActivity(&remaining, map[chain.ID]bool{chain.BTC: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if len(e.s.Activities) != 0 || len(e.s.ActivityTransactions) != 0 || len(e.s.ActivityOwned) != 0 {
		t.Fatal("history indexes did not leave the active tier", e.s.Activities, e.s.ActivityTransactions, e.s.ActivityOwned)
	}
	receipt := Activity{ID: "receive/change", GroupID: "receive/change", Kind: "receive", Chain: chain.BTC, TxID: "parent-tx", Address: "change", Amount: 300, Principal: 300}
	self := Activity{ID: "receive/self", GroupID: "receive/self", Kind: "receive", Chain: chain.BTC, TxID: "later-tx", Amount: 4000, Principal: 4000}
	e.s.Activities = map[string]Activity{receipt.ID: receipt, self.ID: self}
	e.s.ActivityReceipts = map[string]ReceiptEvidence{"btc/later-tx": {Inputs: []CoinOutpoint{{TxID: "owned-tx", Vout: 0}}, Total: 4000, OwnedTotal: 4000}}
	e.reconcileActivityReceipts()
	change := e.s.Activities[receipt.ID]
	transfer := e.s.Activities[self.ID]
	if change.Movement || change.Classification != "change" || change.GroupID != parent.GroupID {
		t.Fatal("archived send change double-counted", change)
	}
	if transfer.Movement || transfer.Classification != "self_transfer" {
		t.Fatal("archived owned input lost classification", transfer)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	logical, err := CompleteState(snapshot)
	if err != nil || len(logical.Activities) != 4 || logical.ActivityTransactions["btc/parent-tx"] != parent.ID {
		t.Fatal("backup lost archived classification evidence", err)
	}
}

func TestArchiveStatusIsBoundedAndDetailRetainsAllActiveObligations(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "fixture"
	e.s.Sends = map[string]*WalletSend{}
	for i := range 1100 {
		id := string(rune(0x1000 + i))
		e.s.Sends[id] = &WalletSend{PublicSend: PublicSend{ID: id, Chain: chain.BTC, Confirmations: 200, State: "confirmed"}, Raw: "private signed recovery bytes"}
	}
	e.s.Sends["pending"] = &WalletSend{PublicSend: PublicSend{ID: "pending", Chain: chain.BTC, Confirmations: 0, State: "saved"}}
	status := e.Status()
	if len(status.Sends) != 101 {
		t.Fatal("status leaked lifetime completed history", len(status.Sends))
	}
	found := false
	for _, send := range status.Sends {
		found = found || send.ID == "pending"
	}
	if !found {
		t.Fatal("bounded status omitted active send")
	}
	id := string(rune(0x1000 + 1099))
	if err := e.stageArchive("sends", id); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]string{"kind": "send", "id": id, "expected_wallet": "fixture", "expected_network": "regtest"})
	detail, err := e.recordDetail(request)
	if err != nil || !detail.Archived || detail.Send.ID != id {
		t.Fatal("stable archived detail missing", err)
	}
	encoded, _ := json.Marshal(detail)
	if string(encoded) == "" || containsPrivate(string(encoded)) {
		t.Fatal("public detail exposed private signing material")
	}
}
func containsPrivate(value string) bool {
	for i := 0; i+7 <= len(value); i++ {
		if value[i:i+7] == "private" {
			return true
		}
	}
	return false
}

func TestArchiveSharedReceiptRetainsActiveClassification(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Activities = map[string]Activity{}
	e.s.ActivityOwned = map[string]int64{"btc/owned-input:0": 10000}
	e.s.ActivityReceipts = map[string]ReceiptEvidence{"btc/self-tx": {Inputs: []CoinOutpoint{{TxID: "owned-input", Vout: 0}}, Total: 8000, OwnedTotal: 8000}}
	for vout := uint32(0); vout < 2; vout++ {
		point := CoinOutpoint{TxID: "self-tx", Vout: vout}
		id := activityID("receive", "btc/"+pointKey(point))
		a := Activity{ID: id, GroupID: id, Kind: "receive", Chain: chain.BTC, Status: "confirmed", Confirmations: 200, TxID: "self-tx", Variants: []string{"self-tx"}, Outpoints: []CoinOutpoint{point}, Amount: 4000, Principal: 4000, Observations: []ActivityObservation{{TxID: "self-tx", Status: "confirmed", Confirmations: 200, Height: 1, BlockHash: "test-canonical-tip", ObservedAt: time.Now().Unix(), Source: "fixture"}}}
		e.putActivity(a, true)
	}
	e.walletCoins = map[chain.ID]map[string][]chain.UTXO{chain.BTC: {hex.EncodeToString(e.receiveBook[chain.BTC][0].script): {{TxID: "self-tx", Vout: 1, Amount: 4000, Confirmations: 200}}}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	activeID := activityID("receive", "btc/self-tx:1")
	if a := e.s.Activities[activeID]; a.Classification != "self_transfer" || a.Movement {
		t.Fatalf("bad precondition: %+v", a)
	}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{chain.BTC: {Height: 200, Hash: "test-canonical-tip"}}
	remaining := 64
	if err := e.compactActivity(&remaining, map[chain.ID]bool{chain.BTC: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	a := e.s.Activities[activeID]
	if a.Classification != "self_transfer" || a.Movement {
		t.Fatalf("compaction changed still-owned sibling to %s movement=%v; active evidence=%v", a.Classification, a.Movement, e.s.ActivityReceipts)
	}
}
func TestArchivedSwapDetailRetainsFundingFee(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "fixture"
	e.s.Swaps = map[string]*Swap{"settled": {ID: "settled", Role: "taker", Stage: "completed"}}
	e.s.FundingFees = map[string]FeeSelection{"swap/settled": {FundingFee: 3456}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"swaps", "settled"}, {"funding_fees", "swap/settled"}} {
		if err := e.stageArchive(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	detail, err := e.recordDetail(json.RawMessage(`{"kind":"swap","id":"settled","expected_wallet":"fixture","expected_network":"regtest"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Archived || detail.Swap.FundingFee != 3456 {
		t.Fatalf("archived swap reports funding fee %d, want retained 3456", detail.Swap.FundingFee)
	}
}

func TestArchivedMakerDetailUsesOfferFeeAndRejectsUnreadableEvidence(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Name = "fixture"
	terms := &protocol.Terms{}
	terms.Request.OfferEvent.Content = `{"id":"source"}`
	e.s.Swaps = map[string]*Swap{"settled": {ID: "settled", Role: "maker", Stage: "completed", Terms: terms}}
	e.s.FundingFees = map[string]FeeSelection{"offer/source": {FundingFee: 4567}, "swap/settled": {FundingFee: 1234}}
	for _, key := range []string{"offer/source", "swap/settled"} {
		if err := e.stageArchive("funding_fees", key); err != nil {
			t.Fatal(err)
		}
	}
	request := json.RawMessage(`{"kind":"swap","id":"settled","expected_wallet":"fixture","expected_network":"regtest"}`)
	detail, err := e.recordDetail(request)
	if err != nil || detail.Swap.FundingFee != 4567 {
		t.Fatal("maker detail lost its offer fee", detail, err)
	}
	// A present but malformed record must never become an apparently accurate
	// legacy fee. This exercises the projection's archive decoding error path.
	key := archiveMoveKey("funding_fees", "offer/source")
	e.archivePuts[key] = storage.ArchiveRecord{Kind: "funding_fees", ID: "offer/source", Data: json.RawMessage(`{"funding_fee":"invalid"}`)}
	if _, err := e.recordDetail(request); err == nil {
		t.Fatal("invalid archived fee became a legacy fee")
	}
	delete(e.archivePuts, key)
}
