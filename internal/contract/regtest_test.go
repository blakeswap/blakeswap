package contract

import (
	"blakeswap/internal/chain"
	"blakeswap/internal/wallet"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"os"
	"path/filepath"
	"testing"
)

func realNode(t *testing.T, id chain.ID) *chain.RPC {
	t.Helper()
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("set BLAKESWAP_REGTEST to project root for real two-chain tests")
	}
	port := 19443
	if id == chain.Blake {
		port = 29443
	}
	r, e := chain.New(id, fmt.Sprintf("http://127.0.0.1:%d", port), filepath.Join(root, ".local", string(id), "regtest", ".cookie"))
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	return r
}
func mine(t *testing.T, r *chain.RPC, n uint32) {
	t.Helper()
	if n == 0 {
		return
	}
	ctx := context.Background()
	var addr string
	if e := r.WithWallet("faucet").Call(ctx, "getnewaddress", &addr); e != nil {
		t.Fatal(e)
	}
	if e := r.Call(ctx, "generatetoaddress", nil, n, addr); e != nil {
		t.Fatal(e)
	}
}
func allowed(t *testing.T, r *chain.RPC, tx *wire.MsgTx, want bool) string {
	t.Helper()
	var result []struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reject-reason"`
	}
	if e := r.Call(context.Background(), "testmempoolaccept", &result, []string{Hex(tx)}); e != nil {
		t.Fatal(e)
	}
	if len(result) != 1 || result[0].Allowed != want {
		t.Fatalf("allowed=%v want=%v response=%+v tx=%s", len(result) > 0 && result[0].Allowed, want, result, tx.TxHash())
	}
	return result[0].Reason
}
func broadcast(t *testing.T, r *chain.RPC, tx *wire.MsgTx) {
	t.Helper()
	if _, e := r.Broadcast(context.Background(), Hex(tx)); e != nil {
		t.Fatal(e)
	}
}
func TestRealContracts(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			r := realNode(t, id)
			ctx := context.Background()
			for _, scenario := range []string{"self-bypasses-fee", "delayed-tower", "refund"} {
				t.Run(scenario, func(t *testing.T) {
					c, claim, refund, s, recipient := fixture(t, id)
					height, e := r.Height(ctx)
					if e != nil {
						t.Fatal(e)
					}
					c.RefundHeight = height + 12
					addr, own, e := wallet.Address(refund.PubKey())
					if e != nil {
						t.Fatal(e)
					}
					w, e := r.Observe(ctx, "test-"+hex.EncodeToString(refund.PubKey().SerializeCompressed())[0:20], []string{addr})
					if e != nil {
						t.Fatal(e)
					}
					if e = r.WithWallet("faucet").Call(ctx, "sendtoaddress", nil, addr, chain.Coins(2000000)); e != nil {
						t.Fatal(e)
					}
					mine(t, r, 1)
					coins, e := w.Unspent(ctx, []string{addr})
					if e != nil {
						t.Fatal(e)
					}
					if len(coins) != 1 {
						t.Fatalf("UTXOs %d", len(coins))
					}
					funding, e := Fund(c, coins, refund, 1000)
					if e != nil {
						t.Fatal(e)
					}
					allowed(t, r, funding, true)
					broadcast(t, r, funding)
					mine(t, r, 1)
					c.TxID = funding.TxHash().String()
					c.Vout = 0
					self, e := Spend(c, claim, recipient, 1000, false, 0, nil, 0, s)
					if e != nil {
						t.Fatal(e)
					}
					fallback, e := Spend(c, claim, recipient, 3000, false, height+5, own, 10000, nil)
					if e != nil {
						t.Fatal(e)
					}
					if e = FillSecret(c, fallback, s); e != nil {
						t.Fatal(e)
					}
					refundTx, e := Spend(c, refund, own, 1000, true, c.RefundHeight, nil, 0, nil)
					if e != nil {
						t.Fatal(e)
					}
					if got := allowed(t, r, fallback, false); got != "non-final" {
						t.Fatalf("tower rejected for wrong reason: %s", got)
					}
					allowed(t, r, refundTx, false)
					allowed(t, r, self, true)
					// The fork's protection is one-way. Ordinary BTC signatures are
					// still valid on Blake2b; unified signatures fail on Bitcoin.
					otherRules := c
					otherRules.Chain = c.Chain.Other()
					otherClaim, err := Spend(otherRules, claim, recipient, 1000, false, 0, nil, 0, s)
					if err != nil {
						t.Fatal(err)
					}
					allowed(t, r, otherClaim, id == chain.Blake)
					if VerifySignature(c, otherClaim, false) == nil {
						t.Fatal("application accepted wrong-chain signature policy")
					}
					bad, _ := Parse(Hex(self))
					bad.TxIn[0].Witness[1][0] ^= 1
					allowed(t, r, bad, false)
					mutated, _ := Parse(Hex(fallback))
					mutated.LockTime = 0
					allowed(t, r, mutated, false)
					var settled *wire.MsgTx
					switch scenario {
					case "self-bypasses-fee":
						broadcast(t, r, self)
						mine(t, r, 1)
						mine(t, r, 3)
						allowed(t, r, fallback, false)
						settled = self
					case "delayed-tower":
						mine(t, r, 3)
						allowed(t, r, fallback, true)
						mutated, _ = Parse(Hex(fallback))
						mutated.TxOut[1].Value++
						allowed(t, r, mutated, false)
						broadcast(t, r, fallback)
						mine(t, r, 1)
						allowed(t, r, self, false)
						settled = fallback
					case "refund":
						h, _ := r.Height(ctx)
						mine(t, r, c.RefundHeight-h)
						allowed(t, r, refundTx, true)
						broadcast(t, r, refundTx)
						mine(t, r, 1)
						allowed(t, r, self, false)
						settled = refundTx
					}
					out, e := r.Output(ctx, c.TxID, c.Vout)
					if e != nil || out != nil {
						t.Fatalf("HTLC unspent after settlement: %v %v", out, e)
					}
					confirmed, e := r.Transaction(ctx, settled.TxHash().String())
					if e != nil || confirmed.Confirmations < 1 {
						t.Fatalf("not confirmed %v %v", confirmed, e)
					}
					t.Logf("%s funding=%s settlement=%s", scenario, c.TxID, settled.TxHash())
				})
			}
		})
	}
}
