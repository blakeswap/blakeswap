package daemon

import (
	"context"
	"testing"
)

func TestArchiveCompactionKeepsExpiredAutomationRenewable(t *testing.T) {
	e, p := automationFixture(t)
	p.Config.VolumeLimit = p.Config.SellAmount
	e.runAutomations(context.Background())
	old := p.CurrentOfferID
	if old == "" {
		t.Fatal(p.Decision)
	}
	// Advance the real parent expiry transition before charge reconciliation.
	// The relay has acknowledged the resulting signed tombstone.
	expirePolicyParent(t, e, p)
	e.s.Outbox = map[string]*Delivery{}
	p.NextAction = 0
	e.reconcileReservations() // Tick refreshes coin holds before compaction.
	if p.Charges[old].State != "reserved" {
		t.Fatal("probe lost initial reservation")
	}
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.runAutomations(context.Background())
	if p.Charges[old].State != "released" || p.CurrentOfferID == old || p.Charges[old].Successor != p.CurrentOfferID {
		t.Fatalf("compaction lost no-fill renewal: old charge=%+v current=%s decision=%s", p.Charges[old], p.CurrentOfferID, p.Decision)
	}
}
