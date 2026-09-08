package daemon

import (
	"context"
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"testing"
	"time"
)

func TestArchiveActualRefundOrderSurvivesCoreArchival(t *testing.T) {
	e, swap, backend, secret := isolatedFixture(t, "maker")
	e.Config.Name = "funded-order"
	e.Config.Mode = "trader"
	offer := swap.Terms.Offer()
	reserved := stageArchiveParent(t, e, swap)
	e.s.Outbox = map[string]*Delivery{}
	swap.Stage = "waiting for refunds"
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	for _, c := range []contract.HTLC{swap.Long, swap.Short} {
		obs := recoverySpend(t, e, swap, c, true, secret)
		obs.Height = 1
		obs.Confirmations = 500
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
	beforeToken := BackupSemanticToken(e.s)
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != beforeToken {
		t.Fatal("derived query companion changed backup freshness on a location-only move")
	}
	if e.s.Swaps[swap.ID] != nil {
		t.Fatal("fixture failed to move positively settled core")
	}
	if e.s.Offers[offer.ID].Content != "" {
		t.Error("completed funded source remains active after its core archives")
	}
	if got := e.finishedOrder(offer.ID); got != "refunded" {
		t.Error("retained completion classification lost", got)
	}
	if e.activeWork("offer") != 0 {
		t.Error("settled refund still consumes active offer capacity")
	}
	if _, err := e.orderSource(OrderActionFields{OrderAction: "recreate", SourceOfferID: offer.ID, SourceEventID: reserved.ID.Hex()}, time.Now().Unix()); err != nil {
		t.Error("refunded historical source cannot be deliberately recreated", err)
	}
	q, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Owner: "mine", Status: "refunded", Limit: 1})
	result, err := e.historyCommand(context.Background(), Request{Method: "market.list", Params: q})
	if err != nil {
		t.Fatal(err)
	}
	page := result.(MarketPage)
	if len(page.Records) != 1 || len(page.Records[0].SwapIDs) != 1 || page.Records[0].SwapIDs[0] != swap.ID {
		t.Fatal("closed own query lost exact archived maker context", page.Total)
	}

	for _, kind := range []string{"offers", "order_records", "offer_towers"} {
		if _, found, err := e.archiveRecord(kind, offer.ID); err != nil || !found {
			t.Fatal("retired order companion not cold", kind, err)
		}
	}
	if _, hot := e.s.OrderRecords[offer.ID]; hot {
		t.Fatal("lifetime order metadata remained hot")
	}
	var coldRecord OrderRecord
	if found, err := e.archivedValue("order_records", offer.ID, &coldRecord); err != nil || !found || coldRecord.Protection == nil || coldRecord.Protection.BPS != 37 || coldRecord.Protection.Scripts[chain.BTC] != policy.Scripts[chain.BTC] {
		t.Fatal("exact order protection context lost", err)
	}
	if _, hot := e.s.OfferTowers[offer.ID]; hot {
		t.Fatal("retired protection remains hot")
	}
	var loaded State
	if _, err := e.vault.Load(&loaded); err != nil {
		t.Fatal(err)
	}
	e.s = loaded
	if e.finishedOrder(offer.ID) == "" {
		t.Fatal("reloaded checkpoint lost terminal identity")
	}
	if _, err := e.activateArchived("swaps", swap.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Swaps[swap.ID].Stage = "awaiting chain confirmations"
	if e.finishedOrder(offer.ID) != "" {
		t.Fatal("cold terminal companion overrode live reactivation")
	}
	if _, err := e.orderSource(OrderActionFields{OrderAction: "recreate", SourceOfferID: offer.ID, SourceEventID: e.s.OrderRecords[offer.ID].EventID}, time.Now().Unix()); err == nil {
		t.Fatal("reactivated obligation granted recreation")
	}
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	delete(e.s.Swaps, swap.ID)
	if _, err := e.finishedOrderChecked(offer.ID); err == nil {
		t.Fatal("unreadable cold order silently supplied completion")
	}
}

func TestArchiveNormalCompletedOrderStillArchivesAndRecreates(t *testing.T) {
	e, swap, backend, secret := isolatedFixture(t, "maker")
	e.Config.Name = "funded-order"
	e.Config.Mode = "trader"
	offer := swap.Terms.Offer()
	stageArchiveParent(t, e, swap)
	e.s.Outbox = map[string]*Delivery{}
	swap.Stage = "waiting for refunds"
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	for _, c := range []contract.HTLC{swap.Long, swap.Short} {
		obs := recoverySpend(t, e, swap, c, false, secret)
		obs.Height = 1
		obs.Confirmations = 500
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
	if swap.Stage != "completed" {
		t.Fatal("actual refund progression missing", swap.Stage)
	}
	e.s.Outbox = map[string]*Delivery{}
	completedID := e.s.Offers[offer.ID].ID.Hex()
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if e.finishedOrder(offer.ID) != "filled" {
		t.Fatal("initial completed maker is not recognized")
	}
	policy := protocol.Tower{PubKey: e.identity.Public().Hex(), BPS: 37, Network: chain.Regtest, Scripts: map[chain.ID]string{chain.BTC: "0014-retained-policy"}}
	e.s.OfferTowers = map[string]protocol.Tower{offer.ID: policy}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	beforeToken := BackupSemanticToken(e.s)
	if err := e.compactArchive(context.Background(), all, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != beforeToken {
		t.Fatal("derived query companion changed backup freshness on a location-only move")
	}
	if e.s.Swaps[swap.ID] != nil {
		t.Fatal("fixture failed to move positively settled core")
	}
	if e.s.Offers[offer.ID].Content != "" {
		t.Error("completed funded source remains active after its core archives")
	}
	if e.activeWork("offer") != 0 {
		t.Error("settled refund still consumes active offer capacity")
	}
	if _, err := e.orderSource(OrderActionFields{OrderAction: "recreate", SourceOfferID: offer.ID, SourceEventID: completedID}, time.Now().Unix()); err != nil {
		t.Error("refunded historical source cannot be deliberately recreated", err)
	}
	q, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Owner: "mine", Status: "filled", Limit: 1})
	result, err := e.historyCommand(context.Background(), Request{Method: "market.list", Params: q})
	if err != nil {
		t.Fatal(err)
	}
	page := result.(MarketPage)
	if len(page.Records) != 1 || len(page.Records[0].SwapIDs) != 1 || page.Records[0].SwapIDs[0] != swap.ID {
		t.Fatal("closed own query lost exact archived maker context", page.Total)
	}

	for _, kind := range []string{"offers", "order_records", "offer_towers"} {
		if _, found, err := e.archiveRecord(kind, offer.ID); err != nil || !found {
			t.Fatal("retired order companion not cold", kind, err)
		}
	}
	if _, hot := e.s.OrderRecords[offer.ID]; hot {
		t.Fatal("lifetime order metadata remained hot")
	}
	var coldRecord OrderRecord
	if found, err := e.archivedValue("order_records", offer.ID, &coldRecord); err != nil || !found || coldRecord.Protection == nil || coldRecord.Protection.BPS != 37 || coldRecord.Protection.Scripts[chain.BTC] != policy.Scripts[chain.BTC] {
		t.Fatal("exact order protection context lost", err)
	}
	if _, hot := e.s.OfferTowers[offer.ID]; hot {
		t.Fatal("retired protection remains hot")
	}
	var loaded State
	if _, err := e.vault.Load(&loaded); err != nil {
		t.Fatal(err)
	}
	e.s = loaded
	if e.finishedOrder(offer.ID) == "" {
		t.Fatal("reloaded checkpoint lost terminal identity")
	}
	if _, err := e.activateArchived("swaps", swap.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Swaps[swap.ID].Stage = "awaiting chain confirmations"
	if e.finishedOrder(offer.ID) != "" {
		t.Fatal("cold terminal companion overrode live reactivation")
	}
	if _, err := e.orderSource(OrderActionFields{OrderAction: "recreate", SourceOfferID: offer.ID, SourceEventID: e.s.OrderRecords[offer.ID].EventID}, time.Now().Unix()); err == nil {
		t.Fatal("reactivated obligation granted recreation")
	}
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	delete(e.s.Swaps, swap.ID)
	if _, err := e.finishedOrderChecked(offer.ID); err == nil {
		t.Fatal("unreadable cold order silently supplied completion")
	}
}
