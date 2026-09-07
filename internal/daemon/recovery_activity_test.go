package daemon

import (
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
)

func TestRestoredActivityRetainsAuditAndUsesCurrentExportContext(t *testing.T) {
	txid := strings.Repeat("a", 64)
	origin := &Engine{Config: Config{Name: "original-profile", Network: chain.Regtest}, s: State{Version: 1, Network: chain.Regtest, ActivityReceipts: map[string]ReceiptEvidence{txid: {Inputs: []CoinOutpoint{{TxID: "parent", Vout: 3}}, Total: 700, OwnedTotal: 500}}, ActivityIndexes: map[chain.ID]ActivityIndex{chain.BTC: {Address: 7, After: "old-cursor", CompletedPass: 100, Source: "old-source", Generation: 1}}}}
	origin.putActivity(Activity{ID: "receive/known", Kind: "receive", Chain: chain.BTC, TxID: txid, Variants: []string{txid}, Status: "confirmed", Observations: []ActivityObservation{{TxID: txid, Status: "confirmed", Confirmations: 6, Height: 10, BlockHash: "old-block", ObservedAt: time.Now().Unix(), Source: "old-source", Generation: 1}}}, true)
	origin.putActivity(Activity{ID: "order/old", Kind: "order", Chain: chain.BTC, Status: "cancelled", LocalStatus: "cancelled"}, true)
	snapshot, err := origin.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = PrepareRecovery(&snapshot, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Config: Config{Name: "new-isolated-profile", Network: chain.Regtest}, s: snapshot}
	e.invalidateActivitySession()
	if !reflect.DeepEqual(e.s.ActivityReceipts, origin.s.ActivityReceipts) {
		t.Fatal("restore lost receipt classification evidence")
	}
	stored := e.s.Activities["receive/known"]
	if stored.Status != "unknown" || stored.Confirmations != 0 || stored.ObservedAt != 0 || len(stored.History) != 1 || stored.History[0].Status != "confirmed" || stored.History[0].Source != "old-source" || stored.Observations[0].BlockHash != "old-block" {
		t.Fatal("restore reused stale proof or discarded audit history", stored)
	}
	if stored.Wallet != "original-profile" {
		t.Fatal("restore overwrote stored audit origin", stored.Wallet)
	}
	if index := e.s.ActivityIndexes[chain.BTC]; index.Address != 0 || index.After != "" || index.CompletedPass != 0 {
		t.Fatal("restore reused imported coverage", index)
	}
	q := ActivityQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest"}
	page := queryActivity(t, e, q)
	for _, a := range page.Records {
		if a.Wallet != e.Config.Name {
			t.Fatal("exported activity has the old profile context", a)
		}
	}
	raw, _ := json.Marshal(q)
	exported, err := e.exportActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(exported.CSV)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	walletColumn := -1
	for i, name := range rows[0] {
		if name == "wallet" {
			walletColumn = i
		}
	}
	if walletColumn < 0 {
		t.Fatal("CSV has no wallet scope")
	}
	for _, row := range rows[1:] {
		if row[walletColumn] != e.Config.Name {
			t.Fatal("CSV retained source profile context", row)
		}
	}
	if e.s.Activities["order/old"].Wallet != "original-profile" || e.s.Activities["receive/known"].Wallet != "original-profile" {
		t.Fatal("public projection rewrote stored origins")
	}
}

func TestRestoredLegacyOrdersRemainAuditableWithoutRepublication(t *testing.T) {
	for _, status := range []string{"cancelled", "open"} {
		t.Run(status, func(t *testing.T) {
			e, p, _ := managedSource(t)
			offer := e.s.OrderRecords[p.SourceOfferID].Offer
			offer.Status = status
			offer.Expires = time.Now().Unix() - 1
			if err := e.publishOffer(offer); err != nil {
				t.Fatal(err)
			}
			event := e.s.Offers[offer.ID]
			e.s.OrderRecords, e.s.Activities = nil, nil
			if err := PrepareRecovery(&e.s, time.Now().Unix(), true); err != nil {
				t.Fatal(err)
			}
			e.syncOrderRecords()
			e.syncActivity()
			got, ok := e.s.Activities[activityID("order", offer.ID)]
			expected := status
			if status == "open" {
				expected = "quarantined"
			}
			if !ok || got.Status != expected || got.CreatedAt != 0 || got.CreatedSource != "unknown" || got.OrderID != offer.ID {
				t.Fatal("legacy quarantine lost order audit or invented time/expiry", got)
			}
			count := len(got.History)
			for i := 0; i < 3; i++ {
				e.syncActivity()
			}
			if len(e.s.Offers) != 0 || len(e.s.Outbox) != 0 || e.s.Recovery.Offers[offer.ID].Content != event.Content || len(e.s.Activities[got.ID].History) != count {
				t.Fatal("quarantine was republished, lost, or duplicated by polling")
			}
		})
	}
}

func TestBackupFingerprintActivityPollingAndMeaningfulEvidence(t *testing.T) {
	state := State{ActivityRevision: 1, ActivityObservationSequence: 1, Activities: map[string]Activity{"record": {ID: "record", Status: "confirmed", Confirmations: 2, ObservedAt: 10, UpdatedAt: 10, TxID: "tx", Variants: []string{"tx"}, Observations: []ActivityObservation{{Sequence: 1, TxID: "tx", Status: "confirmed", Confirmations: 2, Height: 5, BlockHash: "block", ObservedAt: 10, Source: "source", Generation: 1}}}}, ActivityIndexes: map[chain.ID]ActivityIndex{chain.BTC: {Address: 1, After: "first", CompletedPass: 10}}, ActivityReceipts: map[string]ReceiptEvidence{"tx": {Total: 1000, OwnedTotal: 800}}}
	before, err := BackupFingerprint(state)
	if err != nil {
		t.Fatal(err)
	}
	record := state.Activities["record"]
	record.Confirmations++
	record.ObservedAt++
	record.UpdatedAt++
	record.Observations[0].Sequence++
	record.Observations[0].Confirmations++
	record.Observations[0].ObservedAt++
	record.Observations[0].Error = "temporary provider diagnostic"
	state.Activities["record"] = record
	state.ActivityRevision++
	state.ActivityObservationSequence++
	state.ActivityIndexes[chain.BTC] = ActivityIndex{Address: 2, After: "next", CompletedPass: 11, Error: "temporary coverage diagnostic"}
	state.ActivityError = "temporary history error"
	after, err := BackupFingerprint(state)
	if err != nil || after != before {
		t.Fatal("routine activity polling changes backup freshness", err)
	}
	for _, change := range []string{"outcome", "replacement", "receipt", "reorg-history"} {
		t.Run(change, func(t *testing.T) {
			raw, _ := json.Marshal(state)
			var next State
			if err := json.Unmarshal(raw, &next); err != nil {
				t.Fatal(err)
			}
			a := next.Activities["record"]
			switch change {
			case "outcome":
				a.Observations[0].Status = "orphaned"
			case "replacement":
				a.Variants = append(a.Variants, "new-signed-variant")
			case "receipt":
				next.ActivityReceipts["tx"] = ReceiptEvidence{Total: 1000, OwnedTotal: 900}
			case "reorg-history":
				a.History = append(a.History, ActivityOutcome{Status: "confirmed", TxID: "tx", BlockHash: "replaced-block", ObservedAt: 10, Source: "source", Generation: 1})
			}
			next.Activities["record"] = a
			changed, err := BackupFingerprint(next)
			if err != nil || changed == before {
				t.Fatal("meaningful activity recovery evidence omitted", err)
			}
		})
	}
}
