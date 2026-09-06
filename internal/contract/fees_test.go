package contract

import (
	"encoding/hex"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"testing"
)

func TestVirtualSizeBoundsInputCountsAndOutputScripts(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, count := range []int{1, 2, 50} {
			for _, length := range []int{22, 25, 34} {
				_, key, _, _, _ := fixture(t, id)
				_, change, err := wallet.Address(key.PubKey())
				if err != nil {
					t.Fatal(err)
				}
				var coins []chain.UTXO
				for i := 0; i < count; i++ {
					coins = append(coins, chain.UTXO{TxID: fmt.Sprintf("%064x", i+1), Amount: 1000000, Script: hex.EncodeToString(change), Confirmations: 2})
				}
				dest := make([]byte, length)
				bound, err := PaymentVSize(count, dest, change)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := PayWithKeys(id, 100000, dest, coins, map[string]*btcec.PrivateKey{hex.EncodeToString(change): key}, change, 2000)
				if err != nil {
					t.Fatal(err)
				}
				if actual := VirtualSize(tx); actual > bound || actual < bound-int64(count) {
					t.Fatalf("%s %d inputs, script %d: actual %d, bound %d", id, count, length, actual, bound)
				}
				fee, err := FeeForVSize(6539, bound)
				if err != nil || fee*1000 < bound*6539 || (fee-1)*1000 >= bound*6539 {
					t.Fatal("fee did not round upward", fee, err)
				}
			}
		}
	}
}
