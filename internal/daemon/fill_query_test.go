package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func fillHistoryFixture(t *testing.T, count int) (*Engine, FillQuery, []string) {
	t.Helper()
	e, maker, _ := fillAdmissionEngine(t, chain.Blake)
	e.Config.Name = "fills"
	var p *ParentOrder
	for _, parent := range e.s.ParentOrders {
		p = parent
	}
	e.s.FillRecords = map[string]*FillRecord{}
	ids := []string{}
	for i := 0; i < count; i++ {
		r := fillRequestFixture(t, *p, maker, 400000)
		_, f, err := p.reserveFill(r)
		if err != nil {
			t.Fatal(err)
		}
		// Retained returned children have valid original identities and zero
		// current allocation. Their history does not consume the parent again.
		f.Allocation.Disposition, f.FundingDisabled = FillRetired, true
		f.Inputs = []CoinOutpoint{{TxID: transport.RandomID()}}
		e.s.FillRecords[r.ID] = f
		e.s.FundingFees["swap/"+r.ID] = f.FundingPolicy
		e.s.Swaps[r.ID] = &Swap{ID: r.ID, Role: "maker", Request: r, Stage: "expired before maker funding"}
		ids = append(ids, r.ID)
		if i%2 == 0 {
			for _, kind := range []string{"fill_records", "swaps"} {
				if err := e.stageArchive(kind, r.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	return e, FillQuery{ExpectedWallet: "fills", ExpectedNetwork: "regtest", ParentMaker: p.Offer.Maker, ParentID: p.Offer.ID, Limit: 37}, ids
}

func queryFills(t *testing.T, e *Engine, q FillQuery) FillPage {
	t.Helper()
	raw, _ := json.Marshal(q)
	result, err := e.Command(context.Background(), Request{Method: "fills.list", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return result.(FillPage)
}

func TestFillHistoryFrozenColdHotPagesPreserveReturnedAllocation(t *testing.T) {
	e, q, ids := fillHistoryFixture(t, 205)
	before := protocol.Digest(e.s)
	first := queryFills(t, e, q)
	if first.Total != len(ids) || !first.More || len(first.Records) != 37 || first.Records[0].ID != ids[0] {
		t.Fatal("first fill page does not merge complete hot and cold history", first.Total, len(first.Records))
	}
	if historyScratch(t, e) != 1 || protocol.Digest(e.s) != before {
		t.Fatal("history did not own one private result or changed durable authority")
	}
	for _, row := range first.Records {
		if !row.AllocationKnown || row.AllocatedQuantity != 0 || row.Quantity != 400000 || row.BuyAmount != 520001 || row.Disposition != FillRetired {
			t.Fatal("returned child projection lost original quantity or reallocated it", row)
		}
	}
	// Later placement and stage changes cannot rewrite a saved result.
	var changedID string
	for id, s := range e.s.Swaps {
		changedID = id
		s.Stage = "late peer funding"
		if err := e.stageArchive("swaps", id); err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	q.Revision, q.Offset = first.Revision, first.NextOffset
	seen := []string{}
	for _, row := range first.Records {
		seen = append(seen, row.ID)
	}
	for {
		page := queryFills(t, e, q)
		for _, row := range page.Records {
			if row.ID == changedID && row.Stage != "expired before maker funding" {
				t.Fatal("later live changes rewrote a frozen child")
			}
			seen = append(seen, row.ID)
		}
		if !page.More {
			break
		}
		q.Offset = page.NextOffset
	}
	if !reflect.DeepEqual(seen, ids) {
		t.Fatal("fill pages skipped or repeated a child")
	}
	for _, mutate := range []func(*FillQuery){func(q *FillQuery) { q.ParentMaker = transport.RandomID() }, func(q *FillQuery) { q.ParentID = transport.RandomID() }, func(q *FillQuery) { q.ExpectedWallet = "other" }, func(q *FillQuery) { q.ExpectedNetwork = "mainnet" }, func(q *FillQuery) { q.Offset = 999 }, func(q *FillQuery) { q.Limit = 501 }} {
		bad := q
		mutate(&bad)
		raw, _ := json.Marshal(bad)
		if _, err := e.Command(context.Background(), Request{Method: "fills.list", Params: raw}); err == nil {
			t.Fatal("changed parent/context or invalid page reused a fill revision")
		}
	}
	// receiveEngine embeds a deliberately incomplete backend; this test owns
	// query/vault closure, not that fixture's unimplemented network Close.
	e.nodes = nil
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if historyScratch(t, e) != 0 {
		t.Fatal("Close retained private fill result files")
	}
}

func TestFillHistoryTakerNeverInfersRemoteAllocation(t *testing.T) {
	e, q, _ := fillHistoryFixture(t, 1)
	var s Swap
	var id string
	// Use the retained request with its actual local taker identity; no maker
	// ledger is present in a taker wallet, even when the local stage is terminal.
	for _, parent := range e.s.ParentOrders {
		r := fillRequestFixture(t, *parent, e.identity, 400000)
		taker := nostr.Generate()
		r.Taker = taker.Public().Hex()
		e.identity = taker
		s = Swap{ID: r.ID, Request: r, Role: "taker", Stage: "refunded"}
		id = r.ID
		break
	}
	e.s.Swaps = map[string]*Swap{id: &s}
	e.s.ParentOrders, e.s.FillRecords = nil, nil
	// An in-memory view isolates the foreign parent's allocation semantics.
	view := &Engine{Config: e.Config, identity: e.identity, s: e.s}
	page := queryFills(t, view, q)
	if page.Total != 1 || page.Records[0].AllocationKnown || page.Records[0].AllocatedQuantity != 0 || page.Records[0].Disposition != "" || page.Records[0].Quantity != 400000 {
		t.Fatal("taker history inferred a remote allocation", page)
	}
}

func TestFillHistoryFailureAndCancellationDisposeOnlyUnpublishedResults(t *testing.T) {
	e, q, _ := fillHistoryFixture(t, 3)
	first := queryFills(t, e, q)
	e.nodes[chain.BTC] = &reconnectingHistorySource{activityArchiveBackend: &activityArchiveBackend{generation: 1}}
	raw, _ := json.Marshal(q)
	if result, err := e.Command(context.Background(), Request{Method: "fills.list", Params: raw}); err == nil || result != nil {
		t.Fatal("source change published an incoherent fill result")
	}
	if historyScratch(t, e) != 1 || len(e.activitySnapshots) != 1 {
		t.Fatal("failed query leaked new result or discarded an existing revision")
	}
	delete(e.nodes, chain.BTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Command(ctx, Request{Method: "fills.list", Params: raw}); err == nil {
		t.Fatal("canceled fill query succeeded")
	}
	q.Revision = first.Revision
	if page := queryFills(t, e, q); page.Total != 3 {
		t.Fatal("failed query invalidated a complete prior revision")
	}
	for _, child := range e.s.FillRecords {
		child.RequestDigest = transport.RandomID()
		break
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Command(context.Background(), Request{Method: "fills.list", Params: raw}); err == nil {
		t.Fatal("new history silently projected a mismatched allocation companion")
	}
	if page := queryFills(t, e, q); page.Total != 3 {
		t.Fatal("later malformed evidence damaged an already complete result")
	}
	activityRaw, _ := json.Marshal(ActivityQuery{ExpectedWallet: "fills", ExpectedNetwork: "regtest", Snapshot: first.Revision})
	if _, err := e.Command(context.Background(), Request{Method: "activity.export", Params: activityRaw}); err == nil {
		t.Fatal("fill revision was reused as an activity export")
	}
}
