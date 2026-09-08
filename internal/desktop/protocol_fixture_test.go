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
		}
		for _, key := range keys {
			if prior := state.FillKeys[key]; prior != "" && prior != id {
				t.Fatal("fixture reuses another child's identity")
			}
			state.FillKeys[key] = id
		}
	}
}
