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
	e, s, _, _ := isolatedFixture(t, "maker")
	peer := nostr.Generate()
	s.Request.Taker = peer.Public().Hex()
	s.Terms.Request.Taker = s.Request.Taker
	makeEvent := func(id, swapID, raw string, terms *protocol.Terms) nostr.Event {
		body, _ := json.Marshal(fundingMessage{TermsHash: protocol.Digest(terms), Raw: raw})
		event, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), transport.Message{Version: 1, ID: id, Type: "long-funded", SwapID: swapID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	original := makeEvent(transport.RandomID(), s.ID, s.LongFunding, s.Terms)
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
	second := *s
	second.ID = transport.RandomID()
	second.Long.TxID = ""
	second.LongFunding = ""
	secondTerms := *s.Terms
	second.Terms = &secondTerms
	e.s.Swaps[second.ID] = &second
	tx.TxIn[0].Sequence--
	if err := e.receive(makeEvent(transport.RandomID(), second.ID, contract.Hex(tx), second.Terms)); err != nil {
		t.Fatal("new established funding evidence was blocked", err)
	}
	if e.s.Swaps[second.ID].Long.TxID != tx.TxHash().String() {
		t.Fatal("new funding identity not retained")
	}
}

func TestMailboxIgnoredLateAcceptancesCannotCreateUnlimitedDurableEvidence(t *testing.T) {
	e, s, _, _ := isolatedFixture(t, "taker")
	peer := nostr.Generate()
	offer := s.Terms.Offer()
	offer.Maker = peer.Public().Hex()
	raw, err := offer.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := s.Request.OfferEvent
	event.Content = string(raw)
	if err := transport.Sign(&event, peer); err != nil {
		t.Fatal(err)
	}
	s.Request.OfferEvent = event
	makerKeys := s.Terms.MakerKeys
	s.Terms = nil
	s.Stage = "expired before funding"
	s.LongFunding = ""
	s.ShortFunding = ""
	s.LongSent = false
	s.ShortSent = false
	makeEvent := func(height uint32) nostr.Event {
		terms, err := protocol.NewTerms(s.Request, makerKeys, map[chain.ID]uint32{chain.BTC: height, chain.Blake: height})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(terms)
		wrapped, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), transport.Message{Version: 1, ID: transport.RandomID(), Type: "accepted", SwapID: s.ID, Body: body})
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
