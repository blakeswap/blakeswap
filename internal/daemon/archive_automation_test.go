package daemon

import (
	"context"
	"fiatjaf.com/nostr"
	"testing"
	"time"
)

func TestArchiveCompactionKeepsExpiredAutomationRenewable(t *testing.T) {
	e, p := automationFixture(t)
	p.Config.VolumeLimit = p.Config.SellAmount
	e.runAutomations(context.Background())
	old := p.CurrentOfferID
	if old == "" {
		t.Fatal(p.Decision)
	}
	// Model the instant the signed offer expires, before the next save has
	// reconciled its reserved charge. The relay has acknowledged its publication.
	o, err := historicalOffer(e.s.Offers[old])
	if err != nil {
		t.Fatal(err)
	}
	o.Expires = time.Now().Unix() - 1
	event, err := e.signOffer(o, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.stageOffer(o, event)
	e.s.Outbox = map[string]*Delivery{}
	p.NextAction = 0
	e.reconcileReservations() // Tick refreshes coin holds before compaction.
	if p.Charges[old].State != "reserved" {
		t.Fatal("probe lost initial reservation")
	}
	if err = e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	e.runAutomations(context.Background())
	if p.Charges[old].State != "released" || p.CurrentOfferID == old || p.Charges[old].Successor != p.CurrentOfferID {
		t.Fatalf("compaction lost no-fill renewal: old charge=%+v current=%s decision=%s", p.Charges[old], p.CurrentOfferID, p.Decision)
	}
}
