package contract

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

// Exercise the released node's long-maturity rules in a separate, temporary
// chainstate. Never change the maturity schedule of the shared regtest fixture.
func TestRealLongCoinbaseMaturity(t *testing.T) {
	root := os.Getenv("BLAKESWAP_REGTEST")
	if root == "" {
		t.Skip("requires downloaded regtest node")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:")
	listener.Close()
	data := t.TempDir()
	binary := filepath.Join(root, ".cache/nodes/blake/bitcoin-29.4.2.knots20260508/bin/bitcoind")
	cmd := exec.Command(binary, "-datadir="+data, "-regtest", "-daemonwait", "-listen=0", "-connect=0", "-dnsseed=0", "-discover=0", "-natpmp=0", "-txindex=1", "-rpcbind=127.0.0.1", "-rpcallowip=127.0.0.1", "-rpcport="+port, "-testactivationheight=blake2b@1", "-testcoinbasematuritylong=1:2:251")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start maturity fixture: %v: %s", err, output)
	}
	node, err := chain.New(chain.Blake, "http://127.0.0.1:"+port, filepath.Join(data, "regtest/.cookie"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { node.Call(ctx, "stop", nil); node.Close() })
	c, claim, refund, secret, recipient := fixture(t, chain.Blake)
	c.RefundHeight = 1000
	address, script, err := wallet.Address(refund.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	generate := func(count int) {
		t.Helper()
		if err := node.Call(ctx, "generatetoaddress", nil, count, address); err != nil {
			t.Fatal(err)
		}
	}
	generate(100)
	if err := node.Check(ctx); err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Deployments map[string]struct {
			Maturity int
			Active   bool
		}
	}
	if err := node.Call(ctx, "getdeploymentinfo", &deployment); err != nil {
		t.Fatal(err)
	}
	if d := deployment.Deployments["long_coinbase_maturity"]; d.Maturity != 250 || !d.Active {
		t.Fatalf("wrong fixture deployment: %+v", d)
	}
	fundingAt := func(height uint32) *chain.UTXO {
		t.Helper()
		coin, err := node.Coinbase(ctx, height)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := Parse(coin.Hex)
		if err != nil {
			t.Fatal(err)
		}
		return &chain.UTXO{TxID: coin.TxID, Vout: 0, Amount: chain.Coins(tx.TxOut[0].Value), Script: hex.EncodeToString(script), Confirmations: coin.Confirmations}
	}
	coin := fundingAt(1)
	funding, err := Fund(c, []chain.UTXO{*coin}, refund, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if reason := allowed(t, node, funding, false); reason != "bad-txns-premature-spend-of-coinbase" {
		t.Fatal(reason)
	}
	generate(149) // 249 confirmations: one below the new mempool threshold.
	allowed(t, node, funding, false)
	generate(1) // 250 confirmations: the next block is the release height.
	allowed(t, node, funding, true)
	// Even after the consensus window, policy still locks younger coinbases.
	younger := fundingAt(2)
	other, err := Fund(c, []chain.UTXO{*younger}, refund, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if reason := allowed(t, node, other, false); reason != "bad-txns-premature-spend-of-coinbase" {
		t.Fatal(reason)
	}
	broadcast(t, node, funding)
	generate(1)
	allowed(t, node, other, true)
	c.TxID, c.Vout = funding.TxHash().String(), 0
	spend, err := Spend(c, claim, recipient, 1000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	allowed(t, node, spend, true)
	broadcast(t, node, spend)
	generate(1)
	observed, err := node.Transaction(ctx, spend.TxHash().String())
	if err != nil || observed.Confirmations != 1 {
		t.Fatalf("claim not confirmed: %+v %v", observed, err)
	}
}
