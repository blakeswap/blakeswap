package wallet

import (
	"bytes"
	"github.com/blakeswap/blakeswap/internal/chain"
	"strings"
	"testing"
)

func TestHardenedSeparationAndRecovery(t *testing.T) {
	m, e := NewMnemonic()
	if e != nil {
		t.Fatal(e)
	}
	a, e := FromMnemonic(m)
	if e != nil {
		t.Fatal(e)
	}
	b, e := FromMnemonic(m)
	if e != nil {
		t.Fatal(e)
	}
	seen := map[string]bool{}
	for branch := uint32(0); branch < 3; branch++ {
		for _, label := range []string{"deposit", "swap-one", "swap-two"} {
			x, e := a.Derive(branch, label)
			if e != nil {
				t.Fatal(e)
			}
			y, e := b.Derive(branch, label)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(x.Serialize(), y.Serialize()) {
				t.Fatal("recovery differs")
			}
			key := string(x.Serialize())
			if seen[key] {
				t.Fatal("key reused")
			}
			seen[key] = true
		}
	}
	if _, e = a.Spending(chain.ID("wrong"), "x"); e == nil {
		t.Fatal("unknown chain")
	}
	if _, e = FromMnemonic("not a mnemonic"); e == nil {
		t.Fatal("invalid mnemonic")
	}
}

func TestNetworkDerivationsRecoverWithoutReusingKeys(t *testing.T) {
	mnemonic, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		a, _ := FromMnemonic(mnemonic)
		a.SetNetwork(network)
		b, _ := FromMnemonic(mnemonic)
		b.SetNetwork(network)
		for branch := uint32(0); branch < 3; branch++ {
			key, err := a.Derive(branch, "deposit")
			if err != nil {
				t.Fatal(err)
			}
			restored, err := b.Derive(branch, "deposit")
			if err != nil || !bytes.Equal(key.Serialize(), restored.Serialize()) {
				t.Fatal("network key recovery failed")
			}
			if seen[string(key.Serialize())] {
				t.Fatal("key reused between chains/networks")
			}
			seen[string(key.Serialize())] = true
			address, _, err := AddressFor(network, key.PubKey())
			if err != nil {
				t.Fatal(err)
			}
			prefix := "bcrt1"
			if network == chain.Testnet {
				prefix = "tb1"
			}
			if network == chain.Mainnet {
				prefix = "bc1"
			}
			if !strings.HasPrefix(address, prefix) {
				t.Fatal(address)
			}
		}
	}
}

func TestReceiveIndexesPreserveLegacyKeysAndSeparateChainsAndNetworks(t *testing.T) {
	mnemonic, _ := NewMnemonic()
	seen := map[string]bool{}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		keys, _ := FromMnemonic(mnemonic)
		keys.SetNetwork(network)
		restored, _ := FromMnemonic(mnemonic)
		restored.SetNetwork(network)
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			legacy, _ := keys.Spending(id, "deposit")
			for index := uint32(0); index < 3; index++ {
				key, err := keys.Receive(id, index)
				if err != nil {
					t.Fatal(err)
				}
				recovery, err := restored.Receive(id, index)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(key.Serialize(), recovery.Serialize()) {
					t.Fatal("unstable recovery")
				}
				if index == 0 && !bytes.Equal(key.Serialize(), legacy.Serialize()) {
					t.Fatal("legacy address changed")
				}
				if seen[string(key.Serialize())] {
					t.Fatal("receive key reused")
				}
				seen[string(key.Serialize())] = true
			}
		}
	}
}
