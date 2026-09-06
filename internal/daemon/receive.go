package daemon

import (
	"context"
	"encoding/hex"
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
	if e.receiveReady == nil {
		e.receiveReady = map[chain.ID]bool{}
	}
	e.receiveReady[id] = false
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
			e.receiveReady[id] = true
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
		var w chain.Backend
		if rpc, ok := e.nodes[id].(*chain.RPC); ok && e.receiveReady[id] {
			w, err = rpc.ObserveNew(ctx, "blakeswap-"+e.identity.Public().Hex()[:20], []string{next.address})
		} else {
			w, err = e.nodes[id].Observe(ctx, "blakeswap-"+e.identity.Public().Hex()[:20], []string{next.address})
		}
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

// Startup inventories every known script before publishing readiness. During
// normal operation, bounded round-robin polling keeps late payments observable
// without letting an ever-growing history starve swap/rescue advancement.
func (e *Engine) refreshWalletCoins(ctx context.Context, id chain.ID) ([]chain.UTXO, error) {
	if e.walletCoins == nil {
		e.walletCoins = map[chain.ID]map[string][]chain.UTXO{}
		e.walletCursor = map[chain.ID]int{}
	}
	entries := e.receiveBook[id]
	var polled []receiveAddress
	if e.walletCoins[id] == nil {
		polled = entries
	} else {
		polled = append(polled, entries[len(entries)-1])
		historical := len(entries) - 1
		for i := 0; i < 8 && i < historical; i++ {
			cursor := e.walletCursor[id] % historical
			polled = append(polled, entries[cursor])
			e.walletCursor[id] = (cursor + 1) % historical
		}
	}
	addresses := make([]string, 0, len(polled))
	scripts := map[string]bool{}
	for _, entry := range polled {
		addresses = append(addresses, entry.address)
		scripts[hex.EncodeToString(entry.script)] = true
	}
	coins, err := e.watch[id].Unspent(ctx, addresses)
	if err != nil {
		return nil, err
	}
	updates := map[string][]chain.UTXO{}
	for script := range scripts {
		updates[script] = nil
	}
	for _, coin := range coins {
		if !scripts[coin.Script] {
			return nil, errors.New("wallet backend returned an unrequested script")
		}
		updates[coin.Script] = append(updates[coin.Script], coin)
	}
	if e.walletCoins[id] == nil {
		e.walletCoins[id] = map[string][]chain.UTXO{}
	}
	for script, coins := range updates {
		e.walletCoins[id][script] = coins
	}
	return e.knownCoins(id), nil
}
func (e *Engine) knownCoins(id chain.ID) []chain.UTXO {
	var coins []chain.UTXO
	for _, entry := range e.receiveBook[id] {
		coins = append(coins, e.walletCoins[id][hex.EncodeToString(entry.script)]...)
	}
	return coins
}
