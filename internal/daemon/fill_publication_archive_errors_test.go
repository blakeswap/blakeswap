package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func pendingColdParentPublication(t *testing.T) (*Engine, *ParentOrder) {
	t.Helper()
	e, children, secrets := cancelledPublicationFixture(t, chain.BTC)
	all := fillPairOutcomes(t, e, children, secrets, false)
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.advanceSwap(context.Background(), children[0], all); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	parent := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
	waitParentPublicationClock(t, parent)
	return e, parent
}

func TestParentPublicationColdFailuresLeaveOwnersUnchanged(t *testing.T) {
	e, parent := pendingColdParentPublication(t)
	for _, failure := range []string{"offer-read", "floor-read", "floor-counter", "history-read", "history-malformed", "history-parent", "history-economics", "offer-signature", "second-counter"} {
		t.Run(failure, func(t *testing.T) {
			capacity := *e.s.Capacity
			capacity.Archived.Kinds = maps.Clone(capacity.Archived.Kinds)
			if failure == "floor-counter" {
				e.s.Capacity.Archived.Kinds["own_public_versions"] = 0
			}
			if failure == "second-counter" {
				e.s.Capacity.Archived.Kinds["order_records"] = 0
			}
			e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
				record, found, err := e.vault.ReadArchive(kind, id)
				if (id != parent.Offer.ID && id != parent.Offer.Maker+":"+parent.Offer.ID) || err != nil || !found {
					return record, found, err
				}
				if (kind == "own_public_versions" && failure == "floor-read") || (kind == "offers" && failure == "offer-read") || (kind == "order_records" && failure == "history-read") {
					return storage.ArchiveRecord{}, false, errors.New("injected exact source read failure")
				}
				if kind == "order_records" {
					var history OrderRecord
					_ = json.Unmarshal(record.Data, &history)
					switch failure {
					case "history-malformed":
						record.Data = []byte("{")
					case "history-parent":
						history.Offer.ID = protocol.Digest("another parent")
						record.Data, _ = json.Marshal(history)
					case "history-economics":
						history.Offer.BuyAmount++
						record.Data, _ = json.Marshal(history)
					}
				}
				if kind == "offers" && failure == "offer-signature" {
					var event nostr.Event
					_ = json.Unmarshal(record.Data, &event)
					event.Content += " "
					record.Data, _ = json.Marshal(event)
				}
				return record, found, err
			}
			before := protocol.Digest([]any{e.s, e.archivePuts, e.archiveDeletes, e.archiveOrigins})
			if err := e.publishParent(parent.Offer.ID); err == nil {
				t.Fatal("failed cold evidence authorized a new publication")
			}
			if protocol.Digest([]any{e.s, e.archivePuts, e.archiveDeletes, e.archiveOrigins}) != before {
				t.Fatal("failed publication changed ownership, metadata or signed revision")
			}
			e.archiveRead = nil
			*e.s.Capacity = capacity
		})
	}
	before := protocol.Digest(e.s)
	parent.RestoreHold = true
	held := protocol.Digest(e.s)
	if err := e.publishParent(parent.Offer.ID); err != nil || protocol.Digest(e.s) != held {
		t.Fatal("recovery hold published or mutated a parent", err)
	}
	parent.RestoreHold = false
	if protocol.Digest(e.s) != before {
		t.Fatal("fixture did not restore the same local parent")
	}
	if err := e.publishParent(parent.Offer.ID); err != nil {
		t.Fatal("exact-source retry", err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
		t.Fatal("retry left conflicting source owners", err)
	}
}

func TestParentPublicationFailedSavePreservesColdCheckpoint(t *testing.T) {
	e, parent := pendingColdParentPublication(t)
	var before State
	if _, err := e.vault.Load(&before); err != nil {
		t.Fatal(err)
	}
	if err := e.publishParent(parent.Offer.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err == nil || e.fatal == nil {
		t.Fatal("failed publication commit did not stop execution", err)
	}
	if err := PreflightStateVersion(path, []byte("receive-test-password")); err != nil {
		t.Fatal("failed save damaged original reopen checkpoint", err)
	}
	vault, after, err := openCurrentStateVault(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	if protocol.Digest(before) != protocol.Digest(after) || after.ParentOrders[parent.Offer.ID].SignedRevision == parent.SignedRevision {
		t.Fatal("uncommitted publication changed the retained cold checkpoint")
	}
}

func TestParentPublicationKeepsNewerColdOrderingFloor(t *testing.T) {
	e, parent := pendingColdParentPublication(t)
	key := parent.Offer.Maker + ":" + parent.Offer.ID
	// Another authenticated own event can arrive independently of the local
	// parent clock. Its stronger retained ordering floor must stay authoritative.
	offer := parentPublicOffer(*parent)
	newer, err := e.signOffer(offer, nostr.Timestamp(time.Now().Unix()+5))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.retainPublicOffer(newer, offer); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.stageArchive("own_public_versions", key); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, hot := e.s.OwnPublicVersions[key]; hot {
		t.Fatal("fixture did not commit its stronger floor cold")
	}
	reads := 0
	e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
		if kind == "own_public_versions" && id == key {
			reads++
			if reads > 1 {
				return storage.ArchiveRecord{}, false, errors.New("unexpected second ordering-floor read")
			}
		}
		return e.vault.ReadArchive(kind, id)
	}
	if err := e.publishParent(parent.Offer.ID); err != nil {
		t.Fatal(err)
	}
	e.archiveRead = nil
	if reads != 1 {
		t.Fatal("publication repeated a fallible floor read after committing owners", reads)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var floor PublicVersion
	if found, err := e.archivedValue("own_public_versions", key, &floor); err != nil || !found || floor.ID != newer.ID.Hex() || floor.CreatedAt != newer.CreatedAt {
		t.Fatal("older local publication replaced the stronger authenticated floor", err)
	}
	if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
		t.Fatal(err)
	}
}
