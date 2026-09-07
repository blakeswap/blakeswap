package daemon

import (
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/chain"
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
