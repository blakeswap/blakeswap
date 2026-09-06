package contract

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestFundingSignsHistoricalReceiveInputsAndCurrentChange(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			c, a, b, _, change := fixture(t, id)
			keys := map[string]*btcec.PrivateKey{}
			var coins []chain.UTXO
			var spent []*wire.TxOut
			for i, key := range []*btcec.PrivateKey{a, b} {
				_, script, _ := wallet.Address(key.PubKey())
				encoded := hex.EncodeToString(script)
				keys[encoded] = key
				coins = append(coins, chain.UTXO{TxID: c.TxID, Vout: uint32(i), Amount: 600000, Script: encoded, Confirmations: 1})
				spent = append(spent, wire.NewTxOut(600000, script))
			}
			tx, err := FundWithKeys(c, coins, keys, change, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(tx.TxIn) != 2 || len(tx.TxOut) != 2 || tx.TxOut[1].Value != 199000 || !bytes.Equal(tx.TxOut[1].PkScript, change) {
				t.Fatal("incorrect funding or change")
			}
			for i, key := range []*btcec.PrivateKey{a, b} {
				pub := key.PubKey().SerializeCompressed()
				w := tx.TxIn[i].Witness
				if !bytes.Equal(w[1], pub) {
					t.Fatal("wrong input key")
				}
				code, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).AddData(btcutil.Hash160(pub)).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG).Script()
				digest, err := Digest(id, tx, i, code, spent)
				if err != nil {
					t.Fatal(err)
				}
				signature, err := ecdsa.ParseDERSignature(w[0][:len(w[0])-1])
				if err != nil || !signature.Verify(digest, key.PubKey()) {
					t.Fatal("invalid historical-key signature")
				}
			}
			delete(keys, coins[1].Script)
			if _, err := FundWithKeys(c, coins, keys, change, 1000); err == nil {
				t.Fatal("accepted missing key")
			}
		})
	}
}
