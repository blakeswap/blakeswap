package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// Current whole-child authorization and two correctly signed deterministic
// refund observations flow through ordinary advanceSwap. No deep archival or
// manual terminal metadata supplies the hot parent classification.
func hotRefundHistoryFixture(t *testing.T, sell chain.ID) (*Engine, *Swap) {
	t.Helper()
	e, s, backend, secret := isolatedFixtureSell(t, "maker", sell)
	e.Config.Name, e.Config.Mode = "hot-refund", "trader"
	parent := e.s.ParentOrders[s.Terms.Offer().ID]
	event, err := e.signOffer(parentPublicOffer(*parent), nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	parent.SignedRevision, parent.LastSignedAt = parent.Quantities.Revision, int64(event.CreatedAt)
	e.stageOffer(parentPublicOffer(*parent), event)
	e.s.Outbox = map[string]*Delivery{}
	s.Stage = "waiting for refunds"
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	for _, c := range []contract.HTLC{s.Long, s.Short} {
		obs := recoverySpend(t, e, s, c, true, secret)
		obs.Height, obs.Confirmations = 1, e.Config.Network.Confirmations()
		all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = obs
		e.nodes[c.Chain] = &sendBackend{receiveBackend: backend.receiveBackend, transaction: func(_ context.Context, txid string) (chain.Transaction, error) {
			for _, raw := range []string{s.LongFunding, s.ShortFunding} {
				tx, err := contract.Parse(raw)
				if err == nil && tx.TxHash().String() == txid {
					return chain.Transaction{TxID: txid, Hex: raw, Height: 1, Confirmations: 500}, nil
				}
			}
			return chain.Transaction{}, &chain.RPCError{Code: -5}
		}}
		e.chainFresh[c.Chain] = true
	}
	if err := e.advanceSwap(context.Background(), s, all); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if s.Stage != "refunded" || parent.Quantities.Released != parent.Quantities.Total || parent.Quantities.Committed != 0 || !e.s.FillRecords[s.ID].Allocation.EverCommitted {
		t.Fatal("ordinary positive refund did not retain terminal accounting")
	}
	if len(e.s.OrderRecords[parent.Offer.ID].Settlements) != 0 || e.s.Capacity.Archived.Count != 0 {
		t.Fatal("fixture introduced a cold terminal convenience link")
	}
	return e, s
}

func TestParentFillHotWholeRefundHistoryIsReadOnlyAndSurvivesReload(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, s := hotRefundHistoryFixture(t, sell)
			id := s.Terms.Offer().ID
			for _, phase := range []string{"hot", "reloaded"} {
				if phase == "reloaded" {
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					e.s = saved
				}
				before := protocol.Digest(e.s)
				if status, err := e.finishedOrderChecked(id); err != nil || status != "refunded" {
					t.Fatal("positive hot refund classification", phase, status, err)
				}
				raw, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: "regtest", Owner: "mine", Status: "refunded", Limit: 1})
				result, err := e.Command(context.Background(), Request{Method: "market.list", Params: raw})
				if err != nil {
					t.Fatal(err)
				}
				page := result.(MarketPage)
				if page.Total != 1 || len(page.Records) != 1 || page.Records[0].Status != "refunded" || len(page.Records[0].SwapIDs) != 1 || page.Records[0].SwapIDs[0] != s.ID || page.Records[0].Quantities.Released != s.Request.Quantity {
					t.Fatal("hot refunded filter lost exact child or conserved quantity")
				}
				if _, err := e.orderSource(OrderActionFields{OrderAction: "recreate", SourceOfferID: id, SourceEventID: e.s.Offers[id].ID.Hex()}, time.Now().Unix()); err != nil {
					t.Fatal("positive hot refund cannot enter a fresh deliberate review", err)
				}
				if protocol.Digest(e.s) != before || len(e.archivePuts) != 0 || len(e.archiveDeletes) != 0 || len(e.s.OrderRecords[id].Settlements) != 0 {
					t.Fatal("history classification changed custody, charges or terminal metadata")
				}
			}
		})
	}
}

func TestParentFillHotWholeRefundRefusesStaleOrIncompleteIdentity(t *testing.T) {
	for _, mode := range []string{"stale stage after demotion", "live monitoring", "missing allocation", "changed parent identity", "changed parent economics", "never committed", "conflicting allocation", "wrong core key"} {
		t.Run(mode, func(t *testing.T) {
			e, s := hotRefundHistoryFixture(t, chain.BTC)
			id := s.Terms.Offer().ID
			parent, child := e.s.ParentOrders[id], e.s.FillRecords[s.ID]
			wantError := true
			switch mode {
			case "stale stage after demotion":
				// Project a valid durable reorg demotion while retaining the old
				// display stage. Current bins must override that stale label.
				next, fill, err := parent.transitionFill(*child, FillCommitted, false)
				if err != nil {
					t.Fatal(err)
				}
				*parent, *child = next, fill
				wantError = false
			case "live monitoring":
				s.Stage = "awaiting chain confirmations"
				wantError = false
			case "missing allocation":
				delete(e.s.FillRecords, s.ID)
			case "changed parent identity":
				child.ParentID = protocol.Digest("another parent")
			case "changed parent economics":
				parent.Offer.BuyAmount++
				parent.Economics = parent.Offer.EconomicsDigest()
			case "never committed":
				child.Allocation.EverCommitted = false
			case "conflicting allocation":
				child.Allocation.Disposition = FillCommitted
			case "wrong core key":
				delete(e.s.Swaps, s.ID)
				e.s.Swaps[protocol.Digest("another core key")] = s
			}
			before := protocol.Digest(e.s)
			status, err := e.finishedOrderChecked(id)
			if (err != nil) != wantError || status != "" {
				t.Fatal("stale or incomplete hot refund supplied a finished outcome", status, err)
			}
			if protocol.Digest(e.s) != before || len(e.archivePuts) != 0 || len(e.archiveDeletes) != 0 {
				t.Fatal("failed classification changed retained evidence")
			}
		})
	}
}

func TestParentFillHotRefundDoesNotReplaceMixedPartialOrWithdrawnOutcome(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run("mixed/"+string(sell), func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, sell)
			all := fillPairOutcomes(t, e, children, secrets, false)
			for _, s := range children {
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			id := children[0].Terms.Offer().ID
			before := protocol.Digest(e.s)
			status, err := e.finishedOrderChecked(id)
			p := e.s.ParentOrders[id]
			if err != nil || status != "cancelled" || p.Quantities.Filled != 400000 || p.Quantities.Released != 600000 || protocol.Digest(e.s) != before {
				t.Fatal("one refunded partial child relabelled the mixed parent", status, err)
			}
		})
	}
	t.Run("withdrawn", func(t *testing.T) {
		e, maker, now, p := wholeMarketFixture(t)
		r := admissionRequest(t, e, maker, p.Offer.SellAmount)
		if err := applyFillRequest(t, e, r, now); err != nil {
			t.Fatal(err)
		}
		s := e.s.Swaps[r.ID]
		e.clocks[s.Long.Chain], e.clocks[s.Short.Chain] = s.Long.RefundHeight, s.Short.RefundHeight
		if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil {
			t.Fatal(err)
		}
		if err := e.withdrawParentAvailable(p.Offer.ID, now+1); err != nil {
			t.Fatal(err)
		}
		// Even a stale/inconsistent display label cannot turn a positively
		// returned, then withdrawn allocation into a funded refund.
		s.Stage = "refunded"
		before := protocol.Digest(e.s)
		status, err := e.finishedOrderChecked(p.Offer.ID)
		p = e.s.ParentOrders[p.Offer.ID]
		if err != nil || status != "cancelled" || p.Quantities.Withdrawn != p.Quantities.Total || e.s.FillRecords[s.ID].Allocation.currentQuantity() != 0 || protocol.Digest(e.s) != before {
			t.Fatal("withdrawn never-funded quantity became a refund", status, err)
		}
	})
}
