package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestStateCutoverOwnedDeliveryProvenanceActiveAndCold(t *testing.T) {
	for _, kind := range []string{"outbox", "quarantined_outbox"} {
		for _, cold := range []bool{false, true} {
			for _, version := range []int{0, 2, transport.MessageVersion} {
				t.Run(kind+"/"+string(rune('0'+version))+map[bool]string{true: "/cold", false: "/active"}[cold], func(t *testing.T) {
					e := &Engine{Config: Config{Network: chain.Regtest}, identity: nostr.Generate()}
					d, err := e.prepareDelivery(nostr.Generate().Public().Hex(), "ack", transport.RandomID(), json.RawMessage(`{"id":"retained"}`))
					if err != nil {
						t.Fatal(err)
					}
					d.Version = version
					s := State{Version: StateVersion, Network: chain.Regtest}
					if kind == "outbox" {
						s.Outbox = map[string]*Delivery{d.MessageID: d}
					} else {
						s.Recovery = &RecoveryRecord{Outbox: map[string]*Delivery{d.MessageID: d}}
					}
					path := filepath.Join(t.TempDir(), "state.db")
					password := []byte("disposable-delivery-cutover")
					v, err := storage.Open(path, password)
					if err != nil {
						t.Fatal(err)
					}
					if cold {
						data, _ := json.Marshal(d)
						record := storage.ArchiveRecord{Kind: kind, ID: d.MessageID, Data: data}
						x := &Engine{s: s}
						if err := x.archiveDelta(record, true); err != nil {
							t.Fatal(err)
						}
						s = x.s
						if kind == "outbox" {
							s.Outbox = nil
						} else {
							s.Recovery.Outbox = nil
						}
						if _, err := v.CommitArchive(s, storage.ArchiveBatch{Put: []storage.ArchiveRecord{record}}, 0); err != nil {
							t.Fatal(err)
						}
					} else {
						// Deliberately prepare an authenticated incompatible fixture
						// without the typed State writer's own format validation.
						raw, err := json.Marshal(s)
						if err != nil {
							t.Fatal(err)
						}
						var stored map[string]any
						if err := json.Unmarshal(raw, &stored); err != nil {
							t.Fatal(err)
						}
						if err := v.Save(stored); err != nil {
							t.Fatal(err)
						}
					}
					if err := v.Close(); err != nil {
						t.Fatal(err)
					}
					before, _ := os.ReadFile(path)
					err = PreflightStateVersion(path, password)
					if (err == nil) != (version == transport.MessageVersion) {
						t.Fatalf("preflight version%d: %v", version, err)
					}
					opened, _, err := openCurrentStateVault(path, password)
					if opened != nil {
						opened.Close()
					}
					if (err == nil) != (version == transport.MessageVersion) {
						t.Fatalf("writer version%d: %v", version, err)
					}
					after, _ := os.ReadFile(path)
					if !bytes.Equal(before, after) {
						t.Fatal("format inspection modified the retained source")
					}
				})
			}
		}
	}
}

func TestStateCutoverClosedPublicDeliveryRetainsOriginalExpiry(t *testing.T) {
	p, maker := fillParentFixture(t, chain.BTC, 0)
	e := &Engine{Config: Config{Network: chain.Regtest}, identity: maker}
	p.Offer.Status, p.Offer.Available = "cancelled", 0
	event, err := e.signOffer(p.Offer, nostr.Timestamp(p.Offer.Expires+1))
	if err != nil {
		t.Fatal(err)
	}
	d := &Delivery{Version: transport.MessageVersion, Network: chain.Regtest, Event: event, MessageID: event.ID.Hex(), IsAck: true}
	if err := validateDeliveryFormat(chain.Regtest, d.MessageID, d); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.DecodeOffer(event, p.Offer.Expires+1); err == nil {
		t.Fatal("retained format inspection extended negotiation expiry")
	}
	p.Offer.Version = 2
	event, err = e.signOffer(p.Offer, event.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	d.Event, d.MessageID = event, event.ID.Hex()
	if err := validateDeliveryFormat(chain.Regtest, d.MessageID, d); err == nil {
		t.Fatal("old closed public format accepted")
	}
}
