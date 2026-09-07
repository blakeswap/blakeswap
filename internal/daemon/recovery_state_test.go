package daemon

import (
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
)

func TestPrepareRecoveryPreservesObligationsAndQuarantinesPublications(t *testing.T) {
	s := State{Version: 1, Swaps: map[string]*Swap{"swap": {ID: "swap", Role: "taker", Secret: "private", SelfClaim: "signed", Stage: "completed"}}, Sends: map[string]*WalletSend{"send": {PublicSend: PublicSend{ID: "send"}, Raw: "payment"}}, TowerJobs: map[string]*TowerJob{"tower": {Secret: "learned"}}, Offers: map[string]nostr.Event{"offer": {Content: "old offer"}}, Outbox: map[string]*Delivery{"delivery": {Type: "funding", Published: true}}, Book: map[string]nostr.Event{"cached": {}}}
	if err := PrepareRecovery(&s, 100, false); err != nil {
		t.Fatal(err)
	}
	if len(s.Offers) != 0 || len(s.Outbox) != 0 || len(s.Book) != 0 {
		t.Fatal("stale publication remained live")
	}
	if !s.Recovery.Swaps["swap"] || !s.Recovery.Sends["send"] || !s.Recovery.TowerJobs["tower"] || s.Swaps["swap"].Secret != "private" || s.Swaps["swap"].SelfClaim != "signed" || s.Sends["send"].Raw != "payment" || s.TowerJobs["tower"].Secret != "learned" {
		t.Fatal("recovery material lost")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Recovery.Status.State = "ready"
	if err = PrepareRecovery(&restored, 200, true); err != nil {
		t.Fatal(err)
	}
	if restored.Recovery.Status.State != "recovering" || restored.Recovery.SnapshotAt != 100 || !restored.Recovery.Legacy || len(restored.Recovery.Offers) != 1 || len(restored.Recovery.Outbox) != 1 || !restored.Recovery.Swaps["swap"] {
		t.Fatal("re-export bypassed reconciliation or lost original provenance")
	}
}

func TestRecoveryFingerprintRetainsDurableHolds(t *testing.T) {
	s := State{Version: 1}
	if err := PrepareRecovery(&s, 100, false); err != nil {
		t.Fatal(err)
	}
	before, err := BackupFingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Recovery.Status.CheckedAt = 1234
	s.Recovery.Status.Issues = []RecoveryIssue{{Kind: "chain", Reason: "endpoint temporarily unavailable"}}
	after, err := BackupFingerprint(s)
	if err != nil || before != after {
		t.Fatal("polling dirtied backup", err)
	}
	s.Recovery.Swaps["newly-learned-obligation"] = true
	after, err = BackupFingerprint(s)
	if err != nil || before == after {
		t.Fatal("durable recovery hold omitted from fingerprint", err)
	}
}
