package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func actualInputEngine(t *testing.T) *Engine {
	t.Helper()
	e, _, _ := sendFixture(t)
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		b := &sendBackend{receiveBackend: &receiveBackend{used: map[string]bool{}}}
		b.coins = []chain.UTXO{{TxID: strings.Repeat(string(rune('a'+i)), 64), Amount: 100000000, Script: hex.EncodeToString(e.scripts[id]), Confirmations: 2}}
		e.nodes[id] = b
		e.watch[id] = b
	}
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// These are ordinary command-boundary controls, not actual node evidence. They
// exercise the explicit inputs used by the real fixtures, retained maker/child
// custody and the exact current encrypted request. Old inputs stay refused.
func TestActualFixtureExplicitWholeInputsAndPrivateProtection(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, bps := range []int64{0, 50, 125} {
			t.Run(fmt.Sprintf("%s/%d", sell, bps), func(t *testing.T) {
				maker, taker := actualInputEngine(t), actualInputEngine(t)
				provider := discoveryEngine(t)
				provider.Config.RescueFeeBPS = bps
				if err := provider.advertiseTower(); err != nil {
					t.Fatal(err)
				}
				for _, e := range []*Engine{maker, taker} {
					e.Config.Tower = provider.ownTower()
					e.s.Towers = provider.s.Towers
				}
				old, _ := json.Marshal(map[string]any{"sell": sell, "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": bps})
				before := protocol.Digest(maker.s)
				if _, err := maker.Command(context.Background(), Request{Method: "offer.create", Params: old}); err == nil || !strings.Contains(err.Error(), "explicit whole or partial") || protocol.Digest(maker.s) != before {
					t.Fatal("old manual inputs acquired parent authority", err)
				}
				params := walletWholeParams(sell, 1000000, 2000000, 2000, 0, bps)
				if bps == 125 {
					params["tower_pubkey"] = provider.ownTower().Npub
				}
				raw, _ := json.Marshal(params)
				result, err := maker.Command(context.Background(), Request{Method: "offer.create", Params: raw})
				if err != nil {
					t.Fatal(err)
				}
				offer := result.(protocol.Offer)
				parent := maker.s.ParentOrders[offer.ID]
				receivedCap := int64(2000)
				if bps > 0 {
					receivedCap = 20000
				}
				if parent == nil || parent.Offer.FillPolicy != (protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}) || parent.Quantities.Available != 1000000 || parent.Fees[sell].Limit != 22000 || parent.Fees[sell.Other()].Limit != receivedCap || parent.Bounties[sell].Limit != protocol.Bounty(1000000, bps) || parent.Bounties[sell.Other()].Limit != protocol.Bounty(2000000, bps) {
					t.Fatal("actual fixture changed its quantity, fees or private protection limits")
				}
				event := maker.s.Offers[offer.ID]
				if strings.Contains(event.Content, "tower_bps") || strings.Contains(event.Content, "fee_budgets") || strings.Contains(event.Content, "bounty_budgets") {
					t.Fatal("private actual-fixture limits entered the signed public offer")
				}
				taker.s.Book[offer.Maker+":"+offer.ID] = event
				old, _ = json.Marshal(map[string]string{"maker": offer.Maker, "id": offer.ID})
				before = protocol.Digest(taker.s)
				if _, err := taker.Command(context.Background(), Request{Method: "swap.take", Params: old}); err == nil || protocol.Digest(taker.s) != before {
					t.Fatal("old take input silently selected a quantity or revision", err)
				}
				take := map[string]any{"maker": offer.Maker, "id": offer.ID, "quantity": offer.SellAmount, "parent_revision": offer.Revision, "tower_bps": bps}
				if bps == 125 {
					take["tower_pubkey"] = provider.ownTower().Npub
				}
				raw, _ = json.Marshal(take)
				result, err = taker.Command(context.Background(), Request{Method: "swap.take", Params: raw})
				if err != nil {
					t.Fatal(err)
				}
				id := result.(map[string]string)["id"]
				s := taker.s.Swaps[id]
				if s.Request.Version != protocol.Version || s.Request.Quantity != offer.SellAmount || s.Request.Revision != offer.Revision || s.protection().BPS != bps {
					t.Fatal("take lost exact reviewed terms or protection")
				}
				if _, err := s.Request.Validate(int64(event.CreatedAt)); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, d := range taker.s.Outbox {
					if d.Type != "request" {
						continue
					}
					_, envelope, err := transport.Unwrap(maker.identity, d.Event)
					if err != nil || envelope.Version != protocol.Version || envelope.SwapID != id {
						t.Fatal("noncurrent request envelope", err)
					}
					if err := maker.receive(d.Event); err != nil {
						t.Fatal(err)
					}
					found = true
				}
				child := maker.s.FillRecords[id]
				if !found || child == nil || child.ParentRevision != offer.Revision || child.Allocation.Quantity != offer.SellAmount || maker.s.Swaps[id].Terms == nil {
					t.Fatal("request did not retain exact accepted child custody")
				}
				before = protocol.Digest(taker.s)
				if _, err := taker.Command(context.Background(), Request{Method: "swap.take", Params: raw}); err == nil || protocol.Digest(taker.s) != before {
					t.Fatal("second request reused the already assigned whole funding input", err)
				}
			})
		}
	}
}

func TestPreparedFundingSnapshotDoesNotRewindLivePublication(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, _, raw := preparedFundingLookupFixture(t, role)
			path := filepath.Join(t.TempDir(), "state.db")
			capturePreparedFundingSnapshot(t, e, s, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.recordFunding(s); err != nil {
				t.Fatal(err)
			}
			_, currentRaw, sent := localFunding(s)
			if !sent || currentRaw != raw {
				t.Fatal("live publication lost its exact signed transaction")
			}
			after, err := os.ReadFile(path)
			if err != nil || protocol.Digest(before) != protocol.Digest(after) {
				t.Fatal("publication changed the independent prepared checkpoint", err)
			}
			v, err := storage.OpenExisting(path, []byte("receive-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var old State
			if _, err := v.Load(&old); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCompleteFillState(&old); err != nil {
				t.Fatal(err)
			}
			_, savedRaw, savedSent := localFunding(old.Swaps[s.ID])
			if savedSent || savedRaw != raw || len(old.Swaps[s.ID].SelfRefunds) != len(protocol.RescueFees) || len(old.Outbox) != 0 {
				t.Fatal("prepared checkpoint gained later publication state")
			}
			if role == "maker" && (protocol.Digest(old.FillRecords[s.ID]) != protocol.Digest(e.s.FillRecords[s.ID]) || protocol.Digest(old.ParentOrders) != protocol.Digest(e.s.ParentOrders)) {
				t.Fatal("prepared snapshot lost permanent maker accounting")
			}
			// The capture boundary must not relabel a published live state.
			if _, _, liveSent := localFunding(e.s.Swaps[s.ID]); !liveSent {
				t.Fatal("opening a separate prepared checkpoint rewound the running engine")
			}
		})
	}
}
