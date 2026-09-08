package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func waitParentPublicationClock(t *testing.T, parent *ParentOrder) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Unix() <= parent.LastSignedAt && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if time.Now().Unix() <= parent.LastSignedAt {
		t.Fatal("fixture never reached the next real publication second")
	}
}

// Real accepted/funded children and signed outcome fixtures are ordinary tests,
// not chain-inclusion evidence. Only the public delivery is drained as it is
// after a relay acknowledgment; child messages, terms and funding remain intact.
func cancelledPublicationFixture(t *testing.T, sell chain.ID) (*Engine, []*Swap, [][]byte) {
	t.Helper()
	e, children, secrets := fundedFillPair(t, sell)
	parent := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
	waitParentPublicationClock(t, parent)
	if err := e.withdrawParentAvailable(parent.Offer.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	for id, delivery := range e.s.Outbox {
		if delivery.Event.Kind == transport.OfferKind {
			delete(e.s.Outbox, id)
		}
	}
	record := e.s.OrderRecords[parent.Offer.ID]
	record.CancelledEventID = children[0].Request.OfferEvent.ID.Hex()
	e.s.OrderRecords[parent.Offer.ID] = record
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, children, secrets
}

func TestParentPublicationTransfersArchivedOwnership(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, placement := range []string{"pending", "persisted", "reactivated"} {
			t.Run(string(sell)+"/"+placement, func(t *testing.T) {
				e, children, secrets := cancelledPublicationFixture(t, sell)
				ctx := context.Background()
				parentID := e.s.FillRecords[children[0].ID].ParentID
				all := fillPairOutcomes(t, e, children, secrets, false)
				if placement == "pending" || placement == "reactivated" {
					for i, child := range children {
						if i == 1 && placement == "pending" {
							break
						}
						if err := e.advanceSwap(ctx, child, all); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := e.compactArchive(ctx, all, nil); err != nil {
					t.Fatal(err)
				}
				for _, kind := range []string{"offers", "order_records"} {
					if _, found, err := e.archiveRecord(kind, parentID); err != nil || !found {
						t.Fatal("cancelled parent source did not archive", kind, err)
					}
				}
				if placement != "pending" {
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
				}
				if placement == "persisted" {
					if err := e.advanceSwap(ctx, children[0], all); err != nil {
						t.Fatal(err)
					}
				}
				if placement == "reactivated" {
					// Explicit cold placement tests ownership restoration, not
					// the depth policy (the signed outcomes have only two confirmations).
					for _, child := range children {
						if err := e.stageArchive("swaps", child.ID); err != nil {
							t.Fatal(err)
						}
					}
					if err := e.stageArchive("parent_orders", parentID); err != nil {
						t.Fatal(err)
					}
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					if _, err := e.activateArchived("swaps", children[0].ID); err != nil {
						t.Fatal(err)
					}
					child := e.s.Swaps[children[0].ID]
					delete(all[child.Long.Chain], chain.OutpointKey(child.Long.TxID, child.Long.Vout))
					e.nodes[sell] = &fundingLookupBackend{err: context.DeadlineExceeded}
					_ = e.advanceSwap(ctx, child, all)
					if e.s.FillRecords[child.ID].Allocation.Disposition != FillCommitted {
						t.Fatal("positive fresh contradiction did not demote the restored child")
					}
				}
				parent := e.s.ParentOrders[parentID]
				var oldRecord OrderRecord
				if found, err := e.archivedValue("order_records", parentID, &oldRecord); err != nil || !found {
					t.Fatal("source history missing", err)
				}
				custody := protocol.Digest([]any{parent.Quantities, parent.Fees, parent.Bounties, e.s.FillRecords, e.s.Swaps, e.s.CoinReservations})
				waitParentPublicationClock(t, parent)
				if err := e.publishPendingParents(); err != nil {
					t.Fatal(err)
				}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				var saved State
				if _, err := e.vault.Load(&saved); err != nil {
					t.Fatal(err)
				}
				if err := ValidateVaultProtocolState(e.vault, &saved); err != nil {
					t.Fatal("parent revision left duplicate archived ownership", err)
				}
				if protocol.Digest([]any{parent.Quantities, parent.Fees, parent.Bounties, e.s.FillRecords, e.s.Swaps, e.s.CoinReservations}) != custody {
					t.Fatal("publication changed child custody or conserved money")
				}
				current := e.s.OrderRecords[parentID]
				oldRecord.Offer, oldRecord.EventID, oldRecord.Publication, oldRecord.AcknowledgedAt = current.Offer, current.EventID, current.Publication, current.AcknowledgedAt
				if !reflect.DeepEqual(oldRecord, current) || current.Offer.Status != "cancelled" || current.Offer.Available != 0 || current.Offer.Revision != parent.Quantities.Revision || parent.SignedRevision != parent.Quantities.Revision {
					t.Fatal("publication lost management history or changed cancelled availability")
				}
				for _, kind := range []string{"offers", "order_records"} {
					if _, found, err := e.vault.ReadArchive(kind, parentID); err != nil || found {
						t.Fatal("new active publication retained a second cold owner", kind, err)
					}
				}
				path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
				if err := e.vault.Close(); err != nil {
					t.Fatal(err)
				}
				if err := PreflightStateVersion(path, []byte("receive-test-password")); err != nil {
					t.Fatal("reopen preflight", err)
				}
				opened, state, err := openCurrentStateVault(path, []byte("receive-test-password"))
				if err != nil {
					t.Fatal("reopen", err)
				}
				defer opened.Close()
				before, _ := json.Marshal(saved)
				after, _ := json.Marshal(state)
				if string(before) != string(after) {
					t.Fatal("reopen modified the complete saved source")
				}
			})
		}
	}
}
