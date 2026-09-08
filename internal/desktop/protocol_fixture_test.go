package desktop

import (
	"encoding/hex"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
)

// Stable synthetic identities let preservation assertions address the same
// current-format child through active, cold and imported placement.
const fixtureChildID = "1111111111111111111111111111111111111111111111111111111111111111"
const fixtureParentID = "2222222222222222222222222222222222222222222222222222222222222222"
const fixtureSuccessorID = "4444444444444444444444444444444444444444444444444444444444444444"

func fixtureOffer(t *testing.T, network chain.Network, id string, maker nostr.SecretKey) (protocol.Offer, nostr.Event) {
	t.Helper()
	o := protocol.Offer{Version: protocol.Version, ID: id, Network: network, Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 1000000, BuyAmount: 2000000, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}, Revision: 1, Available: 1000000, Status: "open", Expires: time.Now().Unix() + 3600}
	raw, err := o.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", id}, {"t", network.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	return o, event
}

func fixtureSwap(t *testing.T, network chain.Network, seed, role string) *daemon.Swap {
	t.Helper()
	identity := fixtureIdentity(t, network, seed)
	owner, peer := identity, nostr.Generate()
	maker, taker := owner, peer
	if role == "taker" {
		maker, taker = peer, owner
	}
	offer, event := fixtureOffer(t, network, fixtureParentID, maker)
	childKeys := map[chain.ID]string{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		childKeys[id] = hex.EncodeToString(key.PubKey().SerializeCompressed())
	}
	request := protocol.Request{Version: protocol.Version, ID: fixtureChildID, OfferEvent: event, Quantity: offer.SellAmount, Revision: offer.Revision, Taker: taker.Public().Hex(), Hash: transport.RandomID(), Keys: childKeys}
	if _, err := request.Validate(time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return &daemon.Swap{ID: fixtureChildID, Role: role, Request: request}
}

// fixtureMakerCustody explicitly constructs accepted maker state for backup
// preservation tests. A retained own refund is permanent committed authority;
// it is never represented as settled or returned merely to simplify a fixture.
func fixtureMakerCustody(t *testing.T, state *daemon.State, child *daemon.Swap, committed bool) {
	t.Helper()
	if child.Role != "maker" || child.ID != child.Request.ID {
		t.Fatal("fixture requires exact maker child identity")
	}
	makerKeys := map[chain.ID]string{}
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		makerKeys[asset] = hex.EncodeToString(key.PubKey().SerializeCompressed())
	}
	height := state.Network.ForkHeight() + 200
	clock := uint32(time.Now().Unix())
	terms, err := protocol.NewTermsWithClocks(child.Request, makerKeys, map[chain.ID]uint32{chain.BTC: height, chain.Blake: height}, map[chain.ID]uint32{chain.BTC: clock, chain.Blake: clock})
	if err != nil {
		t.Fatal(err)
	}
	child.Terms, child.Long, child.Short = &terms, terms.Long, terms.Short
	child.Protection = &protocol.Tower{Version: protocol.Version}
	offer := terms.Offer()
	amounts, err := child.Request.Amounts()
	if err != nil || amounts.Sell != offer.SellAmount {
		t.Fatal("fixture requires one exact whole child", err)
	}
	policy := daemon.FeeSelection{FundingFee: 2000}
	fees := map[chain.ID]int64{offer.Sell: 22000, offer.Sell.Other(): 2000}
	parent := &daemon.ParentOrder{Offer: offer, Economics: offer.EconomicsDigest(), FundingPolicy: policy,
		Quantities: daemon.QuantityLedger{Total: amounts.Sell, Reserved: amounts.Sell, Revision: 2},
		Fees:       map[chain.ID]daemon.FillBudget{}, Bounties: map[chain.ID]daemon.FillBudget{}}
	allocation := daemon.FillAllocation{Quantity: amounts.Sell, Disposition: daemon.FillReserved}
	if committed {
		parent.Quantities.Reserved, parent.Quantities.Committed = 0, amounts.Sell
		allocation.Disposition, allocation.EverCommitted = daemon.FillCommitted, true
	}
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		charge := daemon.FillBudget{Limit: fees[asset], Reserved: fees[asset]}
		if committed {
			charge.Reserved, charge.Consumed = 0, fees[asset]
		}
		parent.Fees[asset] = charge
		parent.Bounties[asset] = daemon.FillBudget{}
	}
	points := []daemon.CoinOutpoint{{TxID: transport.RandomID()}}
	fill := &daemon.FillRecord{ID: child.ID, ParentID: offer.ID, ParentMaker: offer.Maker,
		ParentRevision: child.Request.Revision, RequestDigest: protocol.Digest(child.Request),
		Allocation: allocation, BuyAmount: amounts.Buy, FundingPolicy: policy, Fees: fees,
		Bounties: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}, Inputs: points}
	if state.ParentOrders == nil {
		state.ParentOrders = map[string]*daemon.ParentOrder{}
	}
	if state.FillRecords == nil {
		state.FillRecords = map[string]*daemon.FillRecord{}
	}
	if state.FundingFees == nil {
		state.FundingFees = map[string]daemon.FeeSelection{}
	}
	if state.CoinReservations == nil {
		state.CoinReservations = map[string]daemon.CoinReservation{}
	}
	if state.ParentOrders[offer.ID] != nil || state.FillRecords[child.ID] != nil {
		t.Fatal("fixture must not replace existing custody")
	}
	state.ParentOrders[offer.ID], state.FillRecords[child.ID] = parent, fill
	state.FundingFees["swap/"+child.ID] = policy
	state.CoinReservations["swap/"+child.ID] = daemon.CoinReservation{Chain: offer.Sell, Inputs: points}
}

func fixtureIdentity(t *testing.T, network chain.Network, seed string) nostr.SecretKey {
	t.Helper()
	keys, err := wallet.FromMnemonic(seed)
	if err != nil {
		t.Fatal(err)
	}
	keys.SetNetwork(network)
	identity, err := keys.Derive(2, "nostr-identity")
	if err != nil {
		t.Fatal(err)
	}
	return nostr.SecretKey(identity.Serialize())
}

// Current-format vaults retain the exact child hash/key index before any
// publication or restore. Synthetic fixtures must meet the same startup gate.
func fixtureIndexSwaps(t *testing.T, state *daemon.State) {
	t.Helper()
	state.FillKeys = map[string]string{}
	for id, child := range state.Swaps {
		if child == nil || child.ID != id || child.Request.ID != id {
			t.Fatal("fixture child identity mismatch")
		}
		keys := []string{"hash/" + child.Request.Hash}
		for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
			keys = append(keys, "key/"+child.Request.Keys[asset])
			if child.Terms != nil {
				keys = append(keys, "key/"+child.Terms.MakerKeys[asset])
			}
		}
		for _, key := range keys {
			if prior := state.FillKeys[key]; prior != "" && prior != id {
				t.Fatal("fixture reuses another child's identity")
			}
			state.FillKeys[key] = id
		}
	}
}
