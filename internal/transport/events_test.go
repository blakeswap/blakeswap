package transport

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"strings"
	"testing"
)

func TestGiftWrapAuthenticationAndPrivacy(t *testing.T) {
	alice, bob, eve := nostr.Generate(), nostr.Generate(), nostr.Generate()
	m := Message{Version: MessageVersion, ID: RandomID(), Type: "test", SwapID: RandomID(), Body: json.RawMessage(`{"sensitive":"test-only secret"}`)}
	event, e := Wrap(alice, bob.Public(), m)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(event.String(), "test-only secret") || event.PubKey == alice.Public() {
		t.Fatal("envelope reveals private contents/sender")
	}
	from, got, e := Unwrap(bob, event)
	if e != nil || from != alice.Public() || got.ID != m.ID {
		t.Fatal("roundtrip failed", e)
	}
	if _, _, e = Unwrap(eve, event); e == nil {
		t.Fatal("wrong recipient decrypts")
	}
	altered := event
	altered.Content += "A"
	if _, _, e = Unwrap(bob, altered); e == nil {
		t.Fatal("tampering accepted")
	}
	altered = event
	altered.ID[0] ^= 1
	if Valid(altered) == nil {
		t.Fatal("false ID accepted")
	}
	altered = event
	altered.Kind = 20059
	_ = Sign(&altered, nostr.Generate())
	if _, _, e = Unwrap(bob, altered); e == nil {
		t.Fatal("ephemeral mailbox kind accepted")
	}
	again, e := Wrap(alice, bob.Public(), m)
	if e != nil {
		t.Fatal(e)
	}
	if again.ID == event.ID || again.PubKey == event.PubKey {
		t.Fatal("one-time outer key reused")
	}
	_, received, e := Unwrap(bob, again)
	if e != nil || received.ID != got.ID {
		t.Fatal("application message identity unstable")
	}
}

func TestGiftWrapRejectsUnsupportedEnvelopeAndNamespace(t *testing.T) {
	sender, recipient := nostr.Generate(), nostr.Generate()
	for _, version := range []int{0, 2, 3} {
		message := Message{Version: version, ID: RandomID(), Type: "request", SwapID: RandomID(), Body: json.RawMessage(`{}`)}
		event, err := Wrap(sender, recipient.Public(), message)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Unwrap(recipient, event); err == nil {
			t.Fatal("incompatible private envelope accepted")
		}
	}
	message := Message{Version: MessageVersion, ID: RandomID(), Type: "request", SwapID: RandomID(), Body: json.RawMessage(`{}`)}
	event, err := WrapFor("blakeswap-regtest-unsupported", sender, recipient.Public(), message)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Unwrap(recipient, event); err == nil {
		t.Fatal("unsupported namespace accepted")
	}
}
func TestRejectMismatchedRumorAuthor(t *testing.T) {
	alice, bob, eve := nostr.Generate(), nostr.Generate(), nostr.Generate()
	message := Message{Version: MessageVersion, ID: RandomID(), Type: "test", Body: json.RawMessage(`{}`)}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	wrapRumor := func(author nostr.PubKey) nostr.Event {
		t.Helper()
		rumor := nostr.Event{PubKey: author, Kind: RumorKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"p", bob.Public().Hex()}, {"t", Namespace}}, Content: string(raw)}
		rumor.ID = EventID(rumor)
		content, err := encrypt(alice, bob.Public(), rumor.String())
		if err != nil {
			t.Fatal(err)
		}
		seal := nostr.Event{Kind: 13, CreatedAt: nostr.Now(), Content: content, Tags: nostr.Tags{}}
		if err = Sign(&seal, alice); err != nil {
			t.Fatal(err)
		}
		ephemeral := nostr.Generate()
		content, err = encrypt(ephemeral, bob.Public(), seal.String())
		if err != nil {
			t.Fatal(err)
		}
		outer := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Content: content, Tags: nostr.Tags{{"p", bob.Public().Hex()}}}
		if err = Sign(&outer, ephemeral); err != nil {
			t.Fatal(err)
		}
		return outer
	}
	if from, got, err := Unwrap(bob, wrapRumor(alice.Public())); err != nil || from != alice.Public() || got.ID != message.ID {
		t.Fatal("valid current-format control failed", err)
	}
	if _, _, err := Unwrap(bob, wrapRumor(eve.Public())); err == nil {
		t.Fatal("seal author silently replaced forged rumor author")
	}
}
func FuzzUnwrap(f *testing.F) {
	key := nostr.Generate()
	f.Add(`{"kind":1059,"content":"bad"}`)
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > MaxEventSize {
			return
		}
		var event nostr.Event
		if json.Unmarshal([]byte(raw), &event) != nil {
			return
		}
		_, _, _ = Unwrap(key, event)
	})
}

func TestMailboxesRejectForeignNetworkBindings(t *testing.T) {
	sender, recipient := nostr.Generate(), nostr.Generate()
	message := Message{Version: MessageVersion, ID: RandomID(), Type: "request", Body: json.RawMessage(`{}`)}
	event, err := WrapFor("blakeswap-mainnet-v1", sender, recipient.Public(), message)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = UnwrapFor("blakeswap-testnet-v1", recipient, event); err == nil {
		t.Fatal("foreign network message accepted")
	}
	if _, _, err = UnwrapFor("blakeswap-mainnet-v1", recipient, event); err != nil {
		t.Fatal(err)
	}
}
