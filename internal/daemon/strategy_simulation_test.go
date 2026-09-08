package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// Exercise a deterministic sequence against the real shared admission,
// reservation and budget reconciler. Chain observations are simulated here;
// actual signed transactions and settlements have separate real-node coverage.
func TestStrategySequenceOpposingFillsCancellationsRefundsAndPendingCoins(t *testing.T) {
	e, p := strategyFixture(t)
	p.Config.MaxConcurrent = 2
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		e.s.Automations[strategyPolicyID(p.Config.ID, sell)].Config = p.Config.policy(sell)
	}
	backends := map[chain.ID]*receiveBackend{}
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		if b, ok := e.nodes[sell].(*sendBackend); ok {
			backends[sell] = b.receiveBackend
		} else {
			backends[sell] = e.nodes[sell].(*receiveBackend)
		}
	}
	refresh := func() {
		t.Helper()
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			if err := e.refreshChain(context.Background(), sell); err != nil {
				t.Fatal(err)
			}
		}
	}
	committed := map[chain.ID]int64{}
	assertBounds := func() {
		t.Helper()
		u, q, s := e.strategyUsage(p.Config, "")
		if q+s > 2 {
			t.Fatal("concurrent cap exceeded", q, s)
		}
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			v := u[sell]
			side := p.Config.side(sell)
			if v.Exposure > side.MaxExposure || v.ReservedVolume+v.CommittedVolume > side.VolumeLimit || v.CommittedVolume != committed[sell] || v.ReservedFees+v.CommittedFees > p.Config.BTCFeeBudget {
				t.Fatal("sequence exceeded durable limits", sell, v, committed)
			}
			if v.Funds.UnlockedConfirmed < side.MinimumReserve {
				t.Fatal("whole-input reserve spent", sell, v.Funds)
			}
		}
	}
	fills, cancels, refunds := 0, 0, 0
	for round := 0; round < 12; round++ {
		// Reserve and cancel one manual input before strategy admission. A manual
		// pending send sees exactly the same lock as a new quote.
		manual := backends[chain.Blake].coins[0]
		e.s.CoinReservations["send/manual"] = CoinReservation{Chain: chain.Blake, Inputs: []CoinOutpoint{{TxID: manual.TxID, Vout: manual.Vout}}}
		if e.strategyAvailable(chain.Blake, "") >= e.chainBalances(e.publicCoins())[chain.Blake].TotalConfirmed {
			t.Fatal("manual pending send lock ignored")
		}
		delete(e.s.CoinReservations, "send/manual")
		active := []protocol.Offer{}
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			if e.strategyRisk(p, sell, 200000, 202000, 4001, "") == nil {
				t.Fatal("high fee exceeded hard cap")
			}
			if e.strategyRisk(p, sell, 200000, 202000, 2000, "") != nil {
				continue
			}
			id := protocol.Digest([]any{"simulation", round, sell})
			o := protocol.Offer{Version: protocol.Version, Revision: 1, Available: 200000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 200000, Max: 200000}, ID: id, Network: e.Config.Network, Maker: e.identity.Public().Hex(), Sell: sell, SellAmount: 200000, BuyAmount: 202000, Status: "open", Expires: time.Now().Unix() + 120}
			event, err := e.signOffer(o, nostr.Now())
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := e.reservationCandidate("offer/"+id, sell, 202000)
			if err != nil {
				t.Fatal(err)
			}
			e.s.Offers[id] = event
			e.s.CoinReservations["offer/"+id] = reservation
			child := e.s.Automations[strategyPolicyID(p.Config.ID, sell)]
			child.Charges[id] = automationCharge(child.Config, id, 202000, 2000)
			active = append(active, o)
			assertBounds()
		}
		for i, o := range active {
			child := e.s.Automations[strategyPolicyID(p.Config.ID, o.Sell)]
			if (round+i)%3 == 0 {
				o.Status, o.Available, o.Revision = "cancelled", 0, o.Revision+1
				event, err := e.signOffer(o, nostr.Now())
				if err != nil {
					t.Fatal(err)
				}
				e.s.Offers[o.ID] = event
				delete(e.s.CoinReservations, "offer/"+o.ID)
				e.reconcileAutomations()
				if child.Charges[o.ID].State != "released" {
					t.Fatal("unfunded cancellation retained charge")
				}
				cancels++
				assertBounds()
				continue
			}
			// Signed funding commits before any wallet confirmation. Its input change
			// remains pending, never reusable to fill the next quote.
			event := e.s.Offers[o.ID]
			id := protocol.Digest([]string{"swap", o.ID})
			request := automationChildRequest(t, e, event)
			request.ID = id
			s := &Swap{ID: id, Role: "maker", Request: request, Stage: "awaiting confirmations", ShortFunding: "simulated durable signed funding"}
			s.Short.TxID = protocol.Digest([]string{"funding", id})
			s.Long.TxID = protocol.Digest([]string{"peer-funding", id})
			e.s.Swaps[id] = s
			committed[o.Sell] += 200000
			e.reconcileAutomations()
			r := e.s.CoinReservations["offer/"+o.ID]
			points := map[string]bool{}
			for _, pt := range r.Inputs {
				points[pointKey(pt)] = true
			}
			coins := []chain.UTXO{}
			var locked int64
			for _, coin := range backends[o.Sell].coins {
				if points[chain.OutpointKey(coin.TxID, coin.Vout)] {
					locked += int64(coin.Amount)
				} else {
					coins = append(coins, coin)
				}
			}
			coins = append(coins, chain.UTXO{TxID: s.Short.TxID, Vout: 1, Amount: chain.Coins(locked - 202000), Script: hex.EncodeToString(e.scripts[o.Sell]), Confirmations: 0})
			backends[o.Sell].coins = coins
			delete(e.s.CoinReservations, "offer/"+o.ID)
			refresh()
			assertBounds()
			if e.chainBalances(e.publicCoins())[o.Sell].Unconfirmed <= 0 {
				t.Fatal("pending funding change not represented")
			}
			refund := round%2 == 0
			creditAsset, credit := o.Sell.Other(), int64(200000)
			s.Stage = "completed"
			if refund {
				creditAsset, credit = o.Sell, int64(198000)
				s.Stage = "refunded"
				refunds++
			}
			s.ShortSpend = protocol.Digest([]string{"short-spend", id})
			s.LongSpend = protocol.Digest([]string{"long-spend", id})
			s.ShortConfirmations = 2
			s.LongConfirmations = 2
			e.recordStrategyExposure(s)
			e.reconcileAutomations()
			e.reconcileStrategyExposure()
			proof := *child.Charges[o.ID].ExposureSettled
			e.recordStrategyExposure(s)
			if proof != *child.Charges[o.ID].ExposureSettled {
				t.Fatal("unchanged scanner proof changed semantic state")
			}
			backends[creditAsset].coins = append(backends[creditAsset].coins, chain.UTXO{TxID: protocol.Digest([]string{"payout", id}), Amount: chain.Coins(credit), Script: hex.EncodeToString(e.scripts[creditAsset]), Confirmations: 0})
			refresh()
			// Simulated next positive block confirms change and payout, not before.
			for _, b := range backends {
				for j := range b.coins {
					b.coins[j].Confirmations = 2
				}
			}
			refresh()
			fills++
			assertBounds()
			// Repeated settlement observations and an durable-schema round trip do
			// not duplicate either the economic charge or the settled exposure fact.
			raw, err := json.Marshal(e.s)
			if err != nil {
				t.Fatal(err)
			}
			var state State
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			e.s = state
			if e.s.CoinReservations == nil {
				e.s.CoinReservations = map[string]CoinReservation{}
			}
			p = e.s.MakerStrategies[p.Config.ID]
			e.reconcileAutomations()
			assertBounds()
		}
	}
	if fills < 4 || cancels < 2 || refunds < 1 {
		t.Fatal("sequence did not exercise enough transitions", fills, cancels, refunds)
	}
	t.Log(fmt.Sprintf("%d signed fills, %d unfunded cancellations, %d refunds; shared caps held after every transition", fills, cancels, refunds))
}
