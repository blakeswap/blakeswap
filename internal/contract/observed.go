package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// VerifyObservedSpend checks the actual witness spending c, not our fixed local
// signing template. It does not prove inclusion, finality, or the other inputs'
// scripts. The caller must authenticate c's funding output and chain observation.
//
// Both chains support BIP143 ALL/NONE/SINGLE, optionally ANYONECANPAY. These
// digests need only c's known script/value, so spent may be nil. Blake UnifiedAll
// additionally commits every previous output: multi-input observations require
// an exact input-aligned spent vector authenticated by the caller against each
// input's previous transaction hash and output index. A single input uses c.
// Any supplied vector must agree with c at the matched input. Unsupported modes
// or unavailable evidence return errors; they must never be treated as a spend
// proof. Local Spend and VerifySignature retain their stricter signing policy.
func VerifyObservedSpend(c HTLC, tx *wire.MsgTx, spent []*wire.TxOut) error {
	script, err := c.Script()
	if err != nil {
		return err
	}
	pk, err := c.PkScript()
	if err != nil {
		return err
	}
	if len(c.TxID) != chainhash.MaxHashStringSize {
		return errors.New("invalid observed contract outpoint")
	}
	op, err := Outpoint(c.TxID, c.Vout)
	if err != nil {
		return err
	}
	if tx == nil || len(tx.TxIn) == 0 || len(tx.TxOut) == 0 {
		return errors.New("missing observed transaction inputs or outputs")
	}
	index := -1
	seen := make(map[wire.OutPoint]bool, len(tx.TxIn))
	for i, in := range tx.TxIn {
		if in == nil || seen[in.PreviousOutPoint] {
			return errors.New("nil or duplicate observed input")
		}
		seen[in.PreviousOutPoint] = true
		if in.PreviousOutPoint == op {
			index = i
		}
	}
	if index < 0 {
		return errors.New("observed transaction does not spend contract")
	}
	var total int64
	for _, out := range tx.TxOut {
		if out == nil || out.Value < 0 || out.Value > MaxMoney-total {
			return errors.New("invalid observed output")
		}
		total += out.Value
	}
	if spent != nil {
		if len(spent) != len(tx.TxIn) || spent[index] == nil || spent[index].Value != c.Amount || !bytes.Equal(spent[index].PkScript, pk) {
			return errors.New("observed contract prevout mismatch")
		}
	}
	in := tx.TxIn[index]
	w := in.Witness
	if len(in.SignatureScript) != 0 || (len(w) != 3 && len(w) != 4) || !bytes.Equal(w[len(w)-1], script) {
		return errors.New("invalid observed witness script or stack")
	}
	selector := w[len(w)-2]
	if len(selector) > txscript.MaxScriptElementSize {
		return errors.New("oversized observed branch selector")
	}
	claim := observedScriptBool(selector)
	key := c.RefundKey
	if claim {
		if len(w) != 4 || len(w[1]) != 32 {
			return errors.New("invalid observed claim preimage")
		}
		hash := sha256.Sum256(w[1])
		expected, _ := hex.DecodeString(c.Hash) // Script validated the exact hash.
		if !bytes.Equal(hash[:], expected) {
			return errors.New("invalid observed claim preimage")
		}
		key = c.ClaimKey
	} else if len(w) != 3 || in.Sequence == wire.MaxTxInSequenceNum || tx.LockTime < c.RefundHeight || (tx.LockTime < txscript.LockTimeThreshold) != (c.RefundHeight < txscript.LockTimeThreshold) {
		return errors.New("invalid observed refund locktime or sequence")
	}
	if len(w[0]) < 9 || len(w[0]) > 73 {
		return errors.New("invalid observed signature encoding")
	}
	der := w[0][:len(w[0])-1]
	// ParseDERSignature tolerates bytes after its declared sequence. Script's
	// strict DER rule does not, so enforce the complete declared length here.
	if int(der[1])+2 != len(der) {
		return errors.New("invalid observed signature length")
	}
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		return err
	}
	typ := txscript.SigHashType(w[0][len(w[0])-1])
	var digest []byte
	switch typ {
	case txscript.SigHashAll, txscript.SigHashNone, txscript.SigHashSingle,
		txscript.SigHashAll | txscript.SigHashAnyOneCanPay,
		txscript.SigHashNone | txscript.SigHashAnyOneCanPay,
		txscript.SigHashSingle | txscript.SigHashAnyOneCanPay:
		digest, err = observedWitnessDigest(tx, index, script, c.Amount, typ)
	case UnifiedAll:
		if c.Chain != chain.Blake {
			return errors.New("unsupported observed signature hash type")
		}
		if spent == nil && len(tx.TxIn) == 1 {
			spent = []*wire.TxOut{wire.NewTxOut(c.Amount, pk)}
		}
		digest, err = UnifiedDigest(tx, index, script, spent)
	default:
		return errors.New("unsupported observed signature hash type")
	}
	if err != nil {
		return err
	}
	keyBytes, _ := hex.DecodeString(key)
	pub, err := btcec.ParsePubKey(keyBytes)
	if err != nil {
		return err
	}
	if !sig.Verify(digest, pub) {
		return errors.New("invalid observed signature")
	}
	return nil
}

// Witness-v0 IF uses script boolean semantics, including negative zero. The
// minimal-IF relay policy is not a restriction on an already observed spend.
func observedScriptBool(value []byte) bool {
	for i, b := range value {
		if b != 0 {
			return i != len(value)-1 || b != 0x80
		}
	}
	return false
}

func observedWitnessDigest(tx *wire.MsgTx, index int, script []byte, amount int64, typ txscript.SigHashType) ([]byte, error) {
	// Construct only BIP143's transaction-derived midstates. NewTxSigHashes
	// also inspects every prevout for taproot, which would require inventing
	// unrelated input metadata when only this witness-v0 output is known.
	var prevouts, sequences, outputs bytes.Buffer
	for _, in := range tx.TxIn {
		prevouts.Write(in.PreviousOutPoint.Hash[:])
		write(&prevouts, in.PreviousOutPoint.Index)
		write(&sequences, in.Sequence)
	}
	for _, out := range tx.TxOut {
		if err := wire.WriteTxOut(&outputs, 0, tx.Version, out); err != nil {
			return nil, err
		}
	}
	hashes := &txscript.TxSigHashes{SegwitSigHashMidstate: txscript.SegwitSigHashMidstate{
		HashPrevOutsV0: chainhash.DoubleHashH(prevouts.Bytes()),
		HashSequenceV0: chainhash.DoubleHashH(sequences.Bytes()),
		HashOutputsV0:  chainhash.DoubleHashH(outputs.Bytes()),
	}}
	return txscript.CalcWitnessSigHash(script, hashes, typ, tx, index, amount)
}
