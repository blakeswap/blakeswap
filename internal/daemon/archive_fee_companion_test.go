package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Rebuild only synthetic fixture authorization before using the selected fee.
// The exact accepted request, principals, child inputs and funded contracts stay
// unchanged; this is not an edit to a running parent's production policy.
func setArchiveMakerFee(t *testing.T, e *Engine, swap *Swap, policy FeeSelection) {
	t.Helper()
	o := swap.Terms.Offer()
	old := e.s.FillRecords[swap.ID]
	fields := FillOrderFields{FillPolicy: o.FillPolicy, FeeBudgets: map[chain.ID]int64{o.Sell: policy.FundingFee + 20000, o.Sell.Other(): max(policy.OwnerFeeCap, int64(2000))}, BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}}
	parent, err := newParentOrder(o, policy, fields, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	reserved, child, err := parent.reserveFill(swap.Request)
	if err != nil {
		t.Fatal(err)
	}
	child.Inputs = append([]CoinOutpoint{}, old.Inputs...)
	committed, allocation, err := reserved.transitionFill(*child, FillCommitted, false)
	if err != nil {
		t.Fatal(err)
	}
	e.s.ParentOrders[o.ID], e.s.FillRecords[swap.ID] = &committed, &allocation
	e.s.FundingFees["swap/"+swap.ID] = policy
	swap.OwnerFeeCap = policy.OwnerFeeCap
}

// Publish the fixture's exact conserved parent projection without rewriting
// the signed source event embedded in an accepted child request.
func stageArchiveParent(t *testing.T, e *Engine, swap *Swap) nostr.Event {
	t.Helper()
	parent := e.s.ParentOrders[swap.Terms.Offer().ID]
	if parent.Offer.Maker != e.identity.Public().Hex() {
		t.Fatal("fixture parent is not owned")
	}
	offer := parentPublicOffer(*parent)
	event, err := e.signOffer(offer, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	parent.SignedRevision, parent.LastSignedAt = parent.Quantities.Revision, int64(event.CreatedAt)
	e.stageOffer(offer, event)
	return event
}

func recentRefundFeeFixture(t *testing.T) (*Engine, *Swap, map[chain.ID]map[string]chain.Observation) {
	t.Helper()
	e, swap, backend, secret := isolatedFixture(t, "maker")
	e.Config.Name = "funded-order"
	e.Config.Mode = "trader"
	offer := swap.Terms.Offer()
	setArchiveMakerFee(t, e, swap, FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000})
	stageArchiveParent(t, e, swap)
	e.s.Outbox = map[string]*Delivery{}
	// A stale parent fee cache is not this child's accepted authorization.
	e.s.FundingFees["offer/"+offer.ID] = FeeSelection{FundingFee: 2000}
	swap.Stage = "waiting for refunds"
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	for _, c := range []contract.HTLC{swap.Long, swap.Short} {
		obs := recoverySpend(t, e, swap, c, true, secret)
		obs.Height = 1
		obs.Confirmations = 2
		all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = obs
		e.nodes[c.Chain] = &sendBackend{receiveBackend: backend.receiveBackend, transaction: func(_ context.Context, txid string) (chain.Transaction, error) {
			for _, raw := range []string{swap.LongFunding, swap.ShortFunding} {
				tx, _ := contract.Parse(raw)
				if tx.TxHash().String() == txid {
					return chain.Transaction{TxID: txid, Hex: raw, Height: 1, Confirmations: 500}, nil
				}
			}
			return chain.Transaction{}, &chain.RPCError{Code: -5}
		}}
		e.chainFresh[c.Chain] = true
		e.archiveCurrent[c.Chain] = recoveryCheckpoint{Height: 500, Hash: "test-canonical-tip"}
	}
	if err := e.advanceSwap(context.Background(), swap, all); err != nil {
		t.Fatal(err)
	}
	if swap.Stage != "refunded" {
		t.Fatal("actual refund progression missing", swap.Stage)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if e.finishedOrder(offer.ID) != "refunded" {
		t.Fatal("initial completed maker is not recognized")
	}
	policy := protocol.Tower{PubKey: e.identity.Public().Hex(), BPS: 37, Network: chain.Regtest, Scripts: map[chain.ID]string{chain.BTC: "0014-retained-policy"}}
	e.s.OfferTowers = map[string]protocol.Tower{offer.ID: policy}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, swap, all
}

func assertHotRefundFee(t *testing.T, e *Engine, swap *Swap) {
	t.Helper()
	if got := e.publicSwap(swap).FundingFee; got != 6500 {
		t.Fatalf("hot child fee=%d want6500", got)
	}
	a, present := e.s.Activities[activityID("swap", swap.ID)+"/funding"]
	if !present || !a.FeeKnown || a.Fee != 6500 || a.Amount != swap.Short.Amount+6500 {
		t.Fatal("hot funding activity lost exact selected fee", a)
	}
}
func TestRecentRefundRetainsFeeUntilChildArchives(t *testing.T) {
	e, swap, all := recentRefundFeeFixture(t)
	offerID := swap.Terms.Offer().ID
	original := e.s.Offers[offerID]
	before := BackupSemanticToken(e.s)
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if e.s.Swaps[swap.ID] == nil || e.s.Offers[offerID].Content != "" {
		t.Fatal("recent child or terminal parent has the wrong archive placement")
	}
	var retained nostr.Event
	if found, err := e.archivedValue("offers", offerID, &retained); err != nil || !found || retained.ID != original.ID {
		t.Fatal("terminal parent lost its exact retained identity", err)
	}
	assertHotRefundFee(t, e, swap)
	if BackupSemanticToken(e.s) != before {
		t.Fatal("location-only compaction changed fee economics/freshness")
	}
	swap.LongConfirmations, swap.ShortConfirmations = 6, 6
	if err := e.CanChangeNetwork(); err != nil {
		t.Fatal("retained fee gave a settled child a new network hold", err)
	}
	swap.Stage = "awaiting chain confirmations"
	if err := e.CanChangeNetwork(); err == nil {
		t.Fatal("reorg-demoted child lost its existing network hold")
	}
	swap.Stage = "refunded"
	for id, observations := range all {
		for key, obs := range observations {
			obs.Confirmations = 500
			all[id][key] = obs
		}
	}
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if e.s.Swaps[swap.ID] != nil || e.s.Offers[offerID].Content != "" {
		t.Fatal("deep child/parent did not archive")
	}
	var fee FeeSelection
	if found, err := e.archivedValue("funding_fees", "swap/"+swap.ID, &fee); err != nil || !found || fee.FundingFee != 6500 {
		t.Fatal("last child lost retained fee", err)
	}
}
func splitRefundFeeFixture(t *testing.T, e *Engine, swap *Swap) {
	t.Helper()
	offerID := swap.Terms.Offer().ID
	// Model a current-format hot core whose exact child fee is already cold;
	// startup repair must point-read it without promoting the parent publisher.
	for _, key := range []storage.ArchiveKey{{Kind: "offers", ID: offerID}, {Kind: "order_records", ID: offerID}, {Kind: "offer_towers", ID: offerID}, {Kind: "funding_fees", ID: "swap/" + swap.ID}} {
		if err := e.stageArchive(key.Kind, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
}
func TestPriorSplitFeeRepairsWithoutPublisherAndFollowsLastChild(t *testing.T) {
	e, swap, all := recentRefundFeeFixture(t)
	offerID := swap.Terms.Offer().ID
	splitRefundFeeFixture(t, e, swap)
	if err := e.restoreActiveFundingFees(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	assertHotRefundFee(t, e, swap)
	if e.s.Offers[offerID].Content != "" || len(e.s.Outbox) != 0 {
		t.Fatal("fee repair restored old publication authority")
	}
	// Missing new fork evidence keeps the recent obligation and fee together.
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	assertHotRefundFee(t, e, swap)
	for id, observations := range all {
		for key, obs := range observations {
			obs.Confirmations = 500
			all[id][key] = obs
		}
	}
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if e.s.Swaps[swap.ID] != nil {
		t.Fatal("deep repaired child remains hot")
	}
	if _, hot := e.s.FundingFees["swap/"+swap.ID]; hot {
		t.Fatal("orphan repaired fee remains hot after last child")
	}
	if _, err := e.activateArchived("swaps", swap.ID); err != nil {
		t.Fatal(err)
	}
	swap = e.s.Swaps[swap.ID]
	swap.Stage = "awaiting chain confirmations"
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.compactArchive(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	assertHotRefundFee(t, e, swap)
	if e.s.Offers[offerID].Content != "" {
		t.Fatal("reorg recreated a publisher")
	}
}

func TestRecentRefundStoredNetworkGuardTracksChildOutcome(t *testing.T) {
	for _, stage := range []string{"refunded", "awaiting chain confirmations"} {
		t.Run(stage, func(t *testing.T) {
			e, swap, all := recentRefundFeeFixture(t)
			if err := e.compactArchive(context.Background(), all, nil); err != nil {
				t.Fatal(err)
			}
			swap.Stage = stage
			swap.LongConfirmations, swap.ShortConfirmations = 6, 6
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			root := e.vault.PrivateDirectory()
			if err := e.vault.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Network: chain.Regtest, DataDir: root, CredentialMode: "native", Credential: credential.SourceFunc(func(context.Context) ([]byte, error) { return []byte("receive-test-password"), nil })}
			err := CheckStoredNetwork(cfg)
			if stage == "refunded" && err != nil {
				t.Fatal("stored terminal parent introduced a new network hold", err)
			}
			if stage != "refunded" && err == nil {
				t.Fatal("stored reorg-demoted child lost its network hold")
			}
		})
	}
}

func TestOpenRepairsPriorSplitFundingFeeBeforeActivityProjection(t *testing.T) {
	e, swap, _ := recentRefundFeeFixture(t)
	splitRefundFeeFixture(t, e, swap)
	root := e.vault.PrivateDirectory()
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: e.Config.Name, Mode: "trader", Network: chain.Regtest, DataDir: root, Relays: []string{"ws://127.0.0.1:1"}, CredentialMode: "native", Credential: credential.SourceFunc(func(context.Context) ([]byte, error) { return []byte("receive-test-password"), nil }), Nodes: map[chain.ID]NodeConfig{}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		cfg.Nodes[id] = NodeConfig{Kind: "rpc", URL: "http://127.0.0.1:1", Cookie: filepath.Join(root, "absent-test-cookie")}
	}
	reopened, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal("prior native profile failed to reopen", err)
	}
	defer reopened.Close()
	assertHotRefundFee(t, reopened, reopened.s.Swaps[swap.ID])
	if reopened.s.Offers[swap.Terms.Offer().ID].Content != "" || len(reopened.s.Outbox) != 0 {
		t.Fatal("startup repair recreated a publisher")
	}
	var durable State
	if _, err = reopened.vault.Load(&durable); err != nil {
		t.Fatal(err)
	}
	if durable.FundingFees["swap/"+swap.ID].FundingFee != 6500 {
		t.Fatal("startup repair was not committed")
	}
}
func TestOpenRejectsUnreadableSplitFeeBeforeConsumers(t *testing.T) {
	e, swap, _ := recentRefundFeeFixture(t)
	splitRefundFeeFixture(t, e, swap)
	owner := "swap/" + swap.ID
	old, found, err := e.vault.ReadArchive("funding_fees", owner)
	if err != nil || !found {
		t.Fatal(err)
	}
	bad := storage.ArchiveRecord{Kind: "funding_fees", ID: owner, Data: json.RawMessage(`{"funding_fee":"invalid"}`)}
	if err = e.archiveDelta(old, false); err != nil {
		t.Fatal(err)
	}
	if err = e.archiveDelta(bad, true); err != nil {
		t.Fatal(err)
	}
	// Archive identities are immutable. Build the malformed initial checkpoint
	// in a separate vault instead of replacing already committed evidence.
	var source State
	records, _, err := e.vault.LoadComplete(&source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if records[i].Kind == bad.Kind && records[i].ID == bad.ID {
			records[i] = bad
		}
	}
	root := t.TempDir()
	fixture, err := storage.Open(filepath.Join(root, "state.db"), []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.CommitArchive(e.s, storage.ArchiveBatch{Put: records}, 0); err != nil {
		_ = fixture.Close()
		t.Fatal(err)
	}
	if err = fixture.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(e.s)
	cfg := Config{Name: e.Config.Name, Mode: "trader", Network: chain.Regtest, DataDir: root, Relays: []string{"ws://127.0.0.1:1"}, CredentialMode: "native", Credential: credential.SourceFunc(func(context.Context) ([]byte, error) { return []byte("receive-test-password"), nil })}
	opened, err := Open(context.Background(), cfg)
	if opened != nil {
		opened.Close()
		t.Fatal("unreadable fee opened")
	}
	if err == nil || !strings.Contains(err.Error(), "cannot restore retained funding fee") {
		t.Fatal("startup did not refuse fee evidence before consumers", err)
	}
	vault, err := storage.Open(filepath.Join(root, "state.db"), []byte("receive-test-password"))
	if err != nil {
		t.Fatal("rejection retained vault lock", err)
	}
	defer vault.Close()
	var after State
	if _, err = vault.Load(&after); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(after)
	if !bytes.Equal(before, raw) {
		t.Fatal("failed repair changed durable state")
	}
	retained, found, err := vault.ReadArchive("funding_fees", owner)
	if err != nil || !found || !bytes.Equal(retained.Data, bad.Data) {
		t.Fatal("failed repair discarded unreadable evidence", err)
	}
}
func TestFeeCompanionReadErrorDoesNotInventLegacyDefault(t *testing.T) {
	e, swap, _ := recentRefundFeeFixture(t)
	delete(e.s.FundingFees, "swap/"+swap.ID)
	readErr := errors.New("isolated archive unavailable")
	e.archiveRead = func(string, string) (storage.ArchiveRecord, bool, error) {
		return storage.ArchiveRecord{}, false, readErr
	}
	if err := e.restoreActiveFundingFees(); !errors.Is(err, readErr) {
		t.Fatal("unavailable companion became legacy fee", err)
	}
}
