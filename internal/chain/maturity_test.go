package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCoinbaseMempoolMaturity(t *testing.T) {
	for _, tc := range []struct {
		n    Network
		id   ID
		want int
	}{
		{Mainnet, Blake, 6480}, {Testnet, Blake, 6705}, {Regtest, Blake, 100}, {"", Blake, 100},
		{Mainnet, BTC, 100}, {Testnet, BTC, 100}, {Regtest, BTC, 100},
	} {
		if got := tc.n.coinbaseMaturity(tc.id); got != tc.want {
			t.Fatalf("%s/%s: got %d want %d", tc.n, tc.id, got, tc.want)
		}
	}
}

func TestRPCRequiresLongCoinbaseDeployment(t *testing.T) {
	for _, n := range []Network{Mainnet, Testnet} {
		start, enforce, release := 973440, 973440, 979920
		if n == Testnet {
			start, enforce, release = 151406, 151550, 158111
		}
		for _, tip := range []int{start - 2, enforce - 2, enforce - 1, enforce, release - 2, release - 1, release} {
			for _, fault := range []string{"none", "moving tip", "missing", "wrong maturity", "wrong start", "wrong height", "wrong end", "wrong type", "wrong active", "missing active"} {
				t.Run(fmt.Sprintf("%s/%d/%s", n, tip, fault), func(t *testing.T) {
					d := map[string]any{"type": "flagday", "height": enforce, "height_end": release - 1, "coinbase_start_height": start, "maturity": n.coinbaseMaturity(Blake), "active": tip >= enforce-1 && tip < release-1}
					switch fault {
					case "wrong maturity":
						d["maturity"] = 100
					case "wrong start":
						d["coinbase_start_height"] = start + 1
					case "wrong height":
						d["height"] = enforce + 1
					case "wrong end":
						d["height_end"] = release
					case "wrong type":
						d["type"] = "bip9"
					case "wrong active":
						d["active"] = !d["active"].(bool)
					case "missing active":
						delete(d, "active")
					}
					rpc := fakeRPC(t, func(method string, params []json.RawMessage) (any, *RPCError) {
						switch method {
						case "getblockchaininfo":
							return map[string]any{"chain": n.NodeName(), "blocks": tip, "bestblockhash": "tip", "difficulty_blake2b": 1}, nil
						case "getblockhash":
							if string(params[0]) == "0" {
								return n.Genesis(), nil
							}
							return "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb", nil
						case "getblockheader":
							return strings.Repeat("00", 164), nil
						case "getdeploymentinfo":
							// A concurrently advanced/reorged tip can have the opposite
							// activation state; an explicit hash preserves the snapshot.
							if fault == "moving tip" && (len(params) != 1 || string(params[0]) != `"tip"`) {
								d["active"] = !d["active"].(bool)
							}
							deployments := map[string]any{}
							if fault != "missing" {
								deployments["long_coinbase_maturity"] = d
							}
							return map[string]any{"blake2b": map[string]any{"active": true, "height": n.ForkHeight()}, "deployments": deployments}, nil
						default:
							t.Errorf("unexpected RPC %s", method)
							return nil, nil
						}
					})
					rpc.ID, rpc.Network = Blake, n
					err := rpc.Check(context.Background())
					if (err == nil) != (fault == "none" || fault == "moving tip") {
						t.Fatalf("Check = %v", err)
					}
					if err != nil && !strings.Contains(err.Error(), "long_coinbase_maturity") {
						t.Fatal(err)
					}
				})
			}
		}
	}
	if err := checkLongCoinbaseDeployment(Regtest, 110, nil); err != nil {
		t.Fatal(err)
	}
}
