package protocol

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

const TowerLifetime int64 = 3600
const DefaultTowerBPS int64 = 50

// Tower is a provider-authenticated quote. Event preserves its proof in an offer
// so a later settings change cannot redirect either party's rescue payments.
type Tower struct {
	Public  bool                `json:"public,omitempty"`
	PubKey  string              `json:"pubkey"`
	Scripts map[chain.ID]string `json:"scripts"`
	BPS     int64               `json:"bps"`
	Npub    string              `json:"npub,omitempty"`
	Name    string              `json:"name,omitempty"`
	Network chain.Network       `json:"network,omitempty"`
	Expires int64               `json:"expires,omitempty"`
	Event   string              `json:"event,omitempty"`
}

func PublicKey(value string) (nostr.PubKey, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "npub1") {
		prefix, decoded, err := nip19.Decode(value)
		if err == nil && prefix == "npub" {
			if pub, ok := decoded.(nostr.PubKey); ok {
				return pub, nil
			}
		}
		return nostr.PubKey{}, errors.New("invalid watchtower npub")
	}
	pub, err := nostr.PubKeyFromHex(value)
	if err != nil || !Hex32(value) {
		return nostr.PubKey{}, errors.New("enter a watchtower npub or public key")
	}
	return pub, nil
}

func DecodeTower(event nostr.Event, network chain.Network, now int64) (Tower, error) {
	var tower Tower
	if err := transport.Valid(event); err != nil {
		return tower, err
	}
	if event.Kind != transport.TowerKind || transport.Tag(event, "t") != network.Namespace() || transport.Tag(event, "d") != network.Namespace() || int64(event.CreatedAt) > now+60 || len(event.Content) > 4096 {
		return tower, errors.New("invalid watchtower announcement namespace or time")
	}
	if err := json.Unmarshal([]byte(event.Content), &tower); err != nil {
		return tower, err
	}
	if tower.Event != "" || tower.Network != network.Normalized() || tower.PubKey != event.PubKey.Hex() || tower.Npub != nip19.EncodeNpub(event.PubKey) || tower.Expires <= now || tower.Expires <= int64(event.CreatedAt) || tower.Expires > int64(event.CreatedAt)+TowerLifetime || transport.Tag(event, "expiration") != strconv.FormatInt(tower.Expires, 10) || len(tower.Name) > 80 || tower.BPS < 1 || tower.BPS > 1000 || len(tower.Scripts) != 2 {
		return tower, errors.New("invalid watchtower identity or quote")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		script, err := hex.DecodeString(tower.Scripts[id])
		if err != nil || len(script) != 22 || script[0] != 0 || script[1] != 20 {
			return tower, errors.New("invalid watchtower payout script")
		}
	}
	tower.Event = event.String()
	return tower, nil
}

// Verify authenticates an immutable quote at its issue time. Discovery and new
// orders additionally check expiry; already signed orders/terms remain valid.
func (t Tower) Verify() error {
	var event nostr.Event
	if err := json.Unmarshal([]byte(t.Event), &event); err != nil {
		return errors.New("missing signed watchtower quote")
	}
	verified, err := DecodeTower(event, t.Network, int64(event.CreatedAt))
	if err != nil {
		return err
	}
	if Digest(verified) != Digest(t) {
		return errors.New("watchtower quote differs from provider signature")
	}
	return nil
}
