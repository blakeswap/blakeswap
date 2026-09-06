package contract

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

func TestRealSettlementFeeVariantsKeepFundingAndPayoutAuthorization(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, kind := range []string{"claim", "refund", "tower"} {
			t.Run(string(id)+"/"+kind, func(t *testing.T) {
				r := realNode(t, id)
				ctx := context.Background()
				c, claim, refund, secret, recipient := fixture(t, id)
				height, err := r.Height(ctx)
				if err != nil {
					t.Fatal(err)
				}
				c.RefundHeight = height + 5
				addr, own, err := wallet.Address(refund.PubKey())
				if err != nil {
					t.Fatal(err)
				}
				name := "fees-" + hex.EncodeToString(refund.PubKey().SerializeCompressed())[:20]
				w, err := r.Observe(ctx, name, []string{addr})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := r.Call(ctx, "unloadwallet", nil, name); err != nil {
						t.Error(err)
					}
				})
				if err := r.WithWallet("faucet").Call(ctx, "sendtoaddress", nil, addr, chain.Coins(2000000)); err != nil {
					t.Fatal(err)
				}
				mine(t, r, 1)
				coins, err := w.Unspent(ctx, []string{addr})
				if err != nil {
					t.Fatal(err)
				}
				funding, err := Fund(c, coins, refund, 2000)
				if err != nil {
					t.Fatal(err)
				}
				broadcast(t, r, funding)
				mine(t, r, 1)
				c.TxID = funding.TxHash().String()
				// A recovery signed for this exact funding output remains valid throughout
				// settlement replacements: funding itself is never rebuilt or replaced.
				recovery, err := Spend(c, refund, own, 2000, true, c.RefundHeight, nil, 0, nil)
				if err != nil {
					t.Fatal(err)
				}
				key := claim
				lock := uint32(0)
				isRefund := kind == "refund"
				var towerScript []byte
				var bounty int64
				if isRefund {
					key = refund
					lock = c.RefundHeight
					recipient = own
				}
				if kind == "tower" {
					lock = height + 3
					towerScript = own
					bounty = 10000
				}
				h, _ := r.Height(ctx)
				if lock > h {
					mine(t, r, lock-h)
				}
				for _, fee := range []int64{2000, 6000, 20000} {
					tx, err := Spend(c, key, recipient, fee, isRefund, lock, towerScript, bounty, secret)
					if err != nil {
						t.Fatal(err)
					}
					if tx.TxIn[0].PreviousOutPoint.Hash.String() != c.TxID || tx.TxOut[0].Value != c.Amount-fee-bounty {
						t.Fatal("principal/outpoint changed")
					}
					if bounty > 0 && (tx.TxOut[1].Value != bounty || string(tx.TxOut[1].PkScript) != string(towerScript)) {
						t.Fatal("tower authorization changed")
					}
					if err := VerifySignature(c, recovery, true); err != nil {
						t.Fatal("lost original recovery", err)
					}
					allowed(t, r, tx, true)
					broadcast(t, r, tx)
					if fee == 20000 {
						mine(t, r, 1)
						known, err := r.Transaction(ctx, tx.TxHash().String())
						if err != nil || known.Confirmations < 1 {
							t.Fatal("replacement did not confirm", err)
						}
					}
				}
			})
		}
	}
}
