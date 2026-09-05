package chain_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bytes"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/testutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestRealElectrumRejectsForgedObservations(t *testing.T) {
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("requires separate regtest nodes")
	}
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			port := fmt.Sprint(19443 + i*10000)
			if configured := os.Getenv("BLAKESWAP_" + strings.ToUpper(string(id)) + "_RPC_PORT"); configured != "" {
				port = configured
			}
			node, err := chain.New(id, "http://127.0.0.1:"+port, filepath.Join(root, ".local", string(id), "regtest/.cookie"))
			if err != nil {
				t.Fatal(err)
			}
			defer node.Close()
			ctx := context.Background()
			height, err := node.Height(ctx)
			if err != nil {
				t.Fatal(err)
			}
			coin, err := node.Coinbase(ctx, height)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := hex.DecodeString(coin.Hex)
			tx := wire.NewMsgTx(2)
			if err = tx.Deserialize(bytes.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
			_, addresses, _, err := txscript.ExtractPkScriptAddrs(tx.TxOut[0].PkScript, chain.Regtest.Params())
			if err != nil || len(addresses) != 1 {
				t.Fatal("fixture coinbase address", err)
			}
			for _, fault := range []string{"none", "wrong genesis", "wrong raw tx", "wrong merkle", "wrong amount", "duplicate UTXO"} {
				t.Run(fault, func(t *testing.T) {
					bridge, endpoint := testutil.NewElectrumBridge(t, node)
					bridge.Transform = func(method string, result any) any {
						switch {
						case fault == "wrong genesis" && method == "server.features":
							result.(map[string]any)["genesis_hash"] = chain.Mainnet.Genesis()
						case fault == "wrong raw tx" && method == "blockchain.transaction.get":
							result = "00000000"
						case fault == "wrong merkle" && method == "blockchain.transaction.get_merkle":
							result.(map[string]any)["merkle"] = []string{strings.Repeat("00", 32)}
						case fault == "wrong amount" && method == "blockchain.scripthash.listunspent":
							coins := result.([]map[string]any)
							if len(coins) > 0 {
								coins[0]["value"] = coins[0]["value"].(int64) + 1
							}
						case fault == "duplicate UTXO" && method == "blockchain.scripthash.listunspent":
							coins := result.([]map[string]any)
							if len(coins) > 0 {
								result = append(coins, coins[0])
							}
						}
						return result
					}
					e, err := chain.NewElectrum(chain.Regtest, id, endpoint, "")
					if err != nil {
						t.Fatal(err)
					}
					defer e.Close()
					err = e.Check(ctx)
					if fault == "wrong genesis" {
						if err == nil {
							t.Fatal("accepted foreign genesis")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if fault == "wrong amount" || fault == "duplicate UTXO" {
						_, err = e.Unspent(ctx, []string{addresses[0].EncodeAddress()})
					} else {
						var observed chain.Transaction
						observed, err = e.Coinbase(ctx, height)
						if fault == "none" && err == nil && (observed.TxID != coin.TxID || observed.Height != height) {
							t.Fatal("wrong inclusion", observed)
						}
					}
					if fault == "none" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("forged observation accepted")
					}
				})
			}
		})
	}
}
