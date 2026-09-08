package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func appendPortableRecord(t *testing.T, state *daemon.State, kind, id string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	record := storage.ArchiveRecord{Kind: kind, ID: id, Data: raw}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if state.Capacity == nil {
		state.Capacity = &daemon.CapacityRecord{Archived: storage.ArchiveStats{Kinds: map[string]uint64{}}}
	}
	state.Version = 2
	state.Archive = append(state.Archive, record)
	state.Capacity.Archived.Count++
	state.Capacity.Archived.Bytes += uint64(len(encoded) + 1)
	state.Capacity.Archived.Kinds[kind]++
}

func TestStreamedRestorePromotesCoreAndKeepsAutomationHistoryCold(t *testing.T) {
	ctx := context.Background()
	manifest := portableManifest(t)
	entry := &manifest.Wallets[0]
	state := entry.Networks[chain.Regtest]
	entry.Networks = map[chain.Network]*daemon.State{chain.Regtest: state}
	appendPortableRecord(t, state, "swaps", "swap", state.Swaps["swap"])
	delete(state.Swaps, "swap")
	appendPortableRecord(t, state, "funding_fees", "swap", daemon.FeeSelection{FundingFee: 3456})
	appendPortableRecord(t, state, "recovery_swaps", "swap", true)
	appendPortableRecord(t, state, "activities", "receive/known", state.Activities["receive/known"])
	delete(state.Activities, "receive/known")
	appendPortableRecord(t, state, "activity_receipts", "transaction", state.ActivityReceipts["transaction"])
	delete(state.ActivityReceipts, "transaction")
	appendPortableRecord(t, state, "order_records", "old", daemon.OrderRecord{Publication: "acknowledged", CancelledEventID: "cancel-exact", ReplacedBy: "source"})
	appendPortableRecord(t, state, "offers", "old", nostr.Event{Content: "saved signed source"})
	appendPortableRecord(t, state, "outbox", "message", &daemon.Delivery{Acknowledged: true})
	appendPortableRecord(t, state, "seen", "sender/message", "exact-digest")
	appendPortableRecord(t, state, "seen_semantics", "canonical-evidence", true)
	appendPortableRecord(t, state, "trade_receipts", "accepted", &daemon.TradeReceipt{Digest: "accepted-exact", Result: daemon.ConfirmTradeResult{ID: "accepted", State: "accepted"}})
	state.Offers = map[string]nostr.Event{"source": {Content: "current signed source"}}
	state.Automations = map[string]*daemon.AutomationPolicy{"policy": {Config: daemon.AutomationConfig{ID: "policy"}, Enabled: true, Revision: 5, CurrentOfferID: "source", Charges: map[string]*daemon.AutomationCharge{
		"old":    {OfferID: "old", Volume: 100, State: "committed", Successor: "source"},
		"source": {OfferID: "source", Volume: 100, State: "reserved"},
	}, Pending: &daemon.ConfirmTradeRequest{RequestID: "pending"}}}
	state.TradeReceipts = map[string]*daemon.TradeReceipt{"pending": {Digest: "pending-exact", Result: daemon.ConfirmTradeResult{ID: "pending", State: "pending"}}}
	source := filepath.Join(t.TempDir(), "complete.backup")
	password := []byte("separately chosen archive password")
	if err := writeStreamManifest(ctx, source, password, manifest); err != nil {
		t.Fatal(err)
	}
	m := installedManager(t)
	result, err := m.importPortable(ctx, portableImportRequest{Path: source, Password: string(password), Revision: m.settings.Revision})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(m.root, "wallets", result.ProfileID)
	_, secret, err := readMaster(root)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	vault, err := storage.Open(filepath.Join(root, "regtest", "state.db"), secret)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	var got daemon.State
	if _, err = vault.Load(&got); err != nil {
		t.Fatal(err)
	}
	if got.Swaps["swap"] == nil || got.Swaps["swap"].SelfRefunds[0] != "saved refund" || got.FundingFees["swap"].FundingFee != 3456 || !got.Recovery.Swaps["swap"] || got.Recovery.Status.State != "recovering" {
		t.Fatal("promoted core lost signed evidence/fees/recovery gate")
	}
	if len(got.Activities) != 0 || len(got.ActivityReceipts) != 0 || len(got.OrderRecords) != 0 || len(got.Seen) != 0 || got.TradeReceipts["accepted"] != nil {
		t.Fatal("cold history was accumulated into active checkpoint")
	}
	if len(got.Offers) != 0 || len(got.Outbox) != 0 || got.Recovery.Offers["source"].Content != "current signed source" || got.Recovery.Status.QuarantinedOffers != 2 || got.Recovery.Status.QuarantinedMessages != 1 {
		t.Fatal("old/current publications regained authority or quarantine count lost")
	}
	policy := got.Automations["policy"]
	if policy.Enabled || !policy.RestoreHold || policy.Revision <= 5 || policy.Pending != nil || policy.Charges["old"].State != "committed" || policy.Charges["old"].Successor != "source" || !policy.Charges["source"].Uncertain || got.TradeReceipts["pending"].Digest != "pending-exact" {
		t.Fatal("import resumed automation or reset charged authorization")
	}
	for _, key := range [][2]string{{"activities", "receive/known"}, {"activity_receipts", "transaction"}, {"order_records", "old"}, {"quarantined_offers", "old"}, {"quarantined_outbox", "message"}, {"seen", "sender/message"}, {"seen_semantics", "canonical-evidence"}, {"trade_receipts", "accepted"}} {
		if _, ok, err := vault.ReadArchive(key[0], key[1]); err != nil || !ok {
			t.Fatal("cold evidence missing", key, err)
		}
	}
	for _, key := range [][2]string{{"swaps", "swap"}, {"funding_fees", "swap"}, {"recovery_swaps", "swap"}, {"offers", "old"}, {"outbox", "message"}} {
		if _, ok, err := vault.ReadArchive(key[0], key[1]); err != nil || ok {
			t.Fatal("duplicate core or live archived authority", key, err)
		}
	}
}

func TestPortableStartupCleanupRetainsPublishedRecoveryProfiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".portable-snapshot-dead", ".restore-dead.db", "wallets/.import-dead", "wallets/wallet-published"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "retained"), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupPortableStaging(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "wallets/wallet-published/retained")); err != nil {
		t.Fatal("published recovery profile removed", err)
	}
	for _, name := range []string{".portable-snapshot-dead", ".restore-dead.db", "wallets/.import-dead"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("private abandoned stage retained", name, err)
		}
	}
}
