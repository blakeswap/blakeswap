package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/wire"
)

// Run this matrix once per adapter with the same isolated node fixture. The
// ordinary suite must skip before opening a wallet, relay, or RPC connection.
// No t.Parallel: every subtest mines the shared private chains.
func TestRealPartialFillConcurrentMixedOutcomes(t *testing.T) {
	if os.Getenv("BLAKESWAP_REGTEST") == "" {
		t.Skip("requires the exclusively leased BTC/Blake regtest fixture")
	}
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, bps := range []int64{0, 50} {
			t.Run(fmt.Sprintf("sell-%s/towers-%d", sell, bps), func(t *testing.T) {
				runRealPartialFillPair(t, sell, bps)
			})
		}
	}
}

// Add wallets through normal startup, first with RPC so the fixture owns their
// watch-only node wallets, then use the same selected adapter as newHarness.
// The parent receives exactly three independent confirmed 610000-sat coins;
// it cannot accidentally assign the harness's original large coin to fill one.
func partialFixtureWallet(h *harness, name string, deposits map[chain.ID][]int64) {
	h.t.Helper()
	cfg := h.configs["maker"]
	selectedNodes := cfg.Nodes
	cfg.Name, cfg.DataDir = name, h.t.TempDir()
	cfg.PasswordFile = filepath.Join(cfg.DataDir, "pass")
	cfg.Socket = filepath.Join(cfg.DataDir, "daemon.sock")
	if err := os.WriteFile(cfg.PasswordFile, []byte(transport.RandomID()), 0600); err != nil {
		h.t.Fatal(err)
	}
	cfg.Nodes = map[chain.ID]NodeConfig{}
	for id, rpc := range h.nodes {
		cfg.Nodes[id] = NodeConfig{URL: rpc.URL, Cookie: rpc.Cookie}
	}
	h.configs[name] = cfg
	h.online(name)
	watchWallet := "blakeswap-" + h.engines[name].Status().PubKey[:20]
	h.t.Cleanup(func() {
		h.offline(name)
		for _, rpc := range h.nodes {
			if err := rpc.Call(context.Background(), "unloadwallet", nil, watchWallet); err != nil {
				h.t.Error("unload partial fixture wallet", err)
			}
		}
	})
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, amount := range deposits[id] {
			h.command(name, "regtest.faucet", map[string]any{"chain": id, "amount": amount})
		}
		h.mine(id, 2)
	}
	h.tick(name)
	if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1" {
		h.offline(name)
		cfg.Nodes = selectedNodes
		h.configs[name] = cfg
		h.online(name)
		tickUntilConnected(h.t, h.engines[name])
	}
}

func partialReviewedTake(h *harness, name string, offer protocol.Offer, quantity, bps int64) (string, ConfirmTradeRequest, nostr.Event) {
	h.t.Helper()
	request := TradeQuoteRequest{Kind: "taker", ExpectedWallet: name, ExpectedNetwork: "regtest", Maker: offer.Maker, ID: offer.ID, TowerBPS: bps, FeeSelection: FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000}, FillTakeFields: FillTakeFields{Quantity: quantity, ParentRevision: offer.Revision}}
	quote := h.command(name, "trade.quote", request).(TradeQuote)
	buy, err := protocol.RoundedBuy(offer.SellAmount, offer.BuyAmount, quantity)
	if err != nil || !quote.Ready || quote.Error != "" || quote.Quantity != quantity || quote.ParentRevision != offer.Revision || quote.PaidPrincipal != buy || quote.ReceivedPrincipal != quantity || len(quote.FeeBudgets) != 2 || len(quote.BountyBudgets) != 2 {
		h.t.Fatal("partial review did not bind exact child economics", quote, err)
	}
	confirm := confirmation(quote)
	result := h.command(name, "trade.confirm", confirm).(ConfirmTradeResult)
	if result.State != "accepted" || result.Error != "" {
		h.t.Fatal("partial take confirmation", result)
	}
	swap := h.swap(name, result.ID)
	if swap.Request.OfferEvent.ID.Hex() != quote.OfferEventID || swap.Request.Quantity != quantity || swap.Request.Revision != offer.Revision {
		h.t.Fatal("saved request differs from the reviewed event/quantity/revision")
	}
	for _, delivery := range h.engines[name].s.Outbox {
		if delivery.Type == "request" && delivery.SwapID == result.ID {
			if delivery.Published {
				h.t.Fatal("request published before explicit dispatch")
			}
			return result.ID, confirm, delivery.Event
		}
	}
	h.t.Fatal("confirmed child has no durable encrypted request")
	return "", ConfirmTradeRequest{}, nostr.Event{}
}

// Concurrent ingress uses the exact encrypted events produced by trade.confirm
// and the production receive path under its normal Engine.mu ownership. Holding
// dispatch until restart makes durable-before-ACK directly observable. Later
// relay delivery replays these same events through the ordinary worker.
func partialReceiveRace(h *harness, events []nostr.Event) {
	h.t.Helper()
	parent := h.engines["parent"]
	start := make(chan struct{})
	errors := make(chan error, len(events))
	var joined sync.WaitGroup
	for _, event := range events {
		joined.Add(1)
		go func(event nostr.Event) {
			defer joined.Done()
			<-start
			parent.mu.Lock()
			err := parent.receive(event)
			parent.mu.Unlock()
			errors <- err
		}(event)
	}
	close(start)
	joined.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			h.t.Fatal("authenticated partial request ingress", err)
		}
	}
}

func partialBins(h *harness, parentID string, available, reserved, committed, filled, released, withdrawn int64) {
	h.t.Helper()
	p := h.engines["parent"].s.ParentOrders[parentID]
	if p == nil || p.Quantities.validate() != nil {
		h.t.Fatal("missing or unconserved parent")
	}
	q := p.Quantities
	if q.Total != 1800000 || q.Available != available || q.Reserved != reserved || q.Committed != committed || q.Filled != filled || q.Released != released || q.Withdrawn != withdrawn {
		h.t.Fatalf("unexpected actual parent quantities: %+v", q)
	}
}

func partialWait(h *harness, label string, ready func() bool, tick func()) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ready() && time.Now().Before(deadline) {
		tick()
		if !ready() {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ready() {
		for name, e := range h.engines {
			if e != nil {
				h.t.Log(name, e.Status().LastError, e.Status().Swaps)
			}
		}
		h.t.Fatal("did not reach", label)
	}
}

func partialMineTo(h *harness, id chain.ID, height uint32) {
	h.t.Helper()
	for current := h.height(id); current < height; current = h.height(id) {
		// Preserve exact target and normal node deadline; bounded fixture batches
		// avoid a deep Blake administrative call outlasting that deadline.
		h.mine(id, min(uint32(16), height-current))
	}
}

func runRealPartialFillPair(t *testing.T, sell chain.ID, bps int64) {
	h := newHarness(t, bps)
	partialFixtureWallet(h, "parent", map[chain.ID][]int64{sell: {610000, 610000, 610000}})
	partialFixtureWallet(h, "second", map[chain.ID][]int64{chain.BTC: {100000000}, chain.Blake: {100000000}})
	const total, price, quantity int64 = 1800000, 2340001, 600000
	buy, err := protocol.RoundedBuy(total, price, quantity)
	if err != nil || buy != 780001 {
		t.Fatal("awkward-ratio fixture arithmetic", buy, err)
	}
	create := TradeQuoteRequest{Kind: "maker", ExpectedWallet: "parent", ExpectedNetwork: "regtest", Sell: sell, SellAmount: total, BuyAmount: price, TowerBPS: bps, FeeSelection: FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000}, FillOrderFields: FillOrderFields{FillPolicy: protocol.FillPolicy{Mode: protocol.FillPartial, Min: 400000, Max: quantity}, FeeBudgets: map[chain.ID]int64{sell: 4 * 26500, sell.Other(): 4 * 20000}, BountyBudgets: map[chain.ID]int64{sell: 4 * protocol.Bounty(quantity, bps), sell.Other(): 4 * protocol.Bounty(buy, bps)}}}
	quote := h.command("parent", "trade.quote", create).(TradeQuote)
	if !quote.Ready || quote.Error != "" || len(quote.Funds.Inputs) != 3 {
		t.Fatal("parent review must own three real independent inputs", quote)
	}
	created := h.command("parent", "trade.confirm", confirmation(quote)).(ConfirmTradeResult)
	if created.State != "accepted" || created.Error != "" {
		t.Fatal("parent confirmation", created)
	}
	parentID := created.ID
	h.offline("parent")
	h.online("parent")
	h.tick("parent")
	parentKey := h.engines["parent"].identity.Public().Hex()
	bookKey := parentKey + ":" + parentID
	partialWait(h, "both takers observe revision one", func() bool {
		return h.engines["taker"].s.Book[bookKey].ID != (nostr.ID{}) && h.engines["second"].s.Book[bookKey].ID != (nostr.ID{})
	}, func() { h.tick("taker", "second") })
	offer, err := protocol.DecodeOffer(h.engines["taker"].s.Book[bookKey], time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"taker", "second"}
	ids := make([]string, 2)
	confirms := make([]ConfirmTradeRequest, 2)
	events := make([]nostr.Event, 2)
	for i, name := range names {
		ids[i], confirms[i], events[i] = partialReviewedTake(h, name, offer, quantity, bps)
		h.offline(name)
	}
	partialReceiveRace(h, events)
	partialBins(h, parentID, 1200000, quantity, 0, 0, 0, 0)
	if len(h.engines["parent"].s.Swaps) != 1 || len(h.engines["parent"].s.FillRecords) != 1 {
		t.Fatal("same-revision race admitted more than one independent child")
	}
	winner := 0
	if h.engines["parent"].s.Swaps[ids[0]] == nil {
		winner = 1
	}
	loser := 1 - winner
	acceptedBefore := protocol.Digest(h.engines["parent"].s.Swaps[ids[winner]].Terms)
	var acceptedDelivery string
	for _, d := range h.engines["parent"].s.Outbox {
		if d.Type == "accepted" && d.SwapID == ids[winner] {
			if d.Published || d.Acknowledged {
				t.Fatal("acceptance dispatched before restart boundary")
			}
			acceptedDelivery = d.Event.ID.Hex()
		}
	}
	if acceptedDelivery == "" {
		t.Fatal("allocation has no durable acceptance")
	}
	h.offline("parent")
	h.online("parent")
	if protocol.Digest(h.swap("parent", ids[winner]).Terms) != acceptedBefore {
		t.Fatal("restart changed accepted contract")
	}
	foundAcceptance := false
	for _, d := range h.engines["parent"].s.Outbox {
		foundAcceptance = foundAcceptance || (d.Type == "accepted" && d.Event.ID.Hex() == acceptedDelivery && !d.Published)
	}
	if !foundAcceptance {
		t.Fatal("restart lost exact unpublished acceptance")
	}
	// Retry ingress before dispatch: the saved bins, key registry and immutable
	// terms remain identical. Only a missing ACK may be queued again.
	beforeRetry := protocol.Digest(h.engines["parent"].s.ParentOrders[parentID])
	partialReceiveRace(h, events)
	if protocol.Digest(h.engines["parent"].s.ParentOrders[parentID]) != beforeRetry {
		t.Fatal("exact request retry allocated quantity or money twice")
	}
	h.online(names[loser])
	partialWait(h, "loser receives rejection and current public revision", func() bool {
		p := h.engines["parent"].s.ParentOrders[parentID]
		remote, err := protocol.DecodeOffer(h.engines[names[loser]].s.Book[bookKey], time.Now().Unix())
		return h.swap(names[loser], ids[loser]).Stage == "rejected" && err == nil && p.SignedRevision == p.Quantities.Revision && remote.Revision == p.Quantities.Revision
	}, func() { h.tick("parent", names[loser]) })
	// An old q/revision cannot silently turn into another authorization.
	stale := TradeQuoteRequest{Kind: "taker", ExpectedWallet: names[loser], ExpectedNetwork: "regtest", Maker: parentKey, ID: parentID, TowerBPS: bps, FeeSelection: create.FeeSelection, FillTakeFields: FillTakeFields{Quantity: quantity, ParentRevision: offer.Revision}}
	raw, _ := json.Marshal(stale)
	if q, err := h.engines[names[loser]].Command(h.ctx, Request{Method: "trade.quote", Params: raw}); err == nil && q.(TradeQuote).Ready {
		t.Fatal("stale revision produced an executable review")
	}
	fresh, err := protocol.DecodeOffer(h.engines[names[loser]].s.Book[bookKey], time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	oldRejectedID := ids[loser]
	ids[loser], confirms[loser], events[loser] = partialReviewedTake(h, names[loser], fresh, quantity, bps)
	if ids[loser] == oldRejectedID {
		t.Fatal("fresh authorization reused a rejected child identity")
	}
	h.offline(names[loser])
	partialReceiveRace(h, []nostr.Event{events[loser]})
	h.offline("parent")
	h.online("parent")
	partialBins(h, parentID, quantity, 2*quantity, 0, 0, 0, 0)
	if len(h.engines["parent"].s.Swaps) != 2 {
		t.Fatal("second independently accepted child missing")
	}
	childrenBefore := protocol.Digest(h.engines["parent"].s.FillRecords)
	feesBefore := protocol.Digest(h.engines["parent"].s.ParentOrders[parentID].Fees)
	h.command("parent", "offer.cancel", map[string]string{"id": parentID})
	partialBins(h, parentID, 0, 2*quantity, 0, 0, quantity, quantity)
	if protocol.Digest(h.engines["parent"].s.FillRecords) != childrenBefore || protocol.Digest(h.engines["parent"].s.ParentOrders[parentID].Fees) != feesBefore {
		t.Fatal("cancellation changed accepted children or reserved fee authorization")
	}
	if _, locked := h.engines["parent"].s.CoinReservations["offer/"+parentID]; locked {
		t.Fatal("cancelled available input pool remains locked")
	}
	partialAssertIndependentChildren(h, ids)
	h.tick("parent")
	h.offline("parent")
	for i, name := range names {
		h.online(name)
		partialWait(h, "independent long funding", func() bool { return h.swap(name, ids[i]).LongSent }, func() { h.tick(name, "tower") })
		s := h.swap(name, ids[i])
		if !towerReady(s) || (bps == 0 && len(s.Jobs) != 0) || (bps > 0 && len(s.Jobs) != 1) {
			t.Fatal("taker funding lacks its own exact refund protection receipt")
		}
		for _, job := range s.Jobs {
			retained := h.engines["tower"].s.TowerJobs[job.ID]
			if job.SwapID != ids[i] || job.TermsHash != protocol.Digest(s.Terms) || job.Validate(s.protection().Scripts, bps) != nil || retained == nil || protocol.Digest(retained.Job) != protocol.Digest(job) {
				t.Fatal("taker tower acknowledgment substituted another child or template")
			}
		}
		h.offline(name)
	}
	h.minePending()
	h.online("parent")
	partialWait(h, "both short fundings after cancellation", func() bool { return h.swap("parent", ids[0]).ShortSent && h.swap("parent", ids[1]).ShortSent }, func() { h.tick("parent", "tower") })
	h.minePending()
	h.tick("parent")
	partialBins(h, parentID, 0, 0, 2*quantity, 0, quantity, quantity)
	partialAssertFundedChildren(h, ids, bps)
	h.offline("parent")
	// Only the winning taker returns while either reveal window is open.
	h.online(names[winner])
	partialWait(h, "first child owner claim", func() bool {
		return h.swap(names[winner], ids[winner]).SecretExposed && h.swap(names[winner], ids[winner]).SelfClaim != ""
	}, func() { h.tick(names[winner], "tower") })
	h.minePending()
	h.online("parent")
	partialWait(h, "first child complete", func() bool { return h.swap("parent", ids[winner]).Stage == "completed" }, func() { h.tick("parent", names[winner], "tower"); h.minePending() })
	partialBins(h, parentID, 0, 0, quantity, quantity, quantity, quantity)
	refunding := h.swap("parent", ids[loser])
	if refunding.Secret != "" || refunding.SecretExposed || refunding.Request.Hash == h.swap("parent", ids[winner]).Request.Hash {
		t.Fatal("completed child's secret leaked into unrelated sibling")
	}
	for _, c := range []contract.HTLC{refunding.Long, refunding.Short} {
		out, err := h.nodes[c.Chain].Output(h.ctx, c.TxID, c.Vout)
		if err != nil || out == nil {
			t.Fatal("first child completion consumed sibling contract", err)
		}
	}
	partialReorgCompletedChild(h, parentID, ids[winner], ids[loser], names[winner])
	h.offline("parent")
	h.offline(names[winner])
	if bps > 0 {
		partialMineTo(h, refunding.Short.Chain, refunding.Short.RefundHeight+protocol.RefundGrace)
		h.tick("tower")
		h.minePending()
		partialMineTo(h, refunding.Long.Chain, refunding.Long.RefundHeight+protocol.RefundGrace)
		h.tick("tower")
		h.minePending()
	} else {
		partialMineTo(h, refunding.Long.Chain, refunding.Long.RefundHeight)
		partialMineTo(h, refunding.Short.Chain, refunding.Short.RefundHeight)
		h.online(names[loser])
		h.tick(names[loser])
		h.minePending()
	}
	h.online("parent")
	h.online(names[loser])
	partialWait(h, "second child refunded with first still complete", func() bool {
		return h.swap("parent", ids[loser]).Stage == "refunded" && h.swap(names[loser], ids[loser]).Stage == "refunded"
	}, func() { h.tick("parent", names[loser], "tower"); h.minePending() })
	partialBins(h, parentID, 0, 0, 0, quantity, 2*quantity, quantity)
	parent := h.engines["parent"].s.ParentOrders[parentID]
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		var fees, bounties int64
		for _, id := range ids {
			fees += h.engines["parent"].s.FillRecords[id].Fees[asset]
			bounties += h.engines["parent"].s.FillRecords[id].Bounties[asset]
		}
		if parent.Fees[asset].Reserved != 0 || parent.Fees[asset].Consumed != fees || parent.Fees[asset].Limit != create.FeeBudgets[asset] || parent.Bounties[asset].Reserved != 0 || parent.Bounties[asset].Consumed != bounties || parent.Bounties[asset].Limit != create.BountyBudgets[asset] {
			t.Fatal("mixed outcomes refunded or duplicated permanent monetary authorization")
		}
	}
	if h.swap("parent", ids[winner]).Stage != "completed" || h.swap("parent", ids[loser]).SecretExposed {
		t.Fatal("mixed child outcomes lost settlement or secret isolation")
	}
	partialAssertSettlements(h, names, ids, winner, bps)
	finalParent := protocol.Digest(h.engines["parent"].s.ParentOrders[parentID])
	h.offline("parent")
	h.online("parent")
	for i, name := range names {
		h.offline(name)
		h.online(name)
		result := h.command(name, "trade.confirm", confirms[i]).(ConfirmTradeResult)
		if result.ID != ids[i] || result.State != "accepted" || result.Error != "" {
			t.Fatal("exact saved confirmation retry changed child", result)
		}
	}
	partialReceiveRace(h, events)
	if protocol.Digest(h.engines["parent"].s.ParentOrders[parentID]) != finalParent {
		t.Fatal("restart or terminal replay changed conserved bins or permanent charges")
	}
	page := h.command("parent", "fills.list", FillQuery{ExpectedWallet: "parent", ExpectedNetwork: "regtest", ParentMaker: parentKey, ParentID: parentID, Limit: 1}).(FillPage)
	if page.Total != 2 || len(page.Records) != 1 || !page.More || page.Revision == "" {
		t.Fatal("bounded parent history first page", page)
	}
	next := h.command("parent", "fills.list", FillQuery{ExpectedWallet: "parent", ExpectedNetwork: "regtest", ParentMaker: parentKey, ParentID: parentID, Limit: 1, Offset: page.NextOffset, Revision: page.Revision}).(FillPage)
	if next.Total != 2 || len(next.Records) != 1 || next.More || page.Records[0].ID == next.Records[0].ID {
		t.Fatal("bounded parent history second page", next)
	}
	for _, row := range append(page.Records, next.Records...) {
		want := FillReleased
		if row.ID == ids[winner] {
			want = FillFilled
		}
		if row.ParentMaker != parentKey || row.ParentID != parentID || row.Quantity != quantity || row.BuyAmount != buy || !row.AllocationKnown || row.AllocatedQuantity != quantity || row.Disposition != want {
			t.Fatal("linked child history lost exact quantity/identity/outcome", row)
		}
	}
	t.Logf("actual partial matrix sell=%s towers=%d electrum=%t: two independent accepted children; filled=%d refunded=%d withdrawn=%d; restart/retry/history preserved", sell, bps, os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "1", quantity, quantity, quantity)
}

func partialAssertIndependentChildren(h *harness, ids []string) {
	h.t.Helper()
	keys := map[string]bool{}
	inputs := map[string]bool{}
	for _, id := range ids {
		s := h.swap("parent", id)
		child := h.engines["parent"].s.FillRecords[id]
		if child == nil || s.Terms == nil || child.RequestDigest != protocol.Digest(s.Request) || child.ParentRevision != s.Request.Revision || child.Allocation.Quantity != 600000 || child.BuyAmount != 780001 || len(child.Inputs) != 1 {
			h.t.Fatal("child lacks exact accepted identity/input ownership")
		}
		identity, err := fillIdentityKeys(s.Request, s.Terms.MakerKeys)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, key := range identity {
			if keys[key] || h.engines["parent"].s.FillKeys[key] != id {
				h.t.Fatal("children share a hash/key or lost durable identity index")
			}
			keys[key] = true
		}
		for _, point := range child.Inputs {
			if inputs[pointKey(point)] {
				h.t.Fatal("children share a funding input")
			}
			inputs[pointKey(point)] = true
		}
	}
}

func partialConfirmedTransaction(h *harness, id chain.ID, txid string) *wire.MsgTx {
	h.t.Helper()
	actual, err := h.nodes[id].Transaction(h.ctx, txid)
	if err != nil || actual.TxID != txid || actual.Confirmations < protocol.Confirmations || actual.BlockHash == "" {
		h.t.Fatalf("actual transaction %s/%s lacks confirmed inclusion: confirmations=%d error=%v", id, txid, actual.Confirmations, err)
	}
	tx, err := contract.Parse(actual.Hex)
	if err != nil || tx.TxHash().String() != txid {
		h.t.Fatal("actual raw transaction identity", err)
	}
	return tx
}

func partialAssertFundedChildren(h *harness, ids []string, bps int64) {
	h.t.Helper()
	used := map[string]bool{}
	jobs := map[string]bool{}
	for _, id := range ids {
		s := h.swap("parent", id)
		child := h.engines["parent"].s.FillRecords[id]
		for _, c := range []contract.HTLC{s.Long, s.Short} {
			tx := partialConfirmedTransaction(h, c.Chain, c.TxID)
			pk, err := c.PkScript()
			if err != nil || int(c.Vout) >= len(tx.TxOut) || tx.TxOut[c.Vout].Value != c.Amount || !bytes.Equal(tx.TxOut[c.Vout].PkScript, pk) || len(tx.TxIn) != 1 {
				h.t.Fatal("actual funded contract or input count changed", err)
			}
			var inputTotal, outputTotal int64
			for _, in := range tx.TxIn {
				key := string(c.Chain) + "/" + in.PreviousOutPoint.String()
				if used[key] {
					h.t.Fatal("actual funding reused a sibling's input")
				}
				used[key] = true
				prev := partialConfirmedTransaction(h, c.Chain, in.PreviousOutPoint.Hash.String())
				if int(in.PreviousOutPoint.Index) >= len(prev.TxOut) {
					h.t.Fatal("actual funding prevout out of range")
				}
				inputTotal += prev.TxOut[in.PreviousOutPoint.Index].Value
				if c.Chain == s.Short.Chain && pointKey(child.Inputs[0]) != chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index) {
					h.t.Fatal("maker funding substituted an unassigned input")
				}
			}
			for _, output := range tx.TxOut {
				outputTotal += output.Value
			}
			if inputTotal-outputTotal != 6500 {
				h.t.Fatal("actual selected funding fee changed", inputTotal-outputTotal)
			}
		}
		if s.Short.Amount != 600000 || s.Long.Amount != 780001 || !towerReady(s) || (bps == 0 && len(s.Jobs) != 0) || (bps > 0 && len(s.Jobs) != 2) {
			h.t.Fatal("child principal or maker receipt policy mismatch")
		}
		for _, job := range s.Jobs {
			if jobs[job.ID] || job.SwapID != id || job.TermsHash != protocol.Digest(s.Terms) || job.Validate(s.protection().Scripts, bps) != nil {
				h.t.Fatal("cross-child tower job identity")
			}
			jobs[job.ID] = true
			stored := h.engines["tower"].s.TowerJobs[job.ID]
			if stored == nil || protocol.Digest(stored.Job) != protocol.Digest(job) {
				h.t.Fatal("actual tower did not retain exact acknowledged job")
			}
		}
	}
}

func partialReorgCompletedChild(h *harness, parentID, completed, sibling, taker string) {
	h.t.Helper()
	e := h.engines["parent"]
	s := h.swap("parent", completed)
	actual, err := h.nodes[s.Long.Chain].Transaction(h.ctx, s.LongSpend)
	if err != nil || actual.Confirmations < 2 || actual.BlockHash == "" {
		h.t.Fatal("reorg fixture lacks original confirmed claim", err)
	}
	node := h.nodes[s.Long.Chain]
	block := actual.BlockHash
	// Register exact-block restoration before mutation, including assertion and
	// administrative-call failures. No fresh chain or discarded fixture state.
	restore := func() {
		if err := archiveFixtureBlockCommand(h.t, node, "reconsiderblock", block); err != nil {
			h.t.Error("restore partial-fill claim block", err)
			return
		}
		var canonical string
		if err := node.Call(h.ctx, "getblockhash", &canonical, actual.Height); err != nil || canonical != block {
			h.t.Error("partial-fill claim block cleanup is not canonical", err)
		}
	}
	h.t.Cleanup(restore)
	childBefore := protocol.Digest(e.s.FillRecords[sibling])
	feesBefore := protocol.Digest(e.s.ParentOrders[parentID].Fees)
	bountiesBefore := protocol.Digest(e.s.ParentOrders[parentID].Bounties)
	secretBefore := s.Secret
	if err := archiveFixtureBlockCommand(h.t, node, "invalidateblock", block); err != nil {
		h.t.Fatal(err)
	}
	h.tick("parent", taker)
	partialBins(h, parentID, 0, 0, 1200000, 0, 600000, 600000)
	if h.swap("parent", completed).Secret != secretBefore || secretBefore == "" || !h.swap("parent", completed).SecretExposed || protocol.Digest(e.s.FillRecords[sibling]) != childBefore || protocol.Digest(e.s.ParentOrders[parentID].Fees) != feesBefore || protocol.Digest(e.s.ParentOrders[parentID].Bounties) != bountiesBefore {
		h.t.Fatal("settled-child reorg changed sibling, secret, or permanent charges")
	}
	restore()
	partialWait(h, "same confirmed child after exact block restoration", func() bool { return h.swap("parent", completed).Stage == "completed" }, func() { h.tick("parent", taker) })
	partialBins(h, parentID, 0, 0, 600000, 600000, 600000, 600000)
	_ = partialConfirmedTransaction(h, s.Long.Chain, s.LongSpend)
}

func partialAssertSettlements(h *harness, names, ids []string, winner int, bps int64) {
	h.t.Helper()
	for i, id := range ids {
		s := h.swap("parent", id)
		for _, leg := range []struct {
			c    contract.HTLC
			txid string
		}{{s.Long, s.LongSpend}, {s.Short, s.ShortSpend}} {
			tx := partialConfirmedTransaction(h, leg.c.Chain, leg.txid)
			if err := contract.VerifyObservedSpend(leg.c, tx, nil); err != nil {
				h.t.Fatal("actual settlement does not spend agreed contract", err)
			}
			if len(tx.TxIn) != 1 {
				h.t.Fatal("fixture owner/tower transaction unexpectedly aggregated inputs")
			}
			refund := i != winner
			selector := tx.TxIn[0].Witness[len(tx.TxIn[0].Witness)-2]
			if (len(selector) == 0) != refund {
				h.t.Fatal("actual settlement branch differs from claimed/refunded outcome")
			}
			owner := "parent"
			if (refund && leg.c.Chain == s.Long.Chain) || (!refund && leg.c.Chain == s.Short.Chain) {
				owner = names[i]
			}
			// Winning taker was offline during refunds; opening it here does not
			// tick or grant a second execution, and exposes its stable payout script.
			h.online(owner)
			bounty := int64(0)
			if refund && bps > 0 {
				bounty = protocol.Bounty(leg.c.Amount, bps)
			}
			var total int64
			for _, output := range tx.TxOut {
				total += output.Value
			}
			fee := leg.c.Amount - total
			validFee := false
			for _, authorized := range protocol.RescueFees {
				validFee = validFee || fee == authorized
			}
			if !validFee || !bytes.Equal(tx.TxOut[0].PkScript, h.engines[owner].scripts[leg.c.Chain]) || tx.TxOut[0].Value != leg.c.Amount-fee-bounty {
				h.t.Fatal("actual principal/net payout/owner fee mismatch")
			}
			if bounty == 0 && len(tx.TxOut) != 1 {
				h.t.Fatal("self settlement paid an unauthorized output")
			}
			if bounty > 0 {
				script, err := hex.DecodeString(s.protection().Scripts[leg.c.Chain])
				if err != nil || len(tx.TxOut) != 2 || tx.TxOut[1].Value != bounty || !bytes.Equal(tx.TxOut[1].PkScript, script) {
					h.t.Fatal("actual tower payout differs from exact authorization", err)
				}
			}
			out, err := h.nodes[leg.c.Chain].Output(h.ctx, leg.c.TxID, leg.c.Vout)
			if err != nil || out != nil {
				h.t.Fatal("settled contract remains unspent", err)
			}
			h.t.Logf("child=%s chain=%s refund=%t principal=%d funding_fee=6500 settlement_fee=%d bounty=%d net=%d", id, leg.c.Chain, refund, leg.c.Amount, fee, bounty, tx.TxOut[0].Value)
		}
	}
}
