package wallet

import (
	"blakeswap/internal/chain"
	"bytes"
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
