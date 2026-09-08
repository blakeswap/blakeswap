package daemon

import (
	"encoding/json"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// Opaque outgoing ciphertext cannot be decoded with only the sender's key.
// Its current-format provenance is authenticated by the enclosing vault record
// and written at the same time as the newly wrapped message. A changed outer
// State marker cannot upgrade an old delivery or authorize its publication.
func validateDeliveryFormat(network chain.Network, id string, d *Delivery) error {
	if d == nil || d.Version != transport.MessageVersion || d.Network != network || d.MessageID != id || !protocol.Hex32(id) {
		return errors.New("incompatible owned delivery format")
	}
	if err := transport.Valid(d.Event); err != nil {
		return err
	}
	if d.Type == "" {
		if !d.IsAck || d.To != "" || d.SwapID != "" || d.Event.ID.Hex() != id {
			return errors.New("invalid public delivery provenance")
		}
		switch d.Event.Kind {
		case transport.OfferKind:
			var retained protocol.Offer
			if err := json.Unmarshal([]byte(d.Event.Content), &retained); err != nil {
				return err
			}
			at := int64(d.Event.CreatedAt)
			// A later terminal revision may close a parent after its negotiation
			// expiry. Authenticate its format without reviving that deadline.
			if retained.Status != "open" && retained.Expires > 0 && retained.Expires <= at {
				at = retained.Expires - 1
			}
			offer, err := protocol.DecodeOffer(d.Event, at)
			if err != nil {
				return err
			}
			if offer.Network != network {
				return errors.New("public delivery network mismatch")
			}
			return nil
		case transport.TowerKind:
			_, err := protocol.DecodeTower(d.Event, network, int64(d.Event.CreatedAt))
			return err
		default:
			return errors.New("unsupported public protocol delivery")
		}
	}
	if d.Event.Kind != 1059 || !protocol.Hex32(d.To) || transport.Tag(d.Event, "p") != d.To || !protocol.Hex32(d.Digest) || (d.SwapID != "" && !protocol.Hex32(d.SwapID)) || d.IsAck != (d.Type == "ack") {
		return errors.New("invalid private delivery provenance")
	}
	return nil
}
