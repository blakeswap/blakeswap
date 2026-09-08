package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestArchiveBatchPreservesRestoredRefundAuthority(t *testing.T) {
	e, s, b, _ := isolatedFixture(t, "maker")
	for i := 0; i < 63; i++ {
		id := fmt.Sprintf("inactive-%02d", i)
		e.s.Swaps[id] = &Swap{ID: id, Role: "maker", Stage: "rejected"}
	}
	markRestored(t, e)
	own := s.Short
	script, _ := own.PkScript()
	b.coins = []chain.UTXO{{TxID: own.TxID, Vout: own.Vout, Amount: chain.Coins(own.Amount), Script: hex.EncodeToString(script), Confirmations: 2}}
	b.transaction = func(context.Context, string) (chain.Transaction, error) {
		return chain.Transaction{TxID: own.TxID, Hex: s.ShortFunding, Confirmations: 2}, nil
	}
	e.nodes[own.Chain] = b
	calls := 0
	b.broadcast = func(raw string) (string, error) {
		calls++
		tx, err := contract.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, claimed := contract.ExtractSecret(own, tx); claimed {
			t.Fatal("unexpected claim")
		}
		return tx.TxHash().String(), nil
	}
	empty := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	_ = e.advanceSwap(context.Background(), s, empty)
	if calls != 0 {
		t.Fatal("baseline restored refund was not held")
	}
	settled := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		settled[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, true, nil)
	}
	if err := e.advanceSwap(context.Background(), s, settled); err != nil || s.Stage != "refunded" {
		t.Fatal(err, s.Stage)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	for id := range e.s.Swaps {
		if err := e.stageArchive("swaps", id); err != nil {
			t.Fatal(err)
		}
		if err := e.stageArchive("recovery_swaps", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.s.Capacity.Reactivating = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.reactivateArchive(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	s = e.s.Swaps[s.ID]
	if s == nil {
		t.Fatal("fixture did not activate target")
	}
	var marker bool
	found, err := e.archivedValue("recovery_swaps", s.ID, &marker)
	t.Logf("first batch restored=%v coldMarker=%v/%v err=%v reactivating=%v", e.restoredSwap(s.ID), found, marker, err, e.s.Capacity.Reactivating)
	err = e.advanceSwap(context.Background(), s, empty)
	if calls != 0 {
		t.Fatalf("reactivation published %d refund without a positively confirmed incoming refund (advanceErr=%v stage=%s restored=%v)", calls, err, s.Stage, e.restoredSwap(s.ID))
	}
	if !e.restoredSwap(s.ID) {
		t.Fatal("active obligation temporarily lost its imported authority marker")
	}
}

func TestArchiveDirectActivationLoadsOriginAndMakerFeeBeforeCore(t *testing.T) {
	e, s, _, _ := isolatedFixture(t, "maker")
	markRestored(t, e)
	feeKey := "offer/" + s.Terms.Offer().ID
	e.s.FundingFees = map[string]FeeSelection{feeKey: {FundingFee: 3456, OwnerFeeCap: 5000}}
	for _, key := range []storage.ArchiveKey{{Kind: "swaps", ID: s.ID}, {Kind: "recovery_swaps", ID: s.ID}, {Kind: "funding_fees", ID: feeKey}} {
		if err := e.stageArchive(key.Kind, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.activateArchived("swaps", s.ID); err != nil {
		t.Fatal(err)
	}
	if !e.restoredSwap(s.ID) || e.s.FundingFees[feeKey].FundingFee != 3456 {
		t.Fatal("direct mailbox activation omitted restore/fee policy")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var restored State
	if _, err := e.vault.Load(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Recovery.Swaps[s.ID] || restored.FundingFees[feeKey].OwnerFeeCap != 5000 {
		t.Fatal("policy companions did not share durable checkpoint")
	}
}

func TestArchiveActivationCompanionErrorsLeaveCoreCold(t *testing.T) {
	for _, kind := range []string{"swaps", "sends", "tower_jobs"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := receiveEngine(t)
			var value any
			switch kind {
			case "swaps":
				value = &Swap{ID: "core", Role: "maker"}
			case "sends":
				value = &WalletSend{PublicSend: PublicSend{ID: "core", Chain: chain.BTC}, Raw: "saved"}
			case "tower_jobs":
				value = &TowerJob{}
			}
			raw, _ := json.Marshal(value)
			core := storage.ArchiveRecord{Kind: kind, ID: "core", Data: raw}
			marker := storage.ArchiveRecord{Kind: "recovery_" + kind, ID: "core", Data: json.RawMessage(`"invalid origin flag"`)}
			// Authenticated storage accepts JSON; the authority loader must reject a
			// structurally wrong boolean before the corresponding core can execute.
			e.s.Version = 2
			e.s.Capacity = &CapacityRecord{Archived: storage.ArchiveStats{Count: 2, Kinds: map[string]uint64{kind: 1, marker.Kind: 1}}}
			for _, r := range []storage.ArchiveRecord{core, marker} {
				b, _ := json.Marshal(r)
				e.s.Capacity.Archived.Bytes += uint64(len(b) + 1)
			}
			if _, err := e.vault.CommitArchive(e.s, storage.ArchiveBatch{Put: []storage.ArchiveRecord{core, marker}}, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := e.activateArchived(kind, "core"); err == nil {
				t.Fatal("malformed cold origin accepted")
			}
			if e.s.Swaps["core"] != nil || e.s.Sends["core"] != nil || e.s.TowerJobs["core"] != nil {
				t.Fatal("core became active before origin validation")
			}
			if _, ok, err := e.vault.ReadArchive(kind, "core"); err != nil || !ok {
				t.Fatal("failed activation lost cold core", err)
			}
		})
	}
}

func TestArchiveActivationPairsSendAndTowerOriginsWithoutInventingThem(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			e, _ := receiveEngine(t)
			e.s.Sends = map[string]*WalletSend{"send": {PublicSend: PublicSend{ID: "send", Chain: chain.BTC, Confirmations: 200}, Raw: "retained"}}
			e.s.TowerJobs = map[string]*TowerJob{"tower": {Confirmed: 200}}
			if restored {
				markRestored(t, e)
			}
			for _, key := range []storage.ArchiveKey{{Kind: "sends", ID: "send"}, {Kind: "tower_jobs", ID: "tower"}, {Kind: "recovery_sends", ID: "send"}, {Kind: "recovery_tower_jobs", ID: "tower"}} {
				if err := e.stageArchive(key.Kind, key.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			for _, key := range []storage.ArchiveKey{{Kind: "sends", ID: "send"}, {Kind: "tower_jobs", ID: "tower"}} {
				if _, err := e.activateArchived(key.Kind, key.ID); err != nil {
					t.Fatal(err)
				}
			}
			if restored && (e.s.Recovery == nil || !e.s.Recovery.Sends["send"] || !e.s.Recovery.TowerJobs["tower"]) {
				t.Fatal("send/tower became active without original restriction")
			}
			if !restored && e.s.Recovery != nil {
				t.Fatal("non-restored obligation acquired an invented import restriction")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
