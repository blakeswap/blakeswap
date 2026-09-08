package daemon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
)

func fillParentFixture(t *testing.T, sell chain.ID, bps int64) (ParentOrder, nostr.SecretKey) {
	t.Helper()
	maker := nostr.Generate()
	o := protocol.Offer{Version: protocol.Version, Network: chain.Regtest, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: sell, SellAmount: 1000000, BuyAmount: 1300001, Expires: time.Now().Unix() + 600, Status: "open", Revision: 1, Available: 1000000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillPartial, Min: 200000, Max: 800000}, TowerBPS: bps}
	p, err := newParentOrder(o, FeeSelection{FundingFee: 6500, OwnerFeeCap: 20000}, FillOrderFields{FillPolicy: o.FillPolicy, FeeBudgets: map[chain.ID]int64{chain.BTC: 500000, chain.Blake: 500000}, BountyBudgets: map[chain.ID]int64{chain.BTC: 500000, chain.Blake: 500000}}, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	return *p, maker
}

func fillRequestFixture(t *testing.T, p ParentOrder, maker nostr.SecretKey, quantity int64) protocol.Request {
	t.Helper()
	o := p.Offer
	o.Revision, o.Available = p.Quantities.Revision, p.Quantities.Available
	raw, err := o.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", o.ID}, {"t", o.Network.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	keys := map[chain.ID]string{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = hex.EncodeToString(key.PubKey().SerializeCompressed())
	}
	return protocol.Request{Version: protocol.Version, ID: transport.RandomID(), Revision: o.Revision, Quantity: quantity, OfferEvent: event, Taker: nostr.Generate().Public().Hex(), Hash: transport.RandomID(), Keys: keys}
}

func TestParentFillResourcesRetainExactBudgetsAcrossSettlementAndReorg(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			p, maker := fillParentFixture(t, sell, 50)
			if reserve, err := p.fundingReserve(p.Quantities.Available); err != nil || reserve != 32500 {
				t.Fatalf("aggregate funding reserve: %d %v", reserve, err)
			}
			first := fillRequestFixture(t, p, maker, 400000)
			next, child, err := p.reserveFill(first)
			if err != nil {
				t.Fatal(err)
			}
			if p.Quantities.Available != 1000000 || p.Fees[sell].Reserved != 0 {
				t.Fatal("planning mutated original parent")
			}
			p = next
			second := fillRequestFixture(t, p, maker, 600000)
			p, sibling, err := p.reserveFill(second)
			if err != nil {
				t.Fatal(err)
			}
			if child.BuyAmount != 520001 || sibling.BuyAmount != 780001 || child.ParentRevision == sibling.ParentRevision {
				t.Fatal("child rounded amount/revision changed")
			}
			if p.Quantities.Available != 0 || p.Quantities.Reserved != 1000000 || p.Fees[sell].Reserved != 53000 || p.Fees[sell.Other()].Reserved != 40000 {
				t.Fatal("child fee/quantity reservation mismatch")
			}
			for _, pair := range []struct {
				child  **FillRecord
				target FillDisposition
			}{{&child, FillCommitted}, {&sibling, FillCommitted}, {&child, FillFilled}, {&sibling, FillReleased}} {
				updated, value, err := p.transitionFill(**pair.child, pair.target, false)
				if err != nil {
					t.Fatal(err)
				}
				p = updated
				*pair.child = &value
			}
			spent := p.Fees[sell].Consumed
			beforeSibling, _ := json.Marshal(sibling)
			p, value, err := p.transitionFill(*child, FillCommitted, false)
			if err != nil {
				t.Fatal(err)
			}
			child = &value
			if p.Fees[sell].Consumed != spent || p.Fees[sell].Reserved != 0 || p.Quantities.Committed != 400000 || p.Quantities.Released != 600000 {
				t.Fatal("reorg credited authorization or changed unrelated child")
			}
			afterSibling, _ := json.Marshal(sibling)
			if !bytes.Equal(beforeSibling, afterSibling) {
				t.Fatal("unrelated sibling was changed")
			}
			// Physical encrypted checkpoint/reopen, using explicit synthetic input
			// identities. Actual disjoint UTXO admission is a separate controller test.
			child.Inputs = []CoinOutpoint{{TxID: transport.RandomID()}}
			sibling.Inputs = []CoinOutpoint{{TxID: transport.RandomID()}}
			path := filepath.Join(t.TempDir(), "state.db")
			password := []byte("disposable-parent-ledger-state-credential")
			v, err := storage.Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			state := State{Version: StateVersion, Network: chain.Regtest, ParentOrders: map[string]*ParentOrder{p.Offer.ID: &p}, FillRecords: map[string]*FillRecord{child.ID: child, sibling.ID: sibling}}
			if err := v.Save(state); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			v, retained, err := openCurrentStateVault(path, password)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if retained.ParentOrders[p.Offer.ID].Fees[sell].Consumed != spent || !retained.FillRecords[child.ID].Allocation.EverCommitted {
				t.Fatal("reopen forgot permanent authority consumption")
			}
		})
	}
}

func TestParentFillReturnRequiresLocalRefusalAndNeverCreditsCommittedAuthority(t *testing.T) {
	p, maker := fillParentFixture(t, chain.BTC, 0)
	p, child, err := p.reserveFill(fillRequestFixture(t, p, maker, 400000))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(p)
	if _, _, err := p.transitionFill(*child, FillRetired, false); err == nil {
		t.Fatal("absence-only return accepted")
	}
	uncertain := *child
	uncertain.ImportedUncertain = true
	if _, _, err := p.transitionFill(uncertain, FillRetired, true); err == nil {
		t.Fatal("imported uncertainty asserted never funded")
	}
	after, _ := json.Marshal(p)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected return mutated budgets")
	}
	p, retired, err := p.transitionFill(*child, FillRetired, true)
	if err != nil {
		t.Fatal(err)
	}
	if !retired.FundingDisabled || retired.Allocation.currentQuantity() != 0 || p.Quantities.Available != p.Quantities.Total || p.Fees[chain.BTC].Reserved != 0 {
		t.Fatal("safe return retained a duplicate allocation")
	}
	if _, _, err := p.transitionFill(retired, FillCommitted, false); err == nil {
		t.Fatal("retired child regained funding allocation")
	}
	p, second, err := p.reserveFill(fillRequestFixture(t, p, maker, 400000))
	if err != nil {
		t.Fatal(err)
	}
	p, committed, err := p.transitionFill(*second, FillCommitted, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.transitionFill(committed, FillRetired, true); err == nil {
		t.Fatal("committed child returned to available")
	}
}

func TestParentFillFailedSecondAssetReservationLeavesOriginalUntouched(t *testing.T) {
	p, maker := fillParentFixture(t, chain.BTC, 50)
	p.Bounties[chain.Blake] = FillBudget{Limit: 0}
	before, _ := json.Marshal(p)
	if _, _, err := p.reserveFill(fillRequestFixture(t, p, maker, 400000)); err == nil {
		t.Fatal("zero bounty budget accepted protected child")
	}
	after, _ := json.Marshal(p)
	if !bytes.Equal(before, after) {
		t.Fatal("failed second-asset reservation consumed first asset")
	}
	if err := validateFillLimits(map[chain.ID]int64{chain.BTC: 100000}); err == nil {
		t.Fatal("missing asset implied unlimited authorization")
	}
	p.RestoreHold = true
	if _, _, err := p.reserveFill(fillRequestFixture(t, p, maker, 400000)); err == nil {
		t.Fatal("restore hold admitted new child")
	}
}

func TestParentFillInputTransferUsesDisjointConfirmedOutpoints(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			parentID := transport.RandomID()
			first := chain.UTXO{TxID: transport.RandomID(), Amount: 406500, Confirmations: 6, Script: "51"}
			second := chain.UTXO{TxID: transport.RandomID(), Amount: 606500, Confirmations: 6, Script: "51"}
			unassigned := chain.UTXO{TxID: transport.RandomID(), Amount: 2000000, Confirmations: 6, Script: "51"}
			pool := CoinReservation{Chain: id, Inputs: []CoinOutpoint{{TxID: first.TxID}, {TxID: second.TxID}}}
			e := &Engine{Config: Config{Network: chain.Regtest}, s: State{CoinReservations: map[string]CoinReservation{"offer/" + parentID: pool}}, receiveBook: map[chain.ID][]receiveAddress{id: {{script: []byte{0x51}}}}, walletCoins: map[chain.ID]map[string][]chain.UTXO{id: {"51": {first, second, unassigned}}}}
			before, _ := json.Marshal(e.s)
			child, remaining, err := e.fillReservationCandidate(parentID, id, 406500)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(e.s)
			if !bytes.Equal(before, after) {
				t.Fatal("candidate changed ownership before atomic commit")
			}
			if len(child.Inputs) != 1 || child.Inputs[0].TxID != first.TxID || len(remaining.Inputs) != 1 || remaining.Inputs[0].TxID != second.TxID {
				t.Fatal("first child did not transfer a whole disjoint input")
			}
			e.s.CoinReservations["swap/first"] = child
			e.s.CoinReservations["offer/"+parentID] = remaining
			sibling, remaining, err := e.fillReservationCandidate(parentID, id, 606500)
			if err != nil {
				t.Fatal(err)
			}
			if len(sibling.Inputs) != 1 || sibling.Inputs[0].TxID == child.Inputs[0].TxID || len(remaining.Inputs) != 0 {
				t.Fatal("sibling input ownership overlaps")
			}
			e.s.CoinReservations["swap/second"] = sibling
			e.s.CoinReservations["offer/"+parentID] = remaining
			if _, _, err := e.fillReservationCandidate(parentID, id, 106500); err == nil {
				t.Fatal("empty parent pool borrowed an unrelated free coin")
			}
			change := chain.UTXO{TxID: transport.RandomID(), Vout: 1, Amount: 306500, Confirmations: 0, Script: "51"}
			e.walletCoins[id]["51"] = append(e.walletCoins[id]["51"], change)
			e.s.CoinReservations["offer/"+parentID] = CoinReservation{Chain: id, Inputs: []CoinOutpoint{{TxID: change.TxID, Vout: 1}}}
			if _, _, err := e.fillReservationCandidate(parentID, id, 206500); err == nil {
				t.Fatal("unconfirmed funding change counted as independent funds")
			}
			e.walletCoins[id]["51"][3].Confirmations = 6
			if third, _, err := e.fillReservationCandidate(parentID, id, 206500); err != nil || len(third.Inputs) != 1 || third.Inputs[0].TxID != change.TxID {
				t.Fatalf("positively confirmed assigned change unavailable: %v", err)
			}
		})
	}
}
