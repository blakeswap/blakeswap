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
	"github.com/blakeswap/blakeswap/internal/transport"
)

func automationWholeFields(sell chain.ID, quantity, buy, fee, bps int64) FillOrderFields {
	return FillOrderFields{FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: quantity, Max: quantity}, FeeBudgets: map[chain.ID]int64{sell: fee + 20000, sell.Other(): 20000}, BountyBudgets: map[chain.ID]int64{sell: protocol.Bounty(quantity, bps), sell.Other(): protocol.Bounty(buy, bps)}}
}

func automationChildRequest(t *testing.T, e *Engine, event nostr.Event) protocol.Request {
	t.Helper()
	o, err := historicalOffer(event)
	if err != nil {
		t.Fatal(err)
	}
	id := transport.RandomID()
	keys, err := e.swapKeys(transport.RandomID())
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Request{Version: protocol.Version, ID: id, Revision: o.Revision, Quantity: o.SellAmount, OfferEvent: event, Taker: nostr.Generate().Public().Hex(), Hash: transport.RandomID(), Keys: keys}
}

func automationOrderRequest(t *testing.T, e *Engine, event nostr.Event) (string, transport.Message) {
	t.Helper()
	r := automationChildRequest(t, e, event)
	raw, _ := json.Marshal(r)
	return r.Taker, transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "request", SwapID: r.ID, Body: raw}
}

func assertAutomaticWholeParent(t *testing.T, e *Engine, policy *AutomationPolicy) {
	t.Helper()
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	id := policy.CurrentOfferID
	parent, receipt := saved.ParentOrders[id], saved.TradeReceipts[id]
	if parent == nil || receipt == nil || receipt.Result.State != "accepted" || receipt.AutomationID != policy.Config.ID || saved.Automations[policy.Config.ID].Pending != nil {
		t.Fatal("whole parent, accepted receipt and policy were not committed together", policy.Decision)
	}
	request, quote := receipt.Snapshot.Request, receipt.Snapshot.Quote
	want := automationWholeFields(request.Sell, request.SellAmount, request.BuyAmount, request.FundingFee, request.TowerBPS)
	if protocol.Digest(request.FillOrderFields) != protocol.Digest(want) || protocol.Digest(quote.FillOrderFields) != protocol.Digest(want) || parent.Offer.FillPolicy != want.FillPolicy || parent.Offer.Version != protocol.Version || parent.Offer.Revision != 1 || parent.Quantities.Available != request.SellAmount || parent.Quantities.Total != request.SellAmount || parent.Quantities.Reserved != 0 || parent.Quantities.Committed != 0 || len(saved.FillRecords) != 0 {
		t.Fatal("automatic quote changed whole quantity or child limits", request, parent)
	}
	if quote.FundingReserve != request.FundingFee || quote.PaidTotal != request.SellAmount+request.FundingFee || parent.FundingPolicy != request.FeeSelection {
		t.Fatal("whole order reserved more than one funding transaction", quote)
	}
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		if parent.Fees[asset] != (FillBudget{Limit: want.FeeBudgets[asset]}) || parent.Bounties[asset] != (FillBudget{Limit: want.BountyBudgets[asset]}) {
			t.Fatal("durable private caps changed", parent)
		}
	}
	// Exercise the actual quantity admission on the persisted parent. A legal
	// whole child consumes its sole slot and the exact selected monetary caps.
	childRequest := automationChildRequest(t, e, saved.Offers[id])
	next, child, err := parent.reserveFill(childRequest)
	if err != nil || next.Quantities.Available != 0 || next.Quantities.Reserved != request.SellAmount || child.Allocation.Quantity != request.SellAmount {
		t.Fatal("saved parent cannot admit its one whole child", err)
	}
	if protocol.Digest(child.Fees) != protocol.Digest(want.FeeBudgets) || protocol.Digest(child.Bounties) != protocol.Digest(want.BountyBudgets) {
		t.Fatal("child changed reviewed caps", child)
	}
	if _, _, err := next.reserveFill(automationChildRequest(t, e, saved.Offers[id])); err == nil {
		t.Fatal("a second child reused the whole quantity")
	}
	charge := saved.Automations[policy.Config.ID].Charges[id]
	if charge == nil || charge.Volume != request.SellAmount || charge.State != "reserved" || charge.BTCFees != want.FeeBudgets[chain.BTC]+want.BountyBudgets[chain.BTC] || charge.BlakeFees != want.FeeBudgets[chain.Blake]+want.BountyBudgets[chain.Blake] {
		t.Fatal("lifetime policy charge differs from its exact child authorization", charge)
	}
}

func TestAutomationWholeParentUsesSelectedFeeAndExactBounties(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, bps := range []int64{0, 50} {
			t.Run(fmt.Sprintf("%s/bps%d", sell, bps), func(t *testing.T) {
				e, p := automationFixture(t)
				if sell == chain.BTC {
					backend := &sendBackend{receiveBackend: e.nodes[chain.BTC].(*receiveBackend)}
					backend.coins = []chain.UTXO{{TxID: transport.RandomID(), Amount: 1000000, Script: hex.EncodeToString(e.scripts[chain.BTC]), Confirmations: 2}}
					e.nodes[chain.BTC] = backend
					if err := e.refresh(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				p.Config.Sell, p.Config.SellAmount, p.Config.VolumeLimit = sell, 600001, 1800003
				p.Config.Rate = AutomationRate{1, 2}
				if sell == chain.BTC {
					p.Config.Rate = AutomationRate{2, 1}
				}
				p.Config.MaxRate = AutomationRate{2, 1}
				p.Config.FundingFee, p.Config.MaxFundingFee = 6500, 7000
				if bps != 0 {
					provider, _, _ := sendFixture(t)
					provider.s.Version, provider.s.Network = StateVersion, chain.Regtest
					provider.Config.RescueFeeBPS = bps
					if err := provider.advertiseTower(); err != nil {
						t.Fatal(err)
					}
					e.ingestTower(provider.s.Towers[provider.identity.Public().Hex()])
					p.Config.TowerBPS, p.Config.TowerPubKey = bps, provider.identity.Public().Hex()
				}
				e.runAutomations(context.Background())
				assertAutomaticWholeParent(t, e, p)
			})
		}
	}
}

func TestStrategyRunnerPersistsWholeParentForEitherDirection(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, strategy := strategyFixture(t)
			if sell == chain.BTC {
				e.nodes[chain.BTC] = &sendBackend{receiveBackend: e.nodes[chain.BTC].(*receiveBackend)}
			}
			e.s.Automations[strategyPolicyID(strategy.Config.ID, sell.Other())].NextAction = time.Now().Unix() + 3600
			p := e.s.Automations[strategyPolicyID(strategy.Config.ID, sell)]
			p.NextAction = 0
			e.runAutomations(context.Background())
			assertAutomaticWholeParent(t, e, p)
		})
	}
}

func TestAutomationClosedParentReleasesOnlyKnownUnfundedChargeBeforePublication(t *testing.T) {
	for _, mode := range []string{"local", "uncertain", "policy-held", "parent-held", "consumed", "committed"} {
		t.Run(mode, func(t *testing.T) {
			e, p := automationFixture(t)
			e.runAutomations(context.Background())
			id := p.CurrentOfferID
			parent := e.s.ParentOrders[id]
			before := e.s.Offers[id]
			if err := e.withdrawParentAvailable(id, parent.LastSignedAt); err != nil {
				t.Fatal(err)
			}
			if !parent.Quantities.Closed || parent.SignedRevision == parent.Quantities.Revision || e.s.Offers[id].ID != before.ID {
				t.Fatal("fixture lost pending same-second publication")
			}
			switch mode {
			case "uncertain":
				p.Charges[id].Uncertain = true
			case "policy-held":
				p.RestoreHold = true
			case "parent-held":
				parent.RestoreHold = true
			case "consumed":
				f := parent.Fees[chain.Blake]
				f.Consumed = 2000
				parent.Fees[chain.Blake] = f
			case "committed":
				p.Charges[id].State = "committed"
			}
			e.reconcileAutomations()
			want := "reserved"
			if mode == "local" {
				want = "released"
			}
			if mode == "committed" {
				want = "committed"
			}
			if p.Charges[id].State != want {
				t.Fatal("closed parent erased uncertain/consumed authority", mode, p.Charges[id])
			}
		})
	}
}

func TestAutomationReceiptRejectsChangedWholeGrant(t *testing.T) {
	for _, strategy := range []bool{false, true} {
		for _, field := range []string{"mode", "min", "max", "paid-fee", "received-fee", "paid-bounty", "received-bounty", "missing-zero"} {
			t.Run(fmt.Sprintf("strategy%t/%s", strategy, field), func(t *testing.T) {
				e, p := automationFixture(t)
				if strategy {
					var parent *MakerStrategy
					e, parent = strategyFixture(t)
					e.s.Automations[strategyPolicyID(parent.Config.ID, chain.BTC)].NextAction = time.Now().Unix() + 3600
					p = e.s.Automations[strategyPolicyID(parent.Config.ID, chain.Blake)]
				}
				buy, err := automationAmounts(p.Config, p.Config.Rate)
				if err != nil {
					t.Fatal(err)
				}
				request := TradeQuoteRequest{Kind: "maker", ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Sell: p.Config.Sell, SellAmount: p.Config.SellAmount, BuyAmount: buy, Expires: time.Now().Unix() + 120, FeeSelection: FeeSelection{FundingFee: 2000, OwnerFeeCap: 20000}, FillOrderFields: automationWholeFields(p.Config.Sell, p.Config.SellAmount, buy, 2000, 0)}
				if strategy {
					config, quote, _, err := e.planStrategy(e.s.MakerStrategies[p.Config.StrategyID], p.Config.Sell, time.Now().Unix(), false)
					if err != nil {
						t.Fatal(err)
					}
					request.SellAmount, request.BuyAmount = config.SellAmount, quote.BuyAmount
					request.FillOrderFields = automationWholeFields(config.Sell, config.SellAmount, quote.BuyAmount, 2000, 0)
				}
				quote := requestQuote(t, e, request)
				confirm := confirmation(quote)
				p.Pending = &confirm
				r := &TradeReceipt{Digest: protocol.Digest(confirm), Snapshot: e.tradeQuotes[quote.Token], Result: ConfirmTradeResult{ID: confirm.RequestID, Kind: "maker", State: "pending"}, AutomationID: p.Config.ID, AutomationRevision: p.Revision}
				if err := e.validateAutomationReceipt(r); err != nil {
					t.Fatal("valid whole grant", err)
				}
				request = r.Snapshot.Request
				switch field {
				case "mode":
					request.FillPolicy.Mode = protocol.FillPartial
				case "min":
					request.FillPolicy.Min--
				case "max":
					request.FillPolicy.Max++
				case "paid-fee":
					request.FeeBudgets[request.Sell]++
				case "received-fee":
					request.FeeBudgets[request.Sell.Other()]++
				case "paid-bounty":
					request.BountyBudgets[request.Sell]++
				case "received-bounty":
					request.BountyBudgets[request.Sell.Other()]++
				case "missing-zero":
					delete(request.BountyBudgets, request.Sell)
				}
				r.Snapshot.Request = request
				if err := e.validateAutomationReceipt(r); err == nil {
					t.Fatal("altered automatic whole grant accepted", field)
				}
			})
		}
	}
}
