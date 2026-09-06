package daemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// Protection belongs only to this wallet's encrypted local state.
func (s *Swap) protection() protocol.Tower {
	if s.Protection != nil {
		return *s.Protection
	}
	return protocol.Tower{}
}

func (e *Engine) ownOffer(o protocol.Offer) protocol.Offer {
	o.Tower, o.TowerBPS = nil, 0
	if tower, ok := e.s.OfferTowers[o.ID]; ok && tower.BPS > 0 {
		o.Tower, o.TowerBPS = &tower, tower.BPS
	}
	return o
}

// selectProtection consumes only an authenticated local command, never a peer's
// offer or terms. Both parties may independently choose their own provider.
func (e *Engine) selectProtection(o protocol.Offer, bps int64, pubkey string) (protocol.Tower, error) {
	o.Tower, o.TowerBPS = nil, bps
	if bps > 0 && pubkey != "" {
		pub, err := protocol.PublicKey(pubkey)
		if err != nil {
			return protocol.Tower{}, err
		}
		event, ok := e.s.Towers[pub.Hex()]
		if !ok {
			return protocol.Tower{}, errors.New("watchtower has not been discovered on your relays")
		}
		tower, err := protocol.DecodeTower(event, e.Config.Network, time.Now().Unix())
		if err != nil {
			return protocol.Tower{}, err
		}
		if tower.BPS != bps {
			return protocol.Tower{}, errors.New("watchtower fee changed; refresh the quote")
		}
		o.Tower = &tower
	}
	if err := o.Validate(time.Now().Unix()); err != nil {
		return protocol.Tower{}, err
	}
	tower, err := e.selectedTower(o)
	if err != nil {
		return protocol.Tower{}, err
	}
	if tower.BPS > 0 {
		if tower.PubKey == e.identity.Public().Hex() {
			return protocol.Tower{}, errors.New("select an independent watchtower")
		}
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			script, err := hex.DecodeString(tower.Scripts[id])
			if err != nil || len(script) != 22 || script[0] != 0 || script[1] != 20 {
				return protocol.Tower{}, errors.New("invalid tower payout")
			}
		}
	}
	return tower, nil
}

// Withdraw cached offers from the retired public schema before relay IO. This
// is cache hygiene, not protocol negotiation or support for legacy swaps.
func (e *Engine) scrubOfferCache() error {
	if e.s.OfferTowers == nil {
		e.s.OfferTowers = map[string]protocol.Tower{}
	}
	for _, event := range e.s.Offers {
		if !retiredOfferContent(event.Content) {
			continue
		}
		var o protocol.Offer
		if err := json.Unmarshal([]byte(event.Content), &o); err != nil {
			return err
		}
		o.Status = "cancelled"
		delete(e.s.OfferTowers, o.ID)
		delete(e.s.CoinReservations, "offer/"+o.ID)
		if err := e.publishOffer(o); err != nil {
			return err
		}
	}
	for id, d := range e.s.Outbox {
		if d.Event.Kind != transport.OfferKind {
			continue
		}
		if retiredOfferContent(d.Event.Content) {
			delete(e.s.Outbox, id)
		}
	}
	for id, event := range e.s.Book {
		if _, err := protocol.DecodeOffer(event, time.Now().Unix()); err != nil {
			delete(e.s.Book, id)
		}
	}
	return nil
}

func retiredOfferContent(content string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &fields) != nil {
		return true
	}
	return fields["tower"] != nil || fields["tower_bps"] != nil || fields["version"] != nil
}
