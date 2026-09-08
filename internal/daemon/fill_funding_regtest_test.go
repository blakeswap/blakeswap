package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/btcsuite/btcd/wire"
)

// Run serially, once per selected RPC/Electrum adapter, under the private node
// fixture lease. Ordinary execution skips before opening any wallet or endpoint.
func TestRealFundingChangeAncestry(t *testing.T) {
	if os.Getenv("BLAKESWAP_REGTEST") == "" {
		t.Skip("requires the exclusively leased BTC/Blake regtest fixture")
	}
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, role := range []string{"maker", "taker"} {
			t.Run(fmt.Sprintf("sell-%s/local-%s", sell, role), func(t *testing.T) {
				runRealFundingChangeAncestry(t, sell, role)
			})
		}
	}
}

const ancestryPrincipal int64 = 1000000
const ancestryFundingFee int64 = 6500

type ancestryTrade struct {
	id, parent, maker, taker            string
	makerConfirm, takerConfirm          ConfirmTradeRequest
	longPublications, shortPublications map[string]partialPublication
}

// Only transaction lookup is unavailable. The selected adapter still supplies
// its real tip, canonical hashes, scanner and source generation. Never invent a
// transaction, confirmation, missing-output result or successful proof.
type ancestryLookupOutage struct {
	*chain.Failover
	ids map[string]bool
}

func (n *ancestryLookupOutage) Transaction(ctx context.Context, id string) (chain.Transaction, error) {
	if n.ids[id] {
		return chain.Transaction{}, errors.New("isolated ancestry transaction lookup unavailable")
	}
	return n.Failover.Transaction(ctx, id)
}

func runRealFundingChangeAncestry(t *testing.T, sell chain.ID, role string) {
	h := newHarness(t, 0)
	const local = "lineage"
	paid := sell
	if role == "taker" {
		paid = sell.Other()
	}
	// Prefund every peer before creating deadlines. D's exact input leaves no
	// change, so the later independent deposit is A's only possible input.
	for _, peer := range []string{"peer-a", "peer-b", "peer-c", "peer-d"} {
		partialFixtureWallet(h, peer, map[chain.ID][]int64{chain.BTC: {100000000}, chain.Blake: {100000000}})
		h.offline(peer)
	}
	partialFixtureWallet(h, local, map[chain.ID][]int64{paid: {ancestryPrincipal + ancestryFundingFee}})
	d := ancestryFundTrade(h, local, "peer-d", sell, role, true)
	dOwn, _, _ := localFunding(h.swap(local, d.id))
	dTx := partialConfirmedTransaction(h, paid, dOwn.TxID)
	if len(dTx.TxOut) != 1 || len(h.swap(local, d.id).FundingParents) != 0 {
		t.Fatal("unrelated D must consume its exact independent input without change or ancestry")
	}
	h.command(local, "regtest.faucet", map[string]any{"chain": paid, "amount": int64(100000000)})
	h.mine(paid, 2)
	h.tick(local)
	a := ancestryFundTrade(h, local, "peer-a", sell, role, false)
	b := ancestryFundTrade(h, local, "peer-b", sell, role, false)
	ancestryComplete(h, b)
	h.offline("peer-b")
	c := ancestryFundTrade(h, local, "peer-c", sell, role, true)
	trades := []ancestryTrade{a, b, c, d}
	ancestryAssertEdge(h, local, a.id, "")
	ancestryAssertEdge(h, local, b.id, a.id)
	ancestryAssertEdge(h, local, c.id, b.id)
	ancestryAssertEdge(h, local, d.id, "")
	if h.swap(local, c.id).SecretExposed || h.swap(local, c.id).SelfClaim != "" {
		t.Fatal("C revealed before the withheld peer funded/revealed")
	}
	before := ancestryStableState(h, local, trades)
	knownSecret := protocol.Digest(h.swap(local, b.id).Secret)
	assertKnownSecret := func() {
		core := ancestryCore(h, local, b.id)
		if core.Secret == "" || !core.SecretObserved || protocol.Digest(core.Secret) != knownSecret {
			t.Fatal("funding ancestry lost the completed child's known preimage")
		}
	}
	assertKnownSecret()
	// Open must retain exact graph/bytes, but cannot reuse the old process proof.
	h.offline(local)
	h.online(local)
	for _, id := range []string{b.id, c.id} {
		if !h.swap(local, id).FundingAncestryHeld {
			t.Fatal("restart reused an ancestry proof")
		}
	}
	ancestryWaitProof(h, local, b.id, c.id)
	ancestryAssertStable(h, local, trades, before)
	ancestryRetry(h, local, trades)

	bOwn, _, _ := localFunding(h.swap(local, b.id))
	cOwn, _, _ := localFunding(h.swap(local, c.id))
	e := h.engines[local]
	original, ok := e.nodes[paid].(*chain.Failover)
	if !ok {
		t.Fatal("fixture must exercise the selected production failover adapter")
	}
	e.nodes[paid] = &ancestryLookupOutage{Failover: original, ids: map[string]bool{bOwn.TxID: true, cOwn.TxID: true}}
	t.Cleanup(func() { e.nodes[paid] = original })
	h.tick(local)
	ancestryAssertHeld(h, local, b, c, d, before, false)
	assertKnownSecret()
	e.nodes[paid] = original
	ancestryWaitProof(h, local, b.id, c.id)
	ancestryAssertStable(h, local, trades, before)

	aOwn, _, _ := localFunding(h.swap(local, a.id))
	anchor, err := h.nodes[paid].Transaction(h.ctx, aOwn.TxID)
	if err != nil || anchor.Height == 0 || anchor.BlockHash == "" || anchor.Confirmations < protocol.Confirmations {
		t.Fatal("A lacks actual confirmed funding inclusion", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := archiveFixtureBlockCommand(t, h.nodes[paid], "reconsiderblock", anchor.BlockHash); err != nil {
			t.Error("restore exact ancestor funding block", err)
			return
		}
		var canonical string
		if err := h.nodes[paid].Call(h.ctx, "getblockhash", &canonical, anchor.Height); err != nil || canonical != anchor.BlockHash {
			t.Error("ancestor cleanup did not restore the exact canonical block", err)
			return
		}
		restored = true
	}
	t.Cleanup(restore) // before the first mutation, including failed assertions
	if err := archiveFixtureBlockCommand(t, h.nodes[paid], "invalidateblock", anchor.BlockHash); err != nil {
		t.Fatal(err)
	}
	// A deliberate reorg may invalidate an Electrum observation generation.
	// Finish the existing bounded full refresh before checking the held graph.
	tickUntilConnected(t, h.engines[local])
	ancestryAssertHeld(h, local, b, c, d, before, true)
	assertKnownSecret()
	// D progresses in this same wallet while B/C remain held. Do not mine the
	// invalidated chain: that would reconfirm the descendants being tested.
	if role == "maker" {
		h.online(d.taker)
		partialWaitForPublications(h, d.taker, d.shortPublications, func() { h.tick(d.taker) })
		ancestryAssertRevealWindow(h, h.swap(d.taker, d.id))
		partialWait(h, "independent D first claim", func() bool { return h.swap(d.taker, d.id).SelfClaim != "" }, func() { h.tick(d.taker) })
		ancestryAssertBroadcastClaim(h, d.taker, h.swap(d.taker, d.id))
		partialWait(h, "independent local D witnessed rescue", func() bool {
			s := h.swap(local, d.id)
			return s.SecretObserved && s.SelfClaim != ""
		}, func() {
			h.tick(local)
			ancestryAssertHeld(h, local, b, c, d, before, true)
		})
		ancestryAssertBroadcastClaim(h, local, h.swap(local, d.id))
	} else {
		h.online(d.maker)
		partialWaitForPublications(h, d.maker, d.longPublications, func() { h.tick(d.maker) })
		partialWait(h, "independent D incoming funding", func() bool { return h.swap(d.maker, d.id).ShortSent }, func() { h.tick(d.maker) })
		h.mine(sell, 2) // other chain only; D's own paid chain remains invalidated
		ancestryAssertRevealWindow(h, h.swap(local, d.id))
		partialWait(h, "independent D first claim", func() bool { return h.swap(local, d.id).SelfClaim != "" }, func() { h.tick(local, d.maker) })
		ancestryAssertBroadcastClaim(h, local, h.swap(local, d.id))
	}
	ancestryAssertHeld(h, local, b, c, d, before, true)
	restore()
	if !restored {
		t.Fatal("exact ancestor restoration failed")
	}
	tickUntilConnected(t, h.engines[local])
	ancestryWaitProof(h, local, b.id, c.id)
	ancestryAssertStable(h, local, trades, before)
	for _, trade := range trades {
		ancestryComplete(h, trade)
	}
	ancestryRetry(h, local, trades)
	for _, trade := range trades {
		s := h.swap(local, trade.id)
		for _, leg := range []struct {
			c  contract.HTLC
			id string
		}{{s.Long, s.LongSpend}, {s.Short, s.ShortSpend}} {
			tx := partialConfirmedTransaction(h, leg.c.Chain, leg.id)
			funding := partialConfirmedTransaction(h, leg.c.Chain, leg.c.TxID)
			if err := contract.VerifyObservedSpend(leg.c, tx, []*wire.TxOut{funding.TxOut[leg.c.Vout]}); err != nil {
				t.Fatal("actual completed child signature", err)
			}
			secret, claim := contract.ExtractSecret(leg.c, tx)
			clear(secret)
			if !claim {
				t.Fatal("completed ancestry fixture used a refund")
			}
			owner := trade.taker
			if leg.c.Chain == s.Long.Chain {
				owner = trade.maker
			}
			feeOK := false
			if len(tx.TxOut) == 1 {
				for _, fee := range protocol.RescueFees {
					feeOK = feeOK || leg.c.Amount-tx.TxOut[0].Value == fee
				}
			}
			payout, err := partialRetainedPayout(h.engines[owner], ancestryCore(h, owner, trade.id), leg.c, tx, false, 0)
			if err != nil || !feeOK || !bytes.Equal(tx.TxOut[0].PkScript, payout) {
				t.Fatal("actual claim changed the reviewed owner payout or fee ladder")
			}
		}
	}
	settled := ancestryStableState(h, local, trades)
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		partialMineTo(h, id, h.height(id)+archiveSettlementDepth)
	}
	partialWait(h, "all four ancestry cores become cold", func() bool {
		for _, trade := range trades {
			if h.engines[local].s.Swaps[trade.id] != nil {
				return false
			}
		}
		return true
	}, func() { h.tick(local) })
	ancestryAssertStable(h, local, trades, settled)
	for _, trade := range trades {
		core := ancestryCore(h, local, trade.id)
		own, _, _ := localFunding(core)
		if _, hot := h.engines[local].s.FillKeys[fundingIdentityKey(own.Chain, own.TxID)]; hot {
			t.Fatal("cold core retained a hot funding identity")
		}
	}
	// Exact point reads and receipt retries must not promote cold authority.
	ancestryRetry(h, local, trades)
	if len(h.engines[local].s.Swaps) != 0 {
		t.Fatal("cold identity lookup or receipt retry reactivated history")
	}
	ancestryRestoreSeparate(h, local)
	for _, id := range []string{b.id, c.id} {
		if !ancestryCore(h, local, id).FundingAncestryHeld {
			t.Fatal("restored descendant reused exported ancestry proof")
		}
	}
	ancestryAssertStable(h, local, trades, settled)
	partialWait(h, "separate restored installation reconciles actual settled graph", func() bool { return h.engines[local].Status().Recovery.State == "ready" }, func() { h.tick(local) })
	ancestryAssertStable(h, local, trades, settled)
	ancestryRetry(h, local, trades)
	assertKnownSecret()
	t.Logf("actual lineage sell=%s role=%s A=%s B=%s C=%s D=%s: exact edges, outage, contradiction, independent claim, restart, cold and separate import", sell, role, a.id, b.id, c.id, d.id)
}

func ancestryFundTrade(h *harness, local, peer string, sell chain.ID, role string, ownOnly bool) ancestryTrade {
	h.t.Helper()
	trade := ancestryTrade{maker: local, taker: peer}
	if role == "taker" {
		trade.maker, trade.taker = peer, local
	}
	h.online(trade.maker)
	h.online(trade.taker)
	tickUntilConnected(h.t, h.engines[trade.maker])
	tickUntilConnected(h.t, h.engines[trade.taker])
	request := TradeQuoteRequest{Kind: "maker", ExpectedWallet: trade.maker, ExpectedNetwork: "regtest", Sell: sell, SellAmount: ancestryPrincipal, BuyAmount: ancestryPrincipal, FeeSelection: FeeSelection{FundingFee: ancestryFundingFee, OwnerFeeCap: 20000}, FillOrderFields: automationFillAuthorization(sell, ancestryPrincipal, ancestryPrincipal, ancestryFundingFee, 0)}
	quote := h.command(trade.maker, "trade.quote", request).(TradeQuote)
	if !quote.Ready || quote.Error != "" || quote.PaidPrincipal != ancestryPrincipal {
		h.t.Fatal("actual whole parent review unavailable", quote.Error)
	}
	trade.makerConfirm = confirmation(quote)
	result := h.command(trade.maker, "trade.confirm", trade.makerConfirm).(ConfirmTradeResult)
	if result.State != "accepted" || result.Error != "" {
		h.t.Fatal("parent confirmation refused", result)
	}
	trade.parent = result.ID
	h.tick(trade.maker)
	key := h.engines[trade.maker].identity.Public().Hex() + ":" + trade.parent
	partialWait(h, "exact parent delivered", func() bool { return h.engines[trade.taker].s.Book[key].ID != (nostr.ID{}) }, func() { h.tick(trade.taker) })
	offer, err := protocol.DecodeOffer(h.engines[trade.taker].s.Book[key], time.Now().Unix())
	if err != nil {
		h.t.Fatal(err)
	}
	trade.id, trade.takerConfirm, _ = partialReviewedTake(h, trade.taker, offer, ancestryPrincipal, 0)
	ancestryPublishBeforeOffline(h, trade.taker, trade.id, "request")
	h.offline(trade.taker)
	partialWaitMailbox(h, "maker durably accepts exact child", func() bool { return h.engines[trade.maker].s.Swaps[trade.id] != nil }, func() { h.tick(trade.maker) })
	acceptance := ancestryPublishBeforeOffline(h, trade.maker, trade.id, "accepted")
	h.offline(trade.maker)
	h.online(trade.taker)
	partialWaitForPublications(h, trade.taker, acceptance, func() { h.tick(trade.taker) })
	partialWait(h, "actual long funding broadcast", func() bool { return h.swap(trade.taker, trade.id).LongSent }, func() { h.tick(trade.taker) })
	trade.longPublications = ancestryPublishBeforeOffline(h, trade.taker, trade.id, "long-funded")
	h.mine(sell.Other(), 2)
	h.offline(trade.taker)
	if role == "maker" || !ownOnly {
		h.online(trade.maker)
		partialWaitForPublications(h, trade.maker, trade.longPublications, func() { h.tick(trade.maker) })
		partialWait(h, "actual short funding broadcast", func() bool { return h.swap(trade.maker, trade.id).ShortSent }, func() { h.tick(trade.maker) })
		trade.shortPublications = ancestryPublishBeforeOffline(h, trade.maker, trade.id, "short-funded")
		h.mine(sell, 2)
		h.offline(trade.maker)
	}
	h.online(local)
	h.tick(local)
	return trade
}

// Keep the recipient offline until the exact saved event has a relay receipt.
func ancestryPublishBeforeOffline(h *harness, sender, id, kind string) map[string]partialPublication {
	h.t.Helper()
	expected, err := partialPublications(h.engines[sender], []string{id}, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	partialWait(h, "exact ancestry "+kind+" relay publication", func() bool {
		ready, err := partialPublicationsPublished(h.engines[sender], expected)
		if err != nil {
			h.t.Fatal(err)
		}
		return ready
	}, func() { h.tick(sender) })
	return expected
}

func ancestryComplete(h *harness, trade ancestryTrade) {
	h.t.Helper()
	h.online(trade.maker)
	h.online(trade.taker)
	partialWaitForPublications(h, trade.maker, trade.longPublications, func() { h.tick(trade.maker, trade.taker) })
	if len(trade.shortPublications) > 0 {
		partialWaitForPublications(h, trade.taker, trade.shortPublications, func() { h.tick(trade.maker, trade.taker) })
	}
	partialWait(h, "actual ancestry child completion", func() bool {
		return h.swap(trade.maker, trade.id).Stage == "completed" && h.swap(trade.taker, trade.id).Stage == "completed"
	}, func() { h.tick(trade.maker, trade.taker); h.minePending() })
}

func ancestryCore(h *harness, name, id string) *Swap {
	h.t.Helper()
	e := h.engines[name]
	s, err := fillValue[Swap](&e.s, engineFillReader{e}, "swaps", id)
	if err != nil || s == nil {
		h.t.Fatal("exact hot/cold ancestry core", id, err)
	}
	return s
}

func ancestryAssertEdge(h *harness, name, id, parentID string) {
	h.t.Helper()
	s := ancestryCore(h, name, id)
	own, raw, sent := localFunding(s)
	tx := partialConfirmedTransaction(h, own.Chain, own.TxID)
	if !sent || contract.Hex(tx) != raw || len(tx.TxIn) != 1 {
		h.t.Fatal("actual single selected funding input changed")
	}
	var total int64
	prev := partialConfirmedTransaction(h, own.Chain, tx.TxIn[0].PreviousOutPoint.Hash.String())
	point := tx.TxIn[0].PreviousOutPoint
	if uint64(point.Index) >= uint64(len(prev.TxOut)) {
		h.t.Fatal("invalid actual funding previous output")
	}
	total = prev.TxOut[point.Index].Value
	for _, out := range tx.TxOut {
		total -= out.Value
	}
	if total != ancestryFundingFee {
		h.t.Fatal("actual funding fee changed", total)
	}
	if parentID == "" {
		if len(s.FundingParents) != 0 {
			h.t.Fatal("independent funding acquired an ancestor")
		}
	} else {
		parent := ancestryCore(h, name, parentID)
		parentOwn, _, _ := localFunding(parent)
		if len(s.FundingParents) != 1 || s.FundingParents[0] != (FundingParent{SwapID: parentID, Chain: own.Chain, TxID: parentOwn.TxID, Vout: point.Index}) || point.Hash.String() != parentOwn.TxID || point.Index == parentOwn.Vout || point.Index != 1 || len(prev.TxOut) != 2 {
			h.t.Fatal("selected actual change input does not match exact direct ancestry")
		}
		retained, err := localFundingTransaction(parent)
		owned := false
		for _, entry := range h.engines[name].receiveBook[own.Chain] {
			owned = owned || bytes.Equal(prev.TxOut[point.Index].PkScript, entry.script)
		}
		if err != nil || retained == nil || contract.Hex(retained) != contract.Hex(prev) || !owned {
			h.t.Fatal("funding ancestor output is not owned change")
		}
	}
}

func ancestryWaitProof(h *harness, name string, ids ...string) {
	h.t.Helper()
	partialWait(h, "fresh current descendant funding proof", func() bool {
		for _, id := range ids {
			if !h.engines[name].fundingAncestryReady(h.swap(name, id)) {
				return false
			}
		}
		return true
	}, func() { h.tick(name) })
}

// Exclude explanatory hold/confirmation/stage fields. Include immutable funding,
// contracts, refund variants, exact edges and every permanent authorization.
func ancestryStableState(h *harness, name string, trades []ancestryTrade) map[string]string {
	h.t.Helper()
	result := map[string]string{}
	for _, trade := range trades {
		s := ancestryCore(h, name, trade.id)
		_, raw, _ := localFunding(s)
		result[trade.id] = protocol.Digest([]any{s.Request, s.Terms, raw, s.SelfRefunds, s.FundingParents})
		if s.Role == "maker" {
			p, err := fillValue[ParentOrder](&h.engines[name].s, engineFillReader{h.engines[name]}, "parent_orders", trade.parent)
			if err != nil || p == nil {
				h.t.Fatal("exact parent authority", err)
			}
			result[trade.parent] = protocol.Digest([]any{p.Offer.Sell, p.Offer.SellAmount, p.Economics, p.Fees, p.Bounties, p.Quantities.Available})
		}
	}
	return result
}

func ancestryAssertStable(h *harness, name string, trades []ancestryTrade, expected map[string]string) {
	h.t.Helper()
	if protocol.Digest(ancestryStableState(h, name, trades)) != protocol.Digest(expected) {
		h.t.Fatal("ancestry changed immutable funding, contracts, quantity or permanent authorization")
	}
	for _, trade := range trades {
		s := ancestryCore(h, name, trade.id)
		own, _, _ := localFunding(s)
		key, err := fillValue[string](&h.engines[name].s, engineFillReader{h.engines[name]}, "fill_keys", fundingIdentityKey(own.Chain, own.TxID))
		if err != nil || key == nil || *key != s.ID {
			h.t.Fatal("exact hot/cold funding identity lost", err)
		}
	}
}

func ancestryAssertHeld(h *harness, name string, b, c, d ancestryTrade, before map[string]string, contradicted bool) {
	h.t.Helper()
	for _, trade := range []ancestryTrade{b, c} {
		s := h.swap(name, trade.id)
		if !s.FundingAncestryHeld || h.engines[name].fundingAncestryReady(s) {
			h.t.Fatal("descendant retained execution proof")
		}
	}
	if h.swap(name, d.id).FundingAncestryHeld {
		h.t.Fatal("unrelated D inherited another child's hold")
	}
	if h.swap(name, c.id).SelfClaim != "" || h.swap(name, c.id).SecretExposed {
		h.t.Fatal("held C first revealed its secret")
	}
	if h.swap(name, b.id).Role == "maker" {
		f := h.engines[name].s.FillRecords[b.id]
		want := FillFilled
		if contradicted {
			want = FillCommitted
		}
		if f == nil || f.Allocation.Disposition != want || !f.Allocation.EverCommitted {
			h.t.Fatal("unknown versus contradicted evidence changed the wrong quantity bin")
		}
	}
	// D may gain an authorized claim, but no funding or fee authority changes.
	for _, trade := range []ancestryTrade{b, c, d} {
		got := ancestryStableState(h, name, []ancestryTrade{trade})
		for key, value := range got {
			if before[key] != value {
				h.t.Fatal("hold credited quantity/fees or rewrote signed funding", key)
			}
		}
	}
}

func ancestryAssertRevealWindow(h *harness, s *Swap) {
	h.t.Helper()
	if s.Terms == nil {
		h.t.Fatal("fixture lacks immutable reveal terms")
	}
	if err := s.Terms.Gate("reveal", map[chain.ID]uint32{chain.BTC: h.height(chain.BTC), chain.Blake: h.height(chain.Blake)}); err != nil {
		h.t.Fatal("fixture exhausted the original first-reveal deadline")
	}
}

// SelfClaim is the base authorization; fee selection may publish another exact
// retained variant. An attempt alone is not node evidence: the caller still
// requires the selected bytes from the actual node and verifies the full spend.
func ancestrySelectedClaim(s *Swap) (*wire.MsgTx, error) {
	if s.ClaimLastAttempt == 0 || s.ClaimVariant < 0 || s.ClaimVariant >= len(s.SelfClaims) {
		return nil, errors.New("no selected owner claim publication attempt")
	}
	return contract.Parse(s.SelfClaims[s.ClaimVariant])
}

func ancestryAssertBroadcastClaim(h *harness, owner string, s *Swap) {
	h.t.Helper()
	tx, err := ancestrySelectedClaim(s)
	if err != nil {
		h.t.Fatal("independent actual claim missing", err)
	}
	c := s.Short // first reveal is the taker's incoming short leg
	if s.Role == "maker" {
		c = s.Long // the maker rescues its incoming leg after witnessing the peer claim
	}
	actual, err := h.nodes[c.Chain].Transaction(h.ctx, tx.TxHash().String())
	if err != nil || actual.TxID != tx.TxHash().String() || actual.Hex != contract.Hex(tx) {
		h.t.Fatal("independent D claim was not actually published", err)
	}
	funding := partialConfirmedTransaction(h, c.Chain, c.TxID)
	if err := contract.VerifyObservedSpend(c, tx, []*wire.TxOut{funding.TxOut[c.Vout]}); err != nil {
		h.t.Fatal("independent D claim signature", err)
	}
	secret, ok := contract.ExtractSecret(c, tx)
	clear(secret)
	if !ok {
		h.t.Fatal("independent D progress did not use the claim branch")
	}
	feeOK := false
	if len(tx.TxOut) == 1 {
		for _, fee := range protocol.RescueFees {
			feeOK = feeOK || c.Amount-tx.TxOut[0].Value == fee
		}
	}
	payout, err := partialRetainedPayout(h.engines[owner], s, c, tx, false, 0)
	if err != nil || !feeOK || !bytes.Equal(tx.TxOut[0].PkScript, payout) {
		h.t.Fatal("independent D claim changed the reviewed owner payout or fee ladder")
	}
}

func ancestryRetry(h *harness, name string, trades []ancestryTrade) {
	h.t.Helper()
	for _, trade := range trades {
		request, want := trade.takerConfirm, trade.id
		if name == trade.maker {
			request, want = trade.makerConfirm, trade.parent
		}
		before, err := fillValue[TradeReceipt](&h.engines[name].s, engineFillReader{h.engines[name]}, "trade_receipts", request.RequestID)
		if err != nil || before == nil {
			h.t.Fatal("saved exact receipt absent", err)
		}
		digest := protocol.Digest(before)
		result := h.command(name, "trade.confirm", request).(ConfirmTradeResult)
		after, err := fillValue[TradeReceipt](&h.engines[name].s, engineFillReader{h.engines[name]}, "trade_receipts", request.RequestID)
		if err != nil || result.State != "accepted" || result.ID != want || after == nil || protocol.Digest(after) != digest {
			h.t.Fatal("exact saved confirmation changed receipt or created another action", err)
		}
	}
}

func ancestryRestoreSeparate(h *harness, name string) {
	h.t.Helper()
	state, err := h.engines[name].BackupSnapshot()
	if err != nil {
		h.t.Fatal(err)
	}
	path := filepath.Join(h.t.TempDir(), "ancestry-state.blakeswap")
	if err := storage.WritePortable(h.ctx, path, []byte(testPortablePassword), state); err != nil {
		h.t.Fatal(err)
	}
	var restored State
	if err := storage.ReadPortable(h.ctx, path, []byte(testPortablePassword), &restored); err != nil {
		h.t.Fatal(err)
	}
	if err := PrepareRecovery(&restored, time.Now().Unix(), false); err != nil {
		h.t.Fatal(err)
	}
	h.offline(name)
	cfg := h.configs[name]
	cfg.DataDir = h.t.TempDir()
	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		h.t.Fatal(err)
	}
	defer clear(password)
	vault, err := storage.Open(filepath.Join(cfg.DataDir, "state.db"), password)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := vault.Save(&restored); err != nil {
		_ = vault.Close()
		h.t.Fatal(err)
	}
	if err := vault.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.configs[name] = cfg
	h.online(name)
	if h.engines[name].Status().Recovery.State != "recovering" {
		h.t.Fatal("separate restored installation bypassed recovery hold")
	}
}
