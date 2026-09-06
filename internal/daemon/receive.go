package daemon

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
)

const maxReceiveIndex = 10000

type receiveAddress struct {
	address string
	script  []byte
	key     *btcec.PrivateKey
}

func (e *Engine) deriveReceive(id chain.ID, index uint32) (receiveAddress, error) {
	key, err := e.keys.Receive(id, index)
	if err != nil {
		return receiveAddress{}, err
	}
	address, script, err := wallet.AddressFor(e.Config.Network, key.PubKey())
	return receiveAddress{address, script, key}, err
}

func (e *Engine) receiveAddresses(id chain.ID) []string {
	addresses := make([]string, 0, len(e.receiveBook[id]))
	for _, entry := range e.receiveBook[id] {
		addresses = append(addresses, entry.address)
	}
	return addresses
}

func (e *Engine) loadReceiveAddresses(ctx context.Context, id chain.ID) error {
	if e.s.ReceiveIndexes == nil {
		e.s.ReceiveIndexes = map[chain.ID]uint32{}
	}
	if e.receiveBook == nil {
		e.receiveBook = map[chain.ID][]receiveAddress{}
	}
	index := e.s.ReceiveIndexes[id]
	if index > maxReceiveIndex {
		return errors.New("receive address history exceeds wallet capacity")
	}
	e.receiveBook[id] = nil
	for i := uint32(0); i <= index; i++ {
		entry, err := e.deriveReceive(id, i)
		if err != nil {
			return err
		}
		e.receiveBook[id] = append(e.receiveBook[id], entry)
	}
	w, err := e.nodes[id].Observe(ctx, "blakeswap-"+e.identity.Public().Hex()[:20], e.receiveAddresses(id))
	if err != nil {
		return err
	}
	e.watch[id] = w
	current := e.receiveBook[id][index]
	e.addresses[id], e.scripts[id] = current.address, current.script
	return nil
}

// Confirmed receipt history survives spending, unlike the current UTXO set.
// Persist each advance before exposing the replacement. Never roll an index back
// on a reorg, an observation error, or restart. Old scripts stay watched/signable.
func (e *Engine) rotateReceiveAddress(ctx context.Context, id chain.ID) error {
	for {
		current := e.receiveBook[id][len(e.receiveBook[id])-1]
		used, err := e.watch[id].ConfirmedReceived(ctx, current.address)
		if err != nil {
			return err
		}
		if !used {
			e.addresses[id] = current.address
			return nil
		}
		e.addresses[id] = ""
		index := e.s.ReceiveIndexes[id]
		if index >= maxReceiveIndex {
			return errors.New("receive address history exceeds wallet capacity")
		}
		next, err := e.deriveReceive(id, index+1)
		if err != nil {
			return err
		}
		w, err := e.nodes[id].Observe(ctx, "blakeswap-"+e.identity.Public().Hex()[:20], []string{next.address})
		if err != nil {
			return err
		}
		e.s.ReceiveIndexes[id] = index + 1
		if err := e.save(); err != nil {
			return err
		}
		e.receiveBook[id] = append(e.receiveBook[id], next)
		e.watch[id] = w
		e.addresses[id], e.scripts[id] = next.address, next.script
	}
}
