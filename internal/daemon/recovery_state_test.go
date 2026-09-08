package daemon

import (
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func TestPrepareRecoveryPreservesObligationsAndQuarantinesPublications(t *testing.T) {
	e, swap, _, secret := isolatedFixture(t, "maker")
	key := isolatedSpendKey(t, e, swap, swap.Long, false)
	claim, err := contract.Spend(swap.Long, key, e.scripts[swap.Long.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	swap.SelfClaim = contract.Hex(claim)
	swap.SecretExposed = true
	swap.Stage = "completed" // A display label must not erase imported obligations.
	tower := e.ownTower()
	swap.Protection = &tower
	job, err := e.makeJob(swap, swap.Short, "refund", nil, swap.Short.RefundHeight+protocol.RefundDelay(e.Config.Network))
	if err != nil {
		t.Fatal(err)
	}
	e.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job, Secret: swap.Secret}}
	e.s.Sends = map[string]*WalletSend{"send": {PublicSend: PublicSend{ID: "send"}, Raw: "payment"}}
	if err := e.queue(swap.Request.Taker, "short-funded", swap.ID, fundingMessage{TermsHash: protocol.Digest(swap.Terms), Raw: swap.ShortFunding}); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range e.s.Outbox {
		delivery.Published = true
	}
	offerID := swap.Terms.Offer().ID
	e.s.Book = map[string]nostr.Event{"cached": e.s.Offers[offerID]}
	s := e.s
	originalSwap, originalJob := protocol.Digest(swap), protocol.Digest(e.s.TowerJobs[job.ID])
	originalOffers, originalOutbox := protocol.Digest(s.Offers), protocol.Digest(s.Outbox)
	originalParent := *s.ParentOrders[offerID]
	originalChild := *s.FillRecords[swap.ID]

	if err := PrepareRecovery(&s, 100, false); err != nil {
		t.Fatal(err)
	}
	if len(s.Offers) != 0 || len(s.Outbox) != 0 || len(s.Book) != 0 {
		t.Fatal("stale publication remained live")
	}
	if !s.Recovery.Swaps[swap.ID] || !s.Recovery.Sends["send"] || !s.Recovery.TowerJobs[job.ID] || protocol.Digest(s.Swaps[swap.ID]) != originalSwap || s.Sends["send"].Raw != "payment" || protocol.Digest(s.TowerJobs[job.ID]) != originalJob {
		t.Fatal("recovery material lost")
	}
	if protocol.Digest(s.Recovery.Offers) != originalOffers || protocol.Digest(s.Recovery.Outbox) != originalOutbox {
		t.Fatal("quarantine changed signed publication bytes or provenance")
	}
	originalParent.RestoreHold = true
	originalChild.ImportedUncertain = true
	if protocol.Digest(s.ParentOrders[offerID]) != protocol.Digest(originalParent) || protocol.Digest(s.FillRecords[swap.ID]) != protocol.Digest(originalChild) {
		t.Fatal("import changed conserved quantities, permanent charges, or child authority")
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
	if restored.Recovery.Status.State != "recovering" || restored.Recovery.SnapshotAt != 100 || !restored.Recovery.Legacy || len(restored.Recovery.Offers) != 1 || len(restored.Recovery.Outbox) != 1 || !restored.Recovery.Swaps[swap.ID] {
		t.Fatal("re-export bypassed reconciliation or lost original provenance")
	}
}

func TestRecoveryFingerprintRetainsDurableHolds(t *testing.T) {
	s := State{Version: StateVersion, Network: chain.Regtest}
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
