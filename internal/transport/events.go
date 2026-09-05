// Package transport implements signed offers and NIP-44 / NIP-59 mailboxes.
package transport

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"fmt"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"time"
)

const OfferKind nostr.Kind = 38481 // Experimental, namespaced; not NIP-69 fiat orders.
const RumorKind nostr.Kind = 10481
const Namespace = "blakeswap-regtest-v1"
const MaxEventSize = 65536

type Message struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	SwapID  string          `json:"swap_id,omitempty"`
	Body    json.RawMessage `json:"body"`
}

func RandomID() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return fmt.Sprintf("%x", b)
}
func Tag(e nostr.Event, name string) string {
	for _, t := range e.Tags {
		if len(t) > 1 && t[0] == name {
			return t[1]
		}
	}
	return ""
}
func Valid(e nostr.Event) error {
	if len(e.Content) > MaxEventSize || len(e.Tags) > 64 || !validStrings(e) || EventID(e) != e.ID {
		return errors.New("invalid event ID or size")
	}
	pub, err := schnorr.ParsePubKey(e.PubKey[:])
	if err != nil {
		return err
	}
	sig, err := schnorr.ParseSignature(e.Sig[:])
	if err != nil {
		return err
	}
	if !sig.Verify(e.ID[:], pub) {
		return errors.New("invalid event signature")
	}
	return nil
}
func randomPast() nostr.Timestamp {
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return nostr.Timestamp(time.Now().Unix() - int64(binary.LittleEndian.Uint64(b[:])%(2*24*60*60)))
}
func encrypt(sk nostr.SecretKey, to nostr.PubKey, raw string) (string, error) {
	key, e := nip44.GenerateConversationKey(to, sk)
	if e != nil {
		return "", e
	}
	return nip44.Encrypt(raw, key)
}
func decrypt(sk nostr.SecretKey, from nostr.PubKey, raw string) (string, error) {
	key, e := nip44.GenerateConversationKey(from, sk)
	if e != nil {
		return "", e
	}
	return nip44.Decrypt(raw, key)
}
func Wrap(sk nostr.SecretKey, to nostr.PubKey, m Message) (nostr.Event, error) {
	raw, e := json.Marshal(m)
	if e != nil {
		return nostr.Event{}, e
	}
	rumor := nostr.Event{Kind: RumorKind, PubKey: sk.Public(), CreatedAt: nostr.Now(), Tags: nostr.Tags{{"p", to.Hex()}, {"t", Namespace}}, Content: string(raw)}
	rumor.ID = EventID(rumor)
	content, e := encrypt(sk, to, rumor.String())
	if e != nil {
		return nostr.Event{}, e
	}
	seal := nostr.Event{Kind: 13, CreatedAt: randomPast(), Tags: nostr.Tags{}, Content: content}
	if e = Sign(&seal, sk); e != nil {
		return nostr.Event{}, e
	}
	ephemeral := nostr.Generate()
	content, e = encrypt(ephemeral, to, seal.String())
	if e != nil {
		return nostr.Event{}, e
	}
	outer := nostr.Event{Kind: 1059, CreatedAt: randomPast(), Tags: nostr.Tags{{"p", to.Hex()}}, Content: content}
	if e = Sign(&outer, ephemeral); e != nil {
		return nostr.Event{}, e
	}
	if len(outer.String()) > MaxEventSize {
		return nostr.Event{}, errors.New("mailbox payload too large")
	}
	return outer, nil
}
func Unwrap(sk nostr.SecretKey, outer nostr.Event) (nostr.PubKey, Message, error) {
	fail := func(e error) (nostr.PubKey, Message, error) { return nostr.PubKey{}, Message{}, e }
	if e := Valid(outer); e != nil {
		return fail(e)
	}
	if outer.Kind != 1059 || Tag(outer, "p") != sk.Public().Hex() {
		return fail(errors.New("wrong envelope recipient/kind"))
	}
	raw, e := decrypt(sk, outer.PubKey, outer.Content)
	if e != nil {
		return fail(e)
	}
	var seal nostr.Event
	if e = json.Unmarshal([]byte(raw), &seal); e != nil {
		return fail(e)
	}
	if e = Valid(seal); e != nil {
		return fail(e)
	}
	if seal.Kind != 13 || len(seal.Tags) != 0 {
		return fail(errors.New("invalid seal"))
	}
	raw, e = decrypt(sk, seal.PubKey, seal.Content)
	if e != nil {
		return fail(e)
	}
	var rumor nostr.Event
	if e = json.Unmarshal([]byte(raw), &rumor); e != nil {
		return fail(e)
	}
	if rumor.Kind != RumorKind || rumor.PubKey != seal.PubKey || rumor.Sig != ([64]byte{}) || !validStrings(rumor) || EventID(rumor) != rumor.ID || Tag(rumor, "p") != sk.Public().Hex() || Tag(rumor, "t") != Namespace {
		return fail(errors.New("invalid rumor binding"))
	}
	var m Message
	if e = json.Unmarshal([]byte(rumor.Content), &m); e != nil {
		return fail(e)
	}
	if m.Version != 1 || len(m.ID) != 64 || len(m.Type) > 32 || m.Type == "" {
		return fail(errors.New("invalid application message"))
	}
	return seal.PubKey, m, nil
}
