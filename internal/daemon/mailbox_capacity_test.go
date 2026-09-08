package daemon

import (
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/wire"
)

func TestMailboxFundingWitnessAliasesCannotBypassRecoveryBudget(t *testing.T) {
	e, maker, now := fillAdmissionEngine(t, chain.BTC)
	e.s.Seen = map[string]string{}
	peer := nostr.Generate()
	first := admissionRequest(t, e, maker, 400000)
	first.Taker = peer.Public().Hex()
	if err := applyFillRequest(t, e, first, now); err != nil {
		t.Fatal(err)
	}
	s := e.s.Swaps[first.ID]
	parent := e.s.ParentOrders[e.s.FillRecords[first.ID].ParentID]
	next, published, err := e.prepareParentPublication(*parent, now+1)
	if err != nil {
		t.Fatal(err)
	}
	*parent = next
	if published != nil {
		e.stageOffer(parentPublicOffer(next), *published)
	}
	request := admissionRequest(t, e, maker, 600000)
	request.Taker = peer.Public().Hex()
	if err := applyFillRequest(t, e, request, now+1); err != nil {
		t.Fatal(err)
	}
	second := e.s.Swaps[request.ID]
	if second == nil || second.ID == s.ID || second.Request.Hash == s.Request.Hash || second.Request.Keys[chain.BTC] == s.Request.Keys[chain.BTC] || second.Terms.MakerKeys[chain.BTC] == s.Terms.MakerKeys[chain.BTC] {
		t.Fatal("fixture did not accept independent children")
	}
	funding := func(swap *Swap) *wire.MsgTx {
		pk, err := swap.Long.PkScript()
		if err != nil {
			t.Fatal(err)
		}
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(swap.Long.Amount, pk))
		return tx
	}
	makeEvent := func(id, swapID, raw string, terms *protocol.Terms) nostr.Event {
		body, _ := json.Marshal(fundingMessage{TermsHash: protocol.Digest(terms), Raw: raw})
		event, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), transport.Message{Version: transport.MessageVersion, ID: id, Type: "long-funded", SwapID: swapID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	original := makeEvent(transport.RandomID(), s.ID, contract.Hex(funding(s)), s.Terms)
	if err := e.receive(original); err != nil {
		t.Fatal(err)
	}
	tx, err := contract.Parse(s.LongFunding)
	if err != nil {
		t.Fatal(err)
	}
	// The funding handler binds the agreed output/non-witness txid. A peer can
	// vary these witness bytes without creating another contract or secret fact.
	tx.TxIn[0].Witness = wire.TxWitness{[]byte("another witness encoding")}
	if tx.TxHash().String() != s.Long.TxID {
		t.Fatal("fixture changed economic identity")
	}
	if _, err := bindFunding(s.Terms.Long, contract.Hex(tx)); err != nil {
		t.Fatal(err)
	}
	e.stateBytes = WalletRecoveryBudget
	seen, outbox := len(e.s.Seen), len(e.s.Outbox)
	if err := e.receive(makeEvent(transport.RandomID(), s.ID, contract.Hex(tx), s.Terms)); err == nil {
		t.Fatal("same funding outpoint witness alias bypassed the admission budget")
	}
	if len(e.s.Seen) != seen || len(e.s.Outbox) != outbox {
		t.Fatal("rejected alias consumed durable records")
	}
	if err := e.receive(original); err != nil {
		t.Fatal("exact authenticated retry was blocked", err)
	}
	// A first funding fact for a different already-established obligation remains
	// admissible at the ceiling. It is not an alias of the earlier contract.
	tx = funding(second)
	if err := e.receive(makeEvent(transport.RandomID(), second.ID, contract.Hex(tx), second.Terms)); err != nil {
		t.Fatal("new established funding evidence was blocked", err)
	}
	if e.s.Swaps[second.ID].Long.TxID != tx.TxHash().String() {
		t.Fatal("new funding identity not retained")
	}
}

func TestMailboxIgnoredLateAcceptancesCannotCreateUnlimitedDurableEvidence(t *testing.T) {
	e, _, _ := sendFixture(t)
	parent, peer := fillParentFixture(t, chain.BTC, 0)
	r := fillRequestFixture(t, parent, peer, 400000)
	r.Taker = e.identity.Public().Hex()
	var err error
	r.Keys, err = e.swapKeys(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	makerKeys, err := e.swapKeys(protocol.Digest("mailbox peer/" + r.ID))
	if err != nil {
		t.Fatal(err)
	}
	// This was an unfunded terminal request from the outset. No previously
	// signed funding, accepted terms or refund bundle is erased by the fixture.
	s := &Swap{ID: r.ID, Role: "taker", Request: r, Stage: "expired before acceptance"}
	e.s.Swaps[s.ID] = s
	if err := e.retainSwapIdentity(s); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	makeEvent := func(height uint32) nostr.Event {
		terms, err := protocol.NewTerms(s.Request, makerKeys, map[chain.ID]uint32{chain.BTC: height, chain.Blake: height})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(terms)
		wrapped, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "accepted", SwapID: s.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return wrapped
	}
	original := makeEvent(100)
	if err := e.receive(original); err != nil {
		t.Fatal(err)
	}
	if s.Terms != nil {
		t.Fatal("late acceptance revived expired authority")
	}
	e.stateBytes = WalletRecoveryBudget
	before := len(e.s.Seen)
	if err := e.receive(makeEvent(101)); err == nil {
		t.Fatal("ignored late acceptance bypassed alias capacity")
	}
	if len(e.s.Seen) != before || s.Terms != nil {
		t.Fatal("ignored message changed durable authority/evidence")
	}
	if err := e.receive(original); err != nil {
		t.Fatal("exact terminal retry blocked", err)
	}
}
