package daemon

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func fillAdmissionEngine(t *testing.T, sell chain.ID) (*Engine, nostr.SecretKey, int64) {
	t.Helper()
	e, _ := receiveEngine(t)
	p, maker := fillParentFixture(t, sell, 0)
	e.identity = maker
	e.Config.Mode = "trader"
	e.s.Version, e.s.Network = StateVersion, chain.Regtest
	e.s.Offers, e.s.Book = map[string]nostr.Event{}, map[string]nostr.Event{}
	e.s.Swaps, e.s.Outbox = map[string]*Swap{}, map[string]*Delivery{}
	e.s.ParentOrders = map[string]*ParentOrder{p.Offer.ID: &p}
	e.s.FundingFees = map[string]FeeSelection{"offer/" + p.Offer.ID: p.FundingPolicy}
	e.s.OfferTowers = map[string]protocol.Tower{p.Offer.ID: {Version: protocol.Version}}
	now := time.Now().Unix()
	first := fillRequestFixture(t, p, maker, 400000)
	p.SignedRevision, p.LastSignedAt = 1, now
	e.stageOffer(p.Offer, first.OfferEvent)
	e.walletCoins = map[chain.ID]map[string][]chain.UTXO{}
	var points []CoinOutpoint
	for _, amount := range []int64{406500, 606500, 19500} {
		point := CoinOutpoint{TxID: transport.RandomID()}
		points = append(points, point)
		coin := chain.UTXO{TxID: point.TxID, Amount: chain.Coins(amount), Script: hex.EncodeToString(e.scripts[sell]), Confirmations: 6}
		if e.walletCoins[sell] == nil {
			e.walletCoins[sell] = map[string][]chain.UTXO{}
		}
		e.walletCoins[sell][coin.Script] = append(e.walletCoins[sell][coin.Script], coin)
	}
	e.s.CoinReservations = map[string]CoinReservation{"offer/" + p.Offer.ID: {Chain: sell, Inputs: points}}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		e.heights[id], e.clocks[id] = 200, 200
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, maker, now
}

func admissionRequest(t *testing.T, e *Engine, maker nostr.SecretKey, quantity int64) protocol.Request {
	t.Helper()
	var parent *ParentOrder
	for _, p := range e.s.ParentOrders {
		parent = p
	}
	r := fillRequestFixture(t, *parent, maker, quantity)
	r.OfferEvent = e.s.Offers[parent.Offer.ID]
	return r
}
func applyFillRequest(t *testing.T, e *Engine, r protocol.Request, at int64) error {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return e.acceptFillRequestAt(r.Taker, transport.Message{Version: transport.MessageVersion, Type: "request", SwapID: r.ID, Body: raw}, at)
}

func TestParentFillAcceptanceCommitsDisjointInputsTermsAndExactRetry(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, maker, now := fillAdmissionEngine(t, sell)
			first := admissionRequest(t, e, maker, 400000)
			if err := applyFillRequest(t, e, first, now); err != nil {
				t.Fatal(err)
			}
			p := e.s.ParentOrders[e.s.FillRecords[first.ID].ParentID]
			if p.Quantities.Available != 600000 || p.Quantities.Reserved != 400000 || p.SignedRevision != 1 {
				t.Fatal("same-second acceptance did not retain pending public revision")
			}
			prior := protocol.Digest(*p)
			if err := applyFillRequest(t, e, first, now+10); err != nil {
				t.Fatal(err)
			}
			if protocol.Digest(*p) != prior {
				t.Fatal("exact request reserved twice")
			}
			stale := first
			stale.ID, stale.Hash = transport.RandomID(), transport.RandomID()
			if err := applyFillRequest(t, e, stale, now); err != nil {
				t.Fatal(err)
			}
			if e.s.FillRecords[stale.ID] != nil || protocol.Digest(*p) != prior {
				t.Fatal("pending public revision admitted another child")
			}
			next, event, err := e.prepareParentPublication(*p, now+1)
			if err != nil || event == nil {
				t.Fatalf("next wall-clock publication: %v", err)
			}
			*p = next
			e.stageOffer(parentPublicOffer(next), *event)
			second := admissionRequest(t, e, maker, 600000)
			if err := applyFillRequest(t, e, second, now+1); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			parent := saved.ParentOrders[p.Offer.ID]
			if parent.Quantities.Reserved != 1000000 || parent.Fees[sell].Reserved != 53000 {
				t.Fatal("both child allocations and fee reservations were not durable")
			}
			a, b := saved.FillRecords[first.ID], saved.FillRecords[second.ID]
			if len(a.Inputs) != 1 || len(b.Inputs) != 1 || pointKey(a.Inputs[0]) == pointKey(b.Inputs[0]) || len(saved.CoinReservations["offer/"+p.Offer.ID].Inputs) != 1 {
				t.Fatal("children do not own disjoint original parent inputs")
			}
			for _, child := range []*FillRecord{a, b} {
				s := saved.Swaps[child.ID]
				if s.Terms.Short.Amount != child.Allocation.Quantity || s.Terms.Long.Amount != child.BuyAmount || saved.FundingFees["swap/"+child.ID].FundingFee != 6500 {
					t.Fatal("child amount or funding policy not committed")
				}
				found := false
				for _, d := range saved.Outbox {
					if d.Type == "accepted" && d.To == s.Request.Taker {
						found = true
					}
				}
				if !found {
					t.Fatal("allocation committed without acceptance")
				}
			}
			if saved.Swaps[a.ID].Request.Hash == saved.Swaps[b.ID].Request.Hash || saved.Swaps[a.ID].Terms.MakerKeys[sell] == saved.Swaps[b.ID].Terms.MakerKeys[sell] {
				t.Fatal("child secret or maker key reused")
			}
		})
	}
}

func TestParentFillAcceptanceRejectsReusedSecretsWithoutAllocating(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.BTC)
	first := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, first, now); err != nil {
		t.Fatal(err)
	}
	p := e.s.ParentOrders[e.s.FillRecords[first.ID].ParentID]
	next, event, err := e.prepareParentPublication(*p, now+1)
	if err != nil || event == nil {
		t.Fatal(err)
	}
	*p = next
	e.stageOffer(parentPublicOffer(next), *event)
	// A disk move must not free either secret or key identity.
	for key := range e.s.FillKeys {
		if err := e.stageArchive("fill_keys", key); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	second := admissionRequest(t, e, maker, 600000)
	for _, kind := range []string{"hash", "key"} {
		r := second
		if kind == "hash" {
			r.Hash = first.Hash
		} else {
			r.Keys = map[chain.ID]string{chain.BTC: first.Keys[chain.BTC], chain.Blake: second.Keys[chain.Blake]}
		}
		before := protocol.Digest(*p)
		if err := applyFillRequest(t, e, r, now+1); err == nil {
			t.Fatalf("reused %s accepted", kind)
		}
		if protocol.Digest(*p) != before || e.s.FillRecords[r.ID] != nil {
			t.Fatal("failed identity admission consumed resources")
		}
	}
}

func TestParentFillAcceptanceFailedSaveCannotPublishAllocation(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.Blake)
	r := admissionRequest(t, e, maker, 400000)
	reads := 0
	e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
		if kind == "fill_keys" {
			reads++
			if reads == 5 {
				if err := e.vault.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
		return storage.ArchiveRecord{}, false, nil
	}
	if err := applyFillRequest(t, e, r, now); err == nil || e.fatal == nil {
		t.Fatalf("failed durable acceptance did not stop execution: %v (identity reads %d)", err, reads)
	}
	if err := e.publicationReady(chain.Blake, true); err == nil {
		t.Fatal("failed commit could publish")
	}
}

func TestParentFillMissingCoreNeverReallocatesRetainedChild(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.BTC)
	r := admissionRequest(t, e, maker, 400000)
	if err := applyFillRequest(t, e, r, now); err != nil {
		t.Fatal(err)
	}
	p := e.s.ParentOrders[e.s.FillRecords[r.ID].ParentID]
	delete(e.s.Swaps, r.ID) // Incomplete retained evidence is a hold, never a new ID.
	before := protocol.Digest(*p)
	if err := applyFillRequest(t, e, r, now); err == nil {
		t.Fatal("missing core recreated accepted child")
	}
	if protocol.Digest(*p) != before {
		t.Fatal("incomplete prior child double-allocated quantity")
	}
}
