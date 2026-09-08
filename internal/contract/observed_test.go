package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func observedFixture(t *testing.T, id chain.ID, refund, multi bool, typ txscript.SigHashType, sequence uint32) (HTLC, *wire.MsgTx, []*wire.TxOut, *btcec.PrivateKey) {
	t.Helper()
	c, claim, refundKey, secret, payout := fixture(t, id)
	key, lock := claim, uint32(0)
	if refund {
		key, lock = refundKey, c.RefundHeight
	}
	tx, err := Spend(c, key, payout, 1000, refund, lock, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := c.PkScript()
	spent := []*wire.TxOut{wire.NewTxOut(c.Amount, pk)}
	if multi {
		op, _ := Outpoint(strings.Repeat("cd", 32), 2)
		extra := wire.NewTxIn(&op, nil, nil)
		extra.Sequence = wire.MaxTxInSequenceNum - 2
		// The additional input spends another HTLC with the same script but a
		// different exact outpoint/value. Sign it too: these are complete valid
		// witnesses, even though the observer verifies only its target input.
		extra.Witness = tx.Copy().TxIn[0].Witness
		tx.TxIn = append([]*wire.TxIn{extra}, tx.TxIn...)
		spent = append([]*wire.TxOut{wire.NewTxOut(250000, bytes.Clone(pk))}, spent...)
		tx.AddTxOut(wire.NewTxOut(249000, bytes.Clone(payout)))
	}
	tx.TxIn[len(tx.TxIn)-1].Sequence = sequence
	resignObserved(t, c, tx, spent, key, typ)
	return c, tx, spent, key
}

// BIP143 tests sign through btcd's ordinary full-prevout hash cache, independent
// of the observer's transaction-only midstate construction.
func resignObserved(t *testing.T, c HTLC, tx *wire.MsgTx, spent []*wire.TxOut, key *btcec.PrivateKey, typ txscript.SigHashType) {
	t.Helper()
	script, err := c.Script()
	if err != nil {
		t.Fatal(err)
	}
	fetch := txscript.NewMultiPrevOutFetcher(nil)
	for i, in := range tx.TxIn {
		fetch.AddPrevOut(in.PreviousOutPoint, spent[i])
	}
	for i := range tx.TxIn {
		var digest []byte
		if typ == UnifiedAll {
			digest, err = UnifiedDigest(tx, i, script, spent)
		} else {
			digest, err = txscript.CalcWitnessSigHash(script, txscript.NewTxSigHashes(tx, fetch), typ, tx, i, spent[i].Value)
		}
		if err != nil {
			t.Fatal(err)
		}
		tx.TxIn[i].Witness[0] = append(ecdsa.Sign(key, digest).Serialize(), byte(typ))
	}
}

func executeObservedBIP143(t *testing.T, tx *wire.MsgTx, spent []*wire.TxOut) {
	t.Helper()
	fetch := txscript.NewMultiPrevOutFetcher(nil)
	for i, in := range tx.TxIn {
		fetch.AddPrevOut(in.PreviousOutPoint, spent[i])
	}
	flags := txscript.ScriptBip16 | txscript.ScriptVerifyWitness | txscript.ScriptVerifyCheckLockTimeVerify | txscript.ScriptVerifyDERSignatures
	for i := range tx.TxIn {
		vm, err := txscript.NewEngine(spent[i].PkScript, tx, i, flags, nil, txscript.NewTxSigHashes(tx, fetch), spent[i].Value, fetch)
		if err != nil {
			t.Fatal(err)
		}
		if err = vm.Execute(); err != nil {
			t.Fatal("independent script validation failed", err)
		}
	}
}

func cloneObservedPrevouts(spent []*wire.TxOut) []*wire.TxOut {
	result := make([]*wire.TxOut, len(spent))
	for i, out := range spent {
		if out != nil {
			result[i] = wire.NewTxOut(out.Value, bytes.Clone(out.PkScript))
		}
	}
	return result
}

func TestObservedSpendAcceptsActualSequencesAtAnyInput(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		typ := txscript.SigHashAll
		if id == chain.Blake {
			typ = UnifiedAll
		}
		for _, refund := range []bool{false, true} {
			for _, multi := range []bool{false, true} {
				for _, sequence := range []uint32{0, 1, wire.MaxTxInSequenceNum - 2, wire.MaxTxInSequenceNum - 1, wire.MaxTxInSequenceNum} {
					if refund && sequence == wire.MaxTxInSequenceNum {
						continue
					}
					t.Run(fmt.Sprintf("%s/refund=%v/multi=%v/sequence=%d", id, refund, multi, sequence), func(t *testing.T) {
						c, tx, spent, _ := observedFixture(t, id, refund, multi, typ, sequence)
						// MsgTx.Copy normalizes nil witness elements; JSON preserves
						// their representation for this caller-ownership assertion.
						before, err := json.Marshal(tx)
						if err != nil {
							t.Fatal(err)
						}
						previous := cloneObservedPrevouts(spent)
						if err := VerifyObservedSpend(c, tx, spent); err != nil {
							t.Fatal(err)
						}
						after, err := json.Marshal(tx)
						if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(spent, previous) {
							t.Fatal("verification changed caller-owned evidence")
						}
						if !multi {
							if err := VerifyObservedSpend(c, tx, nil); err != nil {
								t.Fatal("single known target needed unrelated evidence", err)
							}
						}
						if multi || sequence != wire.MaxTxInSequenceNum-2 {
							if VerifySignature(c, tx, refund) == nil {
								t.Fatal("observer relaxed local template construction")
							}
						} else if err := VerifySignature(c, tx, refund); err != nil {
							t.Fatal("unchanged local template rejected", err)
						}
						if id == chain.BTC {
							executeObservedBIP143(t, tx, spent)
						}
					})
				}
			}
		}
	}
}

func TestObservedSpendBIP143ModesOnBothChains(t *testing.T) {
	types := []txscript.SigHashType{txscript.SigHashAll, txscript.SigHashNone, txscript.SigHashSingle, txscript.SigHashAll | txscript.SigHashAnyOneCanPay, txscript.SigHashNone | txscript.SigHashAnyOneCanPay, txscript.SigHashSingle | txscript.SigHashAnyOneCanPay}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, typ := range types {
			for _, refund := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/type=%x/refund=%v", id, typ, refund), func(t *testing.T) {
					c, tx, spent, key := observedFixture(t, id, refund, true, typ, wire.MaxTxInSequenceNum-1)
					if err := VerifyObservedSpend(c, tx, nil); err != nil {
						t.Fatal("BIP143 required unrelated previous outputs", err)
					}
					partial := []*wire.TxOut{nil, spent[1]}
					if err := VerifyObservedSpend(c, tx, partial); err != nil {
						t.Fatal(err)
					}
					executeObservedBIP143(t, tx, spent)
					if typ&0x1f == txscript.SigHashSingle {
						// Witness-v0 SINGLE is defined even without a paired output.
						tx.TxOut = tx.TxOut[:1]
						resignObserved(t, c, tx, spent, key, typ)
						if err := VerifyObservedSpend(c, tx, nil); err != nil {
							t.Fatal(err)
						}
						executeObservedBIP143(t, tx, spent)
					}
				})
			}
		}
	}
	// The fork accepts BIP143 witnesses, but our local signer must continue
	// requiring replay-protected UnifiedAll signatures on Blake.
	c, tx, spent, _ := observedFixture(t, chain.Blake, false, false, txscript.SigHashAll, wire.MaxTxInSequenceNum-2)
	if err := VerifyObservedSpend(c, tx, nil); err != nil {
		t.Fatal(err)
	}
	executeObservedBIP143(t, tx, spent)
	if VerifySignature(c, tx, false) == nil {
		t.Fatal("ordinary fork observation changed local replay policy")
	}
}

func TestObservedSpendRejectsMissingOrWrongUnifiedPrevouts(t *testing.T) {
	c, tx, spent, _ := observedFixture(t, chain.Blake, false, true, UnifiedAll, wire.MaxTxInSequenceNum)
	for name, mutate := range map[string]func([]*wire.TxOut) []*wire.TxOut{
		"missing":        func([]*wire.TxOut) []*wire.TxOut { return nil },
		"short":          func(p []*wire.TxOut) []*wire.TxOut { return p[:1] },
		"target missing": func(p []*wire.TxOut) []*wire.TxOut { p[1] = nil; return p },
		"other missing":  func(p []*wire.TxOut) []*wire.TxOut { p[0] = nil; return p },
		"target amount":  func(p []*wire.TxOut) []*wire.TxOut { p[1].Value++; return p },
		"target script":  func(p []*wire.TxOut) []*wire.TxOut { p[1].PkScript[3] ^= 1; return p },
		"other amount":   func(p []*wire.TxOut) []*wire.TxOut { p[0].Value++; return p },
		"other script":   func(p []*wire.TxOut) []*wire.TxOut { p[0].PkScript[3] ^= 1; return p },
		"other negative": func(p []*wire.TxOut) []*wire.TxOut { p[0].Value = -1; return p },
		"other overflow": func(p []*wire.TxOut) []*wire.TxOut { p[0].Value = MaxMoney + 1; return p },
		"reordered":      func(p []*wire.TxOut) []*wire.TxOut { p[0], p[1] = p[1], p[0]; return p },
	} {
		t.Run(name, func(t *testing.T) {
			if VerifyObservedSpend(c, tx, mutate(cloneObservedPrevouts(spent))) == nil {
				t.Fatal("missing or mismatched evidence accepted")
			}
		})
	}
	if err := VerifyObservedSpend(c, tx, spent); err != nil {
		t.Fatal("exact full evidence rejected", err)
	}
}

func TestObservedSpendRejectsInvalidWitnessAndTransaction(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		typ := txscript.SigHashAll
		if id == chain.Blake {
			typ = UnifiedAll
		}
		c, tx, spent, _ := observedFixture(t, id, false, false, typ, wire.MaxTxInSequenceNum)
		for name, mutate := range map[string]func(*wire.MsgTx){
			"signature":               func(x *wire.MsgTx) { x.TxIn[0].Witness[0][10] ^= 1 },
			"script":                  func(x *wire.MsgTx) { x.TxIn[0].Witness[3][3] ^= 1 },
			"wrong preimage":          func(x *wire.MsgTx) { x.TxIn[0].Witness[1][0] ^= 1 },
			"short preimage":          func(x *wire.MsgTx) { x.TxIn[0].Witness[1] = x.TxIn[0].Witness[1][:31] },
			"missing preimage":        func(x *wire.MsgTx) { x.TxIn[0].Witness[1] = nil },
			"wrong branch":            func(x *wire.MsgTx) { x.TxIn[0].Witness[2] = nil },
			"extra stack":             func(x *wire.MsgTx) { x.TxIn[0].Witness = append(wire.TxWitness{nil}, x.TxIn[0].Witness...) },
			"oversized selector":      func(x *wire.MsgTx) { x.TxIn[0].Witness[2] = bytes.Repeat([]byte{1}, txscript.MaxScriptElementSize+1) },
			"scriptSig":               func(x *wire.MsgTx) { x.TxIn[0].SignatureScript = []byte{0} },
			"changed signed sequence": func(x *wire.MsgTx) { x.TxIn[0].Sequence-- },
			"wrong outpoint":          func(x *wire.MsgTx) { x.TxIn[0].PreviousOutPoint.Index++ },
			"duplicate target":        func(x *wire.MsgTx) { x.TxIn = append(x.TxIn, x.Copy().TxIn[0]) },
			"nil input":               func(x *wire.MsgTx) { x.TxIn[0] = nil },
			"no inputs":               func(x *wire.MsgTx) { x.TxIn = nil },
			"nil output":              func(x *wire.MsgTx) { x.TxOut[0] = nil },
			"no outputs":              func(x *wire.MsgTx) { x.TxOut = nil },
			"negative output":         func(x *wire.MsgTx) { x.TxOut[0].Value = -1 },
			"output overflow":         func(x *wire.MsgTx) { x.TxOut = []*wire.TxOut{wire.NewTxOut(MaxMoney, nil), wire.NewTxOut(1, nil)} },
			"trailing DER bytes": func(x *wire.MsgTx) {
				sig := x.TxIn[0].Witness[0]
				x.TxIn[0].Witness[0] = append(append(bytes.Clone(sig[:len(sig)-1]), 0), sig[len(sig)-1])
			},
			"unsupported hash mode": func(x *wire.MsgTx) { sig := x.TxIn[0].Witness[0]; sig[len(sig)-1] = 0x41 },
		} {
			t.Run(string(id)+"/"+name, func(t *testing.T) {
				if VerifyObservedSpend(c, func() *wire.MsgTx { x := tx.Copy(); mutate(x); return x }(), nil) == nil {
					t.Fatal("invalid observation accepted")
				}
			})
		}
		for _, field := range []string{"amount", "script", "length", "missing target"} {
			p := cloneObservedPrevouts(spent)
			switch field {
			case "amount":
				p[0].Value++
			case "script":
				p[0].PkScript[3] ^= 1
			case "length":
				p = append(p, p[0])
			case "missing target":
				p[0] = nil
			}
			if VerifyObservedSpend(c, tx, p) == nil {
				t.Fatal("explicit target prevout mismatch accepted", id, field)
			}
		}
		if VerifyObservedSpend(c, nil, nil) == nil {
			t.Fatal("nil transaction accepted")
		}
	}
}

func TestObservedRefundChecksMatchedSequenceAndLockDomain(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		typ := txscript.SigHashAll
		if id == chain.Blake {
			typ = UnifiedAll
		}
		for _, mode := range []string{"final target", "height too early", "wrong time domain"} {
			t.Run(string(id)+"/"+mode, func(t *testing.T) {
				c, tx, spent, key := observedFixture(t, id, true, true, typ, wire.MaxTxInSequenceNum-1)
				switch mode {
				case "final target":
					tx.TxIn[1].Sequence = wire.MaxTxInSequenceNum
				case "height too early":
					tx.LockTime = c.RefundHeight - 1
				case "wrong time domain":
					tx.LockTime = txscript.LockTimeThreshold
				}
				resignObserved(t, c, tx, spent, key, typ)
				if VerifyObservedSpend(c, tx, spent) == nil {
					t.Fatal("valid signature bypassed CLTV")
				}
			})
		}
		c, tx, spent, key := observedFixture(t, id, true, true, typ, wire.MaxTxInSequenceNum-1)
		tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum
		resignObserved(t, c, tx, spent, key, typ)
		if err := VerifyObservedSpend(c, tx, spent); err != nil {
			t.Fatal("unrelated final input blocked target refund", err)
		}
	}
}

func TestObservedSpendUsesWitnessBooleanSemantics(t *testing.T) {
	for _, refund := range []bool{false, true} {
		c, tx, spent, _ := observedFixture(t, chain.BTC, refund, false, txscript.SigHashAll, wire.MaxTxInSequenceNum-1)
		selectors := [][]byte{{2}, {0, 1}}
		if refund {
			selectors = [][]byte{nil, {0}, {0x80}, {0, 0x80}}
		}
		for _, selector := range selectors {
			x := tx.Copy()
			x.TxIn[0].Witness[len(x.TxIn[0].Witness)-2] = selector
			if err := VerifyObservedSpend(c, x, nil); err != nil {
				t.Fatal("valid script boolean rejected", refund, err)
			}
			executeObservedBIP143(t, x, spent)
		}
	}
}

func TestObservedSpendTimestampRefundAndIncompleteTemplate(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		c, claim, refund, secret, payout := fixture(t, id)
		c.RefundHeight = txscript.LockTimeThreshold + 100
		tx, err := Spend(c, refund, payout, 2000, true, c.RefundHeight+1, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifyObservedSpend(c, tx, nil); err != nil {
			t.Fatal("matching timestamp refund rejected", id, err)
		}
		pk, _ := c.PkScript()
		if id == chain.BTC {
			executeObservedBIP143(t, tx, []*wire.TxOut{wire.NewTxOut(c.Amount, pk)})
		}
		// A pre-signed tower claim intentionally lacks a preimage. Its template
		// signature is valid, but it must never be mistaken for an actual spend.
		incomplete, err := Spend(c, claim, payout, 2000, false, c.RefundHeight, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifySignature(c, incomplete, false); err != nil {
			t.Fatal("unchanged incomplete template signature rejected", err)
		}
		if VerifyObservedSpend(c, incomplete, nil) == nil {
			t.Fatal("incomplete template became observed claim proof")
		}
		if err = FillSecret(c, incomplete, secret); err != nil {
			t.Fatal(err)
		}
		if err = VerifyObservedSpend(c, incomplete, nil); err != nil {
			t.Fatal("complete signed claim rejected", err)
		}
	}
}

func TestObservedSpendUnsupportedHashModesFailExplicitly(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		c, tx, _, _ := observedFixture(t, id, false, false, txscript.SigHashAll, wire.MaxTxInSequenceNum)
		modes := []byte{0, 4, 0x20, 0x22, 0x41, 0x80, 0xa1, 0xff}
		if id == chain.BTC {
			modes = append(modes, byte(UnifiedAll))
		}
		for _, mode := range modes {
			x := tx.Copy()
			sig := x.TxIn[0].Witness[0]
			sig[len(sig)-1] = mode
			if err := VerifyObservedSpend(c, x, nil); err == nil || !strings.Contains(err.Error(), "unsupported observed signature hash type") {
				t.Fatal("unsupported digest did not return an explicit error", id, mode, err)
			}
		}
	}
}

func TestObservedSecretExtractionAgreesWithClaimBranch(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		typ := txscript.SigHashAll
		if id == chain.Blake {
			typ = UnifiedAll
		}
		c, tx, spent, _ := observedFixture(t, id, false, true, typ, wire.MaxTxInSequenceNum)
		secret := bytes.Clone(tx.TxIn[1].Witness[1])
		for _, selector := range [][]byte{{1}, {2}, {0, 1}, {0x81}, {0x80, 0}} {
			x := tx.Copy()
			x.TxIn[1].Witness[2] = selector
			if err := VerifyObservedSpend(c, x, spent); err != nil {
				t.Fatal("true claim branch rejected", id, err)
			}
			got, ok := ExtractSecret(c, x)
			if !ok || !bytes.Equal(got, secret) {
				t.Fatal("verified claim classified without its secret", id)
			}
			got[0] ^= 1
			if !bytes.Equal(x.TxIn[1].Witness[1], secret) {
				t.Fatal("extracted secret aliases caller-owned witness")
			}
		}
		for name, mutate := range map[string]func(*wire.MsgTx){
			"false":          func(x *wire.MsgTx) { x.TxIn[1].Witness[2] = nil },
			"zero":           func(x *wire.MsgTx) { x.TxIn[1].Witness[2] = []byte{0} },
			"negative zero":  func(x *wire.MsgTx) { x.TxIn[1].Witness[2] = []byte{0, 0x80} },
			"other outpoint": func(x *wire.MsgTx) { x.TxIn[1].PreviousOutPoint.Index++ },
			"other script":   func(x *wire.MsgTx) { x.TxIn[1].Witness[3][3] ^= 1 },
			"other preimage": func(x *wire.MsgTx) { x.TxIn[1].Witness[1][0] ^= 1 },
			"short preimage": func(x *wire.MsgTx) { x.TxIn[1].Witness[1] = secret[:31] },
			"nil input":      func(x *wire.MsgTx) { x.TxIn[1] = nil },
		} {
			x := tx.Copy()
			mutate(x)
			if _, ok := ExtractSecret(c, x); ok {
				t.Fatal("nonclaim witness supplied secret", id, name)
			}
		}
		refundC, _, refundKey, _, payout := fixture(t, id)
		refund, err := Spend(refundC, refundKey, payout, 2000, true, refundC.RefundHeight, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = VerifyObservedSpend(refundC, refund, nil); err != nil {
			t.Fatal("complete refund fixture rejected", err)
		}
		if _, ok := ExtractSecret(refundC, refund); ok {
			t.Fatal("refund witness supplied claim preimage")
		}
		x := tx.Copy()
		x.TxIn[1].Witness[0] = nil
		if VerifyObservedSpend(c, x, spent) == nil {
			t.Fatal("missing signature became observed spend proof")
		}
		if got, ok := ExtractSecret(c, x); !ok || !bytes.Equal(got, secret) {
			t.Fatal("public preimage knowledge incorrectly depended on signature proof")
		}
		if _, ok := ExtractSecret(c, nil); ok {
			t.Fatal("nil transaction supplied preimage")
		}
	}
}
