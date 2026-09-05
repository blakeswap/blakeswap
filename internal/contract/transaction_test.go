package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"os"
	"strings"
	"testing"
)

func fixture(t testing.TB, id chain.ID) (HTLC, *btcec.PrivateKey, *btcec.PrivateKey, []byte, []byte) {
	t.Helper()
	a, e := btcec.NewPrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	b, e := btcec.NewPrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	s := bytes.Repeat([]byte{42}, 32)
	h := sha256.Sum256(s)
	_, pk, e := wallet.Address(a.PubKey())
	if e != nil {
		t.Fatal(e)
	}
	return HTLC{Chain: id, Hash: hex.EncodeToString(h[:]), ClaimKey: hex.EncodeToString(a.PubKey().SerializeCompressed()), RefundKey: hex.EncodeToString(b.PubKey().SerializeCompressed()), RefundHeight: 200, Amount: 1000000, TxID: strings.Repeat("ab", 32)}, a, b, s, pk
}

func TestUpstreamUnifiedAllSegwitVectors(t *testing.T) {
	b, e := os.ReadFile("testdata/unified_sighash.json")
	if e != nil {
		t.Fatal(e)
	}
	var vectors [][]json.RawMessage
	if e = json.Unmarshal(b, &vectors); e != nil {
		t.Fatal(e)
	}
	n := 0
	for i, row := range vectors[1:] {
		var typ, scriptType, index int
		_ = json.Unmarshal(row[2], &index)
		_ = json.Unmarshal(row[3], &typ)
		_ = json.Unmarshal(row[4], &scriptType)
		if typ != 33 || scriptType != 1 {
			continue
		}
		n++
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var script, raw, want string
			_ = json.Unmarshal(row[0], &script)
			_ = json.Unmarshal(row[1], &raw)
			_ = json.Unmarshal(row[6], &want)
			tx, e := Parse(raw)
			if e != nil {
				t.Fatal(e)
			}
			sc, _ := hex.DecodeString(script)
			var pairs [][]json.RawMessage
			_ = json.Unmarshal(row[5], &pairs)
			var spent []*wire.TxOut
			for _, p := range pairs {
				var val int64
				var sh string
				_ = json.Unmarshal(p[0], &val)
				_ = json.Unmarshal(p[1], &sh)
				s, _ := hex.DecodeString(sh)
				spent = append(spent, wire.NewTxOut(val, s))
			}
			got, e := UnifiedDigest(tx, index, sc, spent)
			if e != nil {
				t.Fatal(e)
			}
			if hex.EncodeToString(got) != want {
				t.Fatalf("got %x want %s", got, want)
			}
		})
	}
	if n < 2 {
		t.Fatalf("only %d applicable vectors", n)
	}
	t.Logf("verified all %d upstream ALL/segwit-v0 vectors; unsupported modes deliberately excluded", n)
}
func TestSignatureCommitsEveryMutableField(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			c, key, _, _, pk := fixture(t, id)
			tx, e := Spend(c, key, pk, 1000, false, 150, pk, 10000, nil)
			if e != nil {
				t.Fatal(e)
			}
			if e = VerifySignature(c, tx, false); e != nil {
				t.Fatal(e)
			}
			mutations := map[string]func(*wire.MsgTx){"locktime": func(x *wire.MsgTx) { x.LockTime-- }, "version": func(x *wire.MsgTx) { x.Version++ }, "sequence": func(x *wire.MsgTx) { x.TxIn[0].Sequence++ }, "outpoint": func(x *wire.MsgTx) { x.TxIn[0].PreviousOutPoint.Index++ }, "user payout": func(x *wire.MsgTx) { x.TxOut[0].Value++ }, "tower payout": func(x *wire.MsgTx) { x.TxOut[1].Value++ }, "destination": func(x *wire.MsgTx) { x.TxOut[0].PkScript[4] ^= 1 }, "remove bounty": func(x *wire.MsgTx) { x.TxOut = x.TxOut[:1] }, "add input": func(x *wire.MsgTx) { x.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil)) }, "hash type": func(x *wire.MsgTx) { w := x.TxIn[0].Witness[0]; w[len(w)-1] = 0x81 }}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					x, e := Parse(Hex(tx))
					if e != nil {
						t.Fatal(e)
					}
					mutate(x)
					if VerifySignature(c, x, false) == nil {
						t.Fatal("mutation accepted")
					}
				})
			}
		})
	}
}
func TestSecretAndRefundIsolation(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		c, a, b, s, pk := fixture(t, id)
		tx, e := Spend(c, a, pk, 1000, false, 0, nil, 0, nil)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains([]byte(Hex(tx)), []byte(hex.EncodeToString(s))) {
			t.Fatal("template leaks secret")
		}
		if e = FillSecret(c, tx, bytes.Repeat([]byte{1}, 32)); e == nil {
			t.Fatal("wrong preimage accepted")
		}
		if e = FillSecret(c, tx, s); e != nil {
			t.Fatal(e)
		}
		if got, ok := ExtractSecret(c, tx); !ok || !bytes.Equal(got, s) {
			t.Fatal("preimage not recovered")
		}
		if _, e = Spend(c, b, pk, 1000, true, 199, nil, 0, nil); e == nil {
			t.Fatal("early refund constructed")
		}
		refund, e := Spend(c, b, pk, 1000, true, 200, nil, 0, nil)
		if e != nil {
			t.Fatal(e)
		}
		if e = VerifySignature(c, refund, true); e != nil {
			t.Fatal(e)
		}
		if VerifySignature(c, refund, false) == nil {
			t.Fatal("refund reinterpreted as claim")
		}
		if VerifySignature(c, tx, true) == nil {
			t.Fatal("claim reinterpreted as refund")
		}
		c.Chain = c.Chain.Other()
		if VerifySignature(c, tx, false) == nil {
			t.Fatal("signature replayed under wrong chain rules")
		}
	}
}
func FuzzParseTransaction(f *testing.F) {
	c, a, _, s, pk := fixture(f, chain.BTC)
	tx, e := Spend(c, a, pk, 1000, false, 0, nil, 0, s)
	if e != nil {
		f.Fatal(e)
	}
	f.Add(Hex(tx))
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 200000 {
			return
		}
		tx, e := Parse(raw)
		if e != nil {
			return
		}
		round, e := Parse(Hex(tx))
		if e != nil || round.TxHash() != tx.TxHash() {
			t.Fatal("serialization instability")
		}
		_, _ = ExtractSecret(c, tx)
	})
}
