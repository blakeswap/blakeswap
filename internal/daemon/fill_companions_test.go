package daemon

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func coldRestoredFill(t *testing.T) (*Engine, string, string) {
	t.Helper()
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	parent := e.s.FillRecords[r.ID].ParentID
	if err := PrepareRecovery(&e.s, now, false); err != nil {
		t.Fatal(err)
	}
	for _, record := range [][2]string{{"swaps", r.ID}, {"recovery_swaps", r.ID}, {"funding_fees", "swap/" + r.ID}, {"parent_orders", parent}} {
		if err := e.stageArchive(record[0], record[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, r.ID, parent
}

func TestParentFillActivationRefusesIncompleteCompanionsBeforeAnyPromotion(t *testing.T) {
	for _, problem := range []string{"parent read", "parent economics", "child read", "child digest", "fee missing", "fee conflict", "origin malformed"} {
		t.Run(problem, func(t *testing.T) {
			e, id, parent := coldRestoredFill(t)
			before := protocol.Digest(e.s)
			e.archiveRead = func(kind, key string) (storage.ArchiveRecord, bool, error) {
				record, found, err := e.vault.ReadArchive(kind, key)
				if !found || err != nil {
					return record, found, err
				}
				switch {
				case problem == "parent read" && kind == "parent_orders", problem == "child read" && kind == "fill_records":
					return record, false, errors.New("injected authenticated companion read failure")
				case problem == "parent economics" && kind == "parent_orders":
					var p ParentOrder
					_ = json.Unmarshal(record.Data, &p)
					p.Offer.BuyAmount++
					p.Economics = p.Offer.EconomicsDigest()
					record.Data, _ = json.Marshal(p)
				case problem == "child digest" && kind == "fill_records":
					var child FillRecord
					_ = json.Unmarshal(record.Data, &child)
					child.RequestDigest = parent
					record.Data, _ = json.Marshal(child)
				case problem == "fee missing" && kind == "funding_fees":
					return storage.ArchiveRecord{}, false, nil
				case problem == "fee conflict" && kind == "funding_fees":
					var fee FeeSelection
					_ = json.Unmarshal(record.Data, &fee)
					fee.FundingFee++
					record.Data, _ = json.Marshal(fee)
				case problem == "origin malformed" && kind == "recovery_swaps":
					record.Data = []byte(`"invalid boolean"`)
				}
				return record, true, nil
			}
			if activated, err := e.activateArchived("swaps", id); err == nil || activated {
				t.Fatal("incomplete companion set activated", activated, err)
			}
			if protocol.Digest(e.s) != before || len(e.archiveDeletes) != 0 || len(e.archiveOrigins) != 0 {
				t.Fatal("failed companion validation partially promoted core or authority")
			}
			e.archiveRead = nil
			if activated, err := e.activateArchived("swaps", id); err != nil || !activated {
				t.Fatal("valid original companions could not activate", err)
			}
			if !e.restoredSwap(id) || e.s.FillRecords[id] == nil || !e.s.FillRecords[id].ImportedUncertain || !e.s.ParentOrders[parent].RestoreHold || e.s.FundingFees["swap/"+id].FundingFee != 6500 {
				t.Fatal("activation split original gate, allocation, parent or exact fee")
			}
		})
	}
}

func TestParentFillActivationFailedSaveReopensAllColdOwnersThenRetries(t *testing.T) {
	e, id, parent := coldRestoredFill(t)
	if _, err := e.activateArchived("swaps", id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err == nil {
		t.Fatal("forced closed-vault write succeeded")
	}
	v, err := storage.OpenExisting(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var saved State
	if _, err := v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Swaps[id] != nil || saved.FillRecords[id] != nil || saved.ParentOrders[parent] != nil || saved.Recovery.Swaps[id] {
		t.Fatal("failed save published some promoted owners")
	}
	for _, key := range [][2]string{{"swaps", id}, {"fill_records", id}, {"parent_orders", parent}, {"recovery_swaps", id}, {"funding_fees", "swap/" + id}} {
		if _, found, err := v.ReadArchive(key[0], key[1]); err != nil || !found {
			t.Fatal("failed save lost original cold owner", key, err)
		}
	}
	e = reopenedFixtureEngine(t, e, v, saved)
	if activated, err := e.activateArchived("swaps", id); err != nil || !activated {
		t.Fatal("original complete cold checkpoint could not retry activation", err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Swaps[id] == nil || saved.FillRecords[id] == nil || saved.ParentOrders[parent] == nil || !saved.Recovery.Swaps[id] {
		t.Fatal("successful retry did not publish all companions together")
	}
	for _, key := range [][2]string{{"swaps", id}, {"fill_records", id}, {"parent_orders", parent}, {"recovery_swaps", id}, {"funding_fees", "swap/" + id}} {
		if _, found, err := v.ReadArchive(key[0], key[1]); err != nil || found {
			t.Fatal("successful retry retained duplicate cold ownership", key, err)
		}
	}
}
