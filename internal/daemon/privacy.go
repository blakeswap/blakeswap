package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// protection is local to this wallet. Legacy accepted swaps retain their exact
// signed policy; v2 never derives our protection from the counterparty's terms.
func (s *Swap) protection() protocol.Tower {
	if s.Terms != nil && s.Terms.Version == 1 {
		return protocol.Tower{PubKey: s.Terms.Tower, Scripts: s.Terms.TowerScripts, BPS: s.Terms.Offer().TowerBPS}
	}
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
	return e.selectedTower(o)
}

// Run before any relay IO. Replace legacy publications, including pending
// retries, without altering accepted contracts or their recovery capabilities.
func (e *Engine) migrateOfferPrivacy() error {
	if e.s.OfferTowers == nil {
		e.s.OfferTowers = map[string]protocol.Tower{}
	}
	for _, event := range e.s.Offers {
		var o protocol.Offer
		if err := json.Unmarshal([]byte(event.Content), &o); err != nil {
			return err
		}
		if o.Version == 2 {
			continue
		}
		tower, err := e.selectedTower(o)
		if err != nil && o.Status == "open" && o.Expires > time.Now().Unix() {
			return err
		}
		if err == nil {
			e.s.OfferTowers[o.ID] = tower
		}
		if err := e.publishOffer(o); err != nil {
			return err
		}
	}
	for id, d := range e.s.Outbox {
		if d.Event.Kind != transport.OfferKind {
			continue
		}
		var o protocol.Offer
		if json.Unmarshal([]byte(d.Event.Content), &o) != nil || o.Version != 2 {
			delete(e.s.Outbox, id)
		}
	}
	return nil
}
