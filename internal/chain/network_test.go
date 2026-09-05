package chain

import (
	"encoding/hex"
	"testing"
)

func TestNetworkIdentitiesAndBlakeCheckpoint(t *testing.T) {
	for _, n := range []Network{Regtest, Testnet, Mainnet} {
		if !n.Valid() || n.Domain(BTC) == n.Domain(Blake) {
			t.Fatal(n)
		}
		for _, other := range []Network{Regtest, Testnet, Mainnet} {
			if other != n && (n.Namespace() == other.Namespace() || n.KeyContext("deposit") == other.KeyContext("deposit")) {
				t.Fatal("cross-network collision")
			}
		}
	}
	raw := "000000a0657e02138733654183a2c7320d85ca9d743fe139c4bb01000000000000000000c137a8515a0f6b3aaf6049cc7611787c022ad523d51094be0a0363d0dc0bc7684dca936a4f8d001a5671798c84daeb494dca936a00000000b1ccf00d0300000000000000000000001e0300000000000000000000000000000000000068ac0e000000000000000000000000000000000000000000000000000000000000000000"
	header, _ := hex.DecodeString(raw)
	hash, err := HeaderHash(header)
	if err != nil || hash.String() != "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb" {
		t.Fatal(hash, err)
	}
	if err = verifyHeaderWork(header); err != nil {
		t.Fatal(err)
	}
	header[36] ^= 1
	if verifyHeaderWork(header) == nil {
		t.Fatal("tampered merkle root retained proof of work")
	}
}
