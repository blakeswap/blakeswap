package chain

import (
	"context"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var ErrReplayUnsafe = errors.New("BTC inputs are not proven chain-exclusive: use split coins descended from a post-fork BTC coinbase")

// ProveBTCExclusive requires an ancestor coinbase committed on BTC and different
// from Blake's coinbase at the same BIP34 height. New addresses, separate keys,
// an absent transaction, and an absent UTXO are not replay protection. The proof
// is deliberately bounded and may reject legitimate split coins. No inference
// is made from an indexer's 'not found' reply.
func ProveBTCExclusive(ctx context.Context, network Network, btc, blake Backend, tx *wire.MsgTx) error {
	if network.Normalized() == Regtest {
		return nil
	} // Independent mined regtest chains; tested separately.
	budget := 512
	seen := map[string]bool{}
	var visit func(string, int) (bool, error)
	visit = func(id string, depth int) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if budget <= 0 || depth > 64 || seen[id] {
			return false, nil
		}
		budget--
		seen[id] = true
		t, err := btc.Transaction(ctx, id)
		if err != nil {
			return false, err
		}
		raw, err := parseRaw(t.Hex)
		if err != nil {
			return false, err
		}
		if raw.TxHash().String() != id {
			return false, errors.New("replay proof transaction ID mismatch")
		}
		if isCoinbase(raw) {
			if t.Height < network.ForkHeight() || t.Confirmations < 100 || !coinbaseHeight(raw, t.Height) {
				return false, nil
			}
			canonical, err := btc.Coinbase(ctx, t.Height)
			if err != nil {
				return false, err
			}
			other, err := blake.Coinbase(ctx, t.Height)
			if err != nil {
				return false, err
			}
			otherTx, err := parseRaw(other.Hex)
			if err != nil {
				return false, err
			}
			if canonical.TxID != id || canonical.Confirmations < 100 || !isCoinbase(otherTx) || !coinbaseHeight(otherTx, t.Height) || otherTx.TxHash().String() != other.TxID || other.Height != t.Height || other.Confirmations < network.Confirmations() {
				return false, nil
			}
			return canonical.TxID != other.TxID, nil
		}
		for _, in := range raw.TxIn {
			ok, err := visit(in.PreviousOutPoint.Hash.String(), depth+1)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	for _, in := range tx.TxIn {
		ok, err := visit(in.PreviousOutPoint.Hash.String(), 0)
		if err != nil {
			return fmt.Errorf("BTC replay proof: %w", err)
		}
		if ok {
			return nil
		}
	}
	return ErrReplayUnsafe
}
func isCoinbase(tx *wire.MsgTx) bool {
	return len(tx.TxIn) == 1 && tx.TxIn[0].PreviousOutPoint.Hash == (chainhash.Hash{}) && tx.TxIn[0].PreviousOutPoint.Index == ^uint32(0)
}
func coinbaseHeight(tx *wire.MsgTx, height uint32) bool {
	if !isCoinbase(tx) {
		return false
	}
	tok := txscript.MakeScriptTokenizer(0, tx.TxIn[0].SignatureScript)
	if !tok.Next() {
		return false
	}
	data := tok.Data()
	var n uint32
	if data == nil {
		if tok.Opcode() >= txscript.OP_1 && tok.Opcode() <= txscript.OP_16 {
			n = uint32(tok.Opcode() - txscript.OP_1 + 1)
		} else {
			return false
		}
	} else {
		if len(data) == 0 || len(data) > 5 || data[len(data)-1]&0x80 != 0 {
			return false
		}
		if len(data) == 5 && data[4] != 0 {
			return false
		}
		for i, b := range data {
			if i < 4 {
				n |= uint32(b) << (8 * i)
			}
		}
	}
	return n == height
}
