package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// A rejected local taker still retains its exact current request identity.
// This fixture never grants a maker allocation or creates signed funding.
func archiveRejectedChild(t *testing.T, e *Engine) *Swap {
	t.Helper()
	maker := nostr.Generate()
	o := protocol.Offer{Version: protocol.Version, Network: chain.Regtest, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Revision: 1, Available: 100000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 100000, Max: 100000}, Status: "open", Expires: time.Now().Unix() + 3600}
	raw, _ := o.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", o.Network.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	id := transport.RandomID()
	keys, err := e.swapKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	r := protocol.Request{Version: protocol.Version, ID: id, Revision: o.Revision, Quantity: o.SellAmount, OfferEvent: event, Taker: e.identity.Public().Hex(), Hash: transport.RandomID(), Keys: keys}
	if _, err := r.Validate(time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return &Swap{ID: id, Role: "taker", Request: r, Stage: "rejected"}
}

func TestArchiveBatchPreservesRestoredRefundAuthority(t *testing.T) {
	e, s, b, _ := isolatedFixture(t, "maker")
	for i := 0; i < 63; i++ {
		child := archiveRejectedChild(t, e)
		e.s.Swaps[child.ID] = child
		e.s.FundingFees["swap/"+child.ID] = FeeSelection{FundingFee: 2000}
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
	e, s, _, _ := isolatedFixture(t, "maker", FeeSelection{FundingFee: 3456, OwnerFeeCap: 20000})
	markRestored(t, e)
	feeKey := "swap/" + s.ID
	parentKey := "offer/" + s.Terms.Offer().ID
	e.s.FundingFees[parentKey] = FeeSelection{FundingFee: 2000}
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
	if !e.restoredSwap(s.ID) || e.s.FundingFees[feeKey].FundingFee != 3456 || e.s.FundingFees[parentKey].FundingFee != 2000 {
		t.Fatal("direct mailbox activation omitted restore/fee policy")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	var restored State
	if _, err := e.vault.Load(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Recovery.Swaps[s.ID] || restored.FundingFees[feeKey].OwnerFeeCap != 20000 {
		t.Fatal("policy companions did not share durable checkpoint")
	}
}

func TestArchiveActivationCompanionErrorsLeaveCoreCold(t *testing.T) {
	for _, kind := range []string{"swaps", "sends", "tower_jobs"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := receiveEngine(t)
			var value any
			id := "core"
			switch kind {
			case "swaps":
				child := archiveRejectedChild(t, e)
				id, value = child.ID, child
				e.s.FundingFees = map[string]FeeSelection{"swap/" + id: {FundingFee: 2000}}
			case "sends":
				value = &WalletSend{PublicSend: PublicSend{ID: "core", Chain: chain.BTC}, Raw: "saved"}
			case "tower_jobs":
				value = &TowerJob{Job: protocol.Job{Version: protocol.Version}}
			}
			raw, _ := json.Marshal(value)
			core := storage.ArchiveRecord{Kind: kind, ID: id, Data: raw}
			marker := storage.ArchiveRecord{Kind: "recovery_" + kind, ID: id, Data: json.RawMessage(`"invalid origin flag"`)}
			// Authenticated storage accepts JSON; the authority loader must reject a
			// structurally wrong boolean before the corresponding core can execute.
			e.s.Version = StateVersion
			e.s.Capacity = &CapacityRecord{Archived: storage.ArchiveStats{Count: 2, Kinds: map[string]uint64{kind: 1, marker.Kind: 1}}}
			for _, r := range []storage.ArchiveRecord{core, marker} {
				b, _ := json.Marshal(r)
				e.s.Capacity.Archived.Bytes += uint64(len(b) + 1)
			}
			if _, err := e.vault.CommitArchive(e.s, storage.ArchiveBatch{Put: []storage.ArchiveRecord{core, marker}}, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := e.activateArchived(kind, id); err == nil || !strings.Contains(err.Error(), "invalid archived recovery record") {
				t.Fatal("malformed cold origin was not refused at the recovery-record boundary", err)
			}
			if e.s.Swaps[id] != nil || e.s.Sends[id] != nil || e.s.TowerJobs[id] != nil {
				t.Fatal("core became active before origin validation")
			}
			if _, ok, err := e.vault.ReadArchive(kind, id); err != nil || !ok {
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
			e.s.TowerJobs = map[string]*TowerJob{"tower": {Job: protocol.Job{Version: protocol.Version}, Confirmed: 200}}
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

func TestArchiveActivationRejectsUnsupportedChildFeeCap(t *testing.T) {
	e, s, _, _ := isolatedFixture(t, "taker", FeeSelection{FundingFee: 3456, OwnerFeeCap: 20000})
	owner := "swap/" + s.ID
	for _, key := range []storage.ArchiveKey{{Kind: "swaps", ID: s.ID}, {Kind: "funding_fees", ID: owner}} {
		if err := e.stageArchive(key.Kind, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	// Normal archival now refuses unsupported policy before committing it.
	// Inject the malformed cold reader reply to exercise activation's separate
	// guard without weakening the writer or modifying the committed source.
	e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
		record, found, err := e.vault.ReadArchive(kind, id)
		if err == nil && found && kind == "funding_fees" && id == owner {
			record.Data, err = json.Marshal(FeeSelection{FundingFee: 3456, OwnerFeeCap: 5000})
		}
		return record, found, err
	}
	before := protocol.Digest(e.s)
	if activated, err := e.activateArchived("swaps", s.ID); err == nil || activated || !strings.Contains(err.Error(), "exact retained child funding fee") {
		t.Fatal("unsupported saved fee cap acquired current authority", err)
	}
	var fee FeeSelection
	if found, err := e.archivedValue("funding_fees", owner, &fee); err != nil || !found || fee.OwnerFeeCap != 5000 || fee.FundingFee != 3456 || protocol.Digest(e.s) != before || e.s.Swaps[s.ID] != nil {
		t.Fatal("refusal changed retained core or exact fee evidence", err)
	}
	e.archiveRead = nil
	if activated, err := e.activateArchived("swaps", s.ID); err != nil || !activated {
		t.Fatal("unchanged supported source could not activate", activated, err)
	}

}
