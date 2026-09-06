// Package contract implements the deliberately narrow v1 contract and signer.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const Dust int64 = 600
const MaxMoney int64 = 2100000000000000
const UnifiedAll txscript.SigHashType = 0x21

type HTLC struct {
	Chain        chain.ID `json:"chain"`
	Hash         string   `json:"hash"`
	ClaimKey     string   `json:"claim_key"`
	RefundKey    string   `json:"refund_key"`
	RefundHeight uint32   `json:"refund_height"`
	Amount       int64    `json:"amount"`
	TxID         string   `json:"txid,omitempty"`
	Vout         uint32   `json:"vout"`
}

func (c HTLC) Script() ([]byte, error) {
	h, e := hex.DecodeString(c.Hash)
	if e != nil || len(h) != 32 {
		return nil, errors.New("hash must be 32 bytes")
	}
	claim, e := hex.DecodeString(c.ClaimKey)
	if e != nil {
		return nil, e
	}
	refund, e := hex.DecodeString(c.RefundKey)
	if e != nil {
		return nil, e
	}
	if len(claim) != 33 || len(refund) != 33 {
		return nil, errors.New("compressed keys required")
	}
	if _, e = btcec.ParsePubKey(claim); e != nil {
		return nil, e
	}
	if _, e = btcec.ParsePubKey(refund); e != nil {
		return nil, e
	}
	if !c.Chain.Valid() || c.RefundHeight < 1 || c.RefundHeight > 4000000000 || c.Amount < Dust || c.Amount > MaxMoney {
		return nil, errors.New("invalid HTLC bounds")
	}
	return txscript.NewScriptBuilder().AddOp(txscript.OP_IF).AddOp(txscript.OP_SIZE).AddInt64(32).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_SHA256).AddData(h).AddOp(txscript.OP_EQUALVERIFY).AddData(claim).AddOp(txscript.OP_CHECKSIG).AddOp(txscript.OP_ELSE).AddInt64(int64(c.RefundHeight)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP).AddData(refund).AddOp(txscript.OP_CHECKSIG).AddOp(txscript.OP_ENDIF).Script()
}
func (c HTLC) PkScript() ([]byte, error) {
	s, e := c.Script()
	if e != nil {
		return nil, e
	}
	hash := sha256.Sum256(s)
	return txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(hash[:]).Script()
}
func (c HTLC) Address() (string, error) {
	s, e := c.Script()
	if e != nil {
		return "", e
	}
	hash := sha256.Sum256(s)
	a, e := btcutil.NewAddressWitnessScriptHash(hash[:], &chaincfg.RegressionNetParams)
	if e != nil {
		return "", e
	}
	return a.EncodeAddress(), nil
}
func Parse(raw string) (*wire.MsgTx, error) {
	b, e := hex.DecodeString(raw)
	if e != nil {
		return nil, e
	}
	tx := wire.NewMsgTx(2)
	r := bytes.NewReader(b)
	if e = tx.Deserialize(r); e != nil {
		return nil, e
	}
	if r.Len() != 0 {
		return nil, errors.New("trailing transaction bytes")
	}
	return tx, nil
}
func Hex(tx *wire.MsgTx) string {
	var b bytes.Buffer
	_ = tx.Serialize(&b)
	return hex.EncodeToString(b.Bytes())
}
func Outpoint(id string, n uint32) (wire.OutPoint, error) {
	h, e := chainhash.NewHashFromStr(id)
	if e != nil {
		return wire.OutPoint{}, e
	}
	return wire.OutPoint{Hash: *h, Index: n}, nil
}
func write(b *bytes.Buffer, v any)       { _ = binary.Write(b, binary.LittleEndian, v) }
func varbytes(b *bytes.Buffer, v []byte) { _ = wire.WriteVarBytes(b, 0, v) }

// UnifiedDigest implements only ALL, segwit v0: every input/output/sequence and
// the five-byte zero-extended locktime is committed. Other modes fail closed.
func UnifiedDigest(tx *wire.MsgTx, index int, scriptCode []byte, spent []*wire.TxOut) ([]byte, error) {
	if index < 0 || index >= len(tx.TxIn) || len(spent) != len(tx.TxIn) {
		return nil, errors.New("missing prevouts")
	}
	var prev, amounts, scripts, sequences, outputs bytes.Buffer
	for i, in := range tx.TxIn {
		if spent[i] == nil || spent[i].Value < 0 || spent[i].Value > MaxMoney {
			return nil, errors.New("invalid prevout")
		}
		prev.Write(in.PreviousOutPoint.Hash[:])
		write(&prev, in.PreviousOutPoint.Index)
		write(&amounts, spent[i].Value)
		varbytes(&scripts, spent[i].PkScript)
		write(&sequences, in.Sequence)
	}
	for _, out := range tx.TxOut {
		if out.Value < 0 || out.Value > MaxMoney {
			return nil, errors.New("invalid output")
		}
		_ = wire.WriteTxOut(&outputs, 0, tx.Version, out)
	}
	var m bytes.Buffer
	m.WriteByte(0)
	m.WriteByte(byte(UnifiedAll))
	write(&m, tx.Version)
	write(&m, tx.LockTime)
	m.WriteByte(0)
	for _, v := range []*bytes.Buffer{&prev, &amounts, &scripts, &sequences, &outputs} {
		h := sha256.Sum256(v.Bytes())
		m.Write(h[:])
	}
	m.WriteByte(1)
	write(&m, uint32(index))
	varbytes(&m, scriptCode)
	tag := sha256.Sum256([]byte("UnifiedSighash"))
	h := sha256.New()
	h.Write(tag[:])
	h.Write(tag[:])
	h.Write(m.Bytes())
	return h.Sum(nil), nil
}
func Digest(id chain.ID, tx *wire.MsgTx, index int, script []byte, spent []*wire.TxOut) ([]byte, error) {
	if !id.Valid() {
		return nil, errors.New("unknown chain")
	}
	if id == chain.Blake {
		return UnifiedDigest(tx, index, script, spent)
	}
	if index < 0 || index >= len(tx.TxIn) || len(spent) != len(tx.TxIn) {
		return nil, errors.New("missing prevouts")
	}
	fetch := txscript.NewMultiPrevOutFetcher(nil)
	for i, in := range tx.TxIn {
		if spent[i] == nil {
			return nil, errors.New("missing prevout")
		}
		fetch.AddPrevOut(in.PreviousOutPoint, spent[i])
	}
	hashes := txscript.NewTxSigHashes(tx, fetch)
	return txscript.CalcWitnessSigHash(script, hashes, txscript.SigHashAll, tx, index, spent[index].Value)
}
func sign(id chain.ID, tx *wire.MsgTx, index int, script []byte, spent []*wire.TxOut, key *btcec.PrivateKey) ([]byte, error) {
	h, e := Digest(id, tx, index, script, spent)
	if e != nil {
		return nil, e
	}
	typ := byte(txscript.SigHashAll)
	if id == chain.Blake {
		typ = byte(UnifiedAll)
	}
	return append(ecdsa.Sign(key, h).Serialize(), typ), nil
}

// Fund consumes confirmed P2WPKH inputs to one HTLC and optional change.
func Fund(c HTLC, coins []chain.UTXO, key *btcec.PrivateKey, fee int64) (*wire.MsgTx, error) {
	pub := key.PubKey().SerializeCompressed()
	own, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(btcutil.Hash160(pub)).Script()
	if err != nil {
		return nil, err
	}
	return FundWithKeys(c, coins, map[string]*btcec.PrivateKey{hex.EncodeToString(own): key}, own, fee)
}

// FundWithKeys signs each historical receive input with its own key and directs
// change to the current receive script. Unknown scripts fail closed.
func FundWithKeys(c HTLC, coins []chain.UTXO, keys map[string]*btcec.PrivateKey, changeScript []byte, fee int64) (*wire.MsgTx, error) {
	script, e := c.PkScript()
	if e != nil {
		return nil, e
	}
	return PayWithKeys(c.Chain, c.Amount, script, coins, keys, changeScript, fee)
}

// PayWithKeys spends explicitly selected wallet inputs to one destination and
// optional change. All inputs are signed locally with the chain's hash type.
func PayWithKeys(id chain.ID, amount int64, script []byte, coins []chain.UTXO, keys map[string]*btcec.PrivateKey, changeScript []byte, fee int64) (*wire.MsgTx, error) {
	if !id.Valid() || amount < Dust || amount > MaxMoney || len(script) == 0 {
		return nil, errors.New("invalid payment")
	}
	if fee < 1 || fee > 1000000 || len(coins) < 1 || len(coins) > 50 {
		return nil, errors.New("invalid funding policy")
	}
	if len(changeScript) != 22 || changeScript[0] != 0 || changeScript[1] != 20 {
		return nil, errors.New("invalid change script")
	}
	tx := wire.NewMsgTx(2)
	spent := []*wire.TxOut{}
	var total int64
	seen := map[wire.OutPoint]bool{}
	for _, coin := range coins {
		op, e := Outpoint(coin.TxID, coin.Vout)
		if e != nil {
			return nil, e
		}
		s, e := hex.DecodeString(coin.Script)
		key := keys[coin.Script]
		if key == nil {
			return nil, errors.New("unknown funding key")
		}
		own, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(btcutil.Hash160(key.PubKey().SerializeCompressed())).Script()
		if err != nil {
			return nil, err
		}
		if e != nil || !bytes.Equal(s, own) || coin.Amount <= 0 || coin.Confirmations < 1 || seen[op] {
			return nil, errors.New("invalid funding UTXO")
		}
		seen[op] = true
		total += int64(coin.Amount)
		if total > MaxMoney {
			return nil, errors.New("input overflow")
		}
		in := wire.NewTxIn(&op, nil, nil)
		in.Sequence = wire.MaxTxInSequenceNum - 2
		tx.AddTxIn(in)
		spent = append(spent, wire.NewTxOut(int64(coin.Amount), s))
	}
	change := total - amount - fee
	if change < 0 {
		return nil, errors.New("insufficient confirmed funds")
	}
	if change > 0 && change < Dust {
		return nil, errors.New("change would be dust; select another coin")
	}
	tx.AddTxOut(wire.NewTxOut(amount, script))
	if change > 0 {
		tx.AddTxOut(wire.NewTxOut(change, changeScript))
	}
	for i := range tx.TxIn {
		key := keys[coins[i].Script]
		pub := key.PubKey().SerializeCompressed()
		code, err := txscript.NewScriptBuilder().AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).AddData(btcutil.Hash160(pub)).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG).Script()
		if err != nil {
			return nil, err
		}
		sig, e := sign(id, tx, i, code, spent, key)
		if e != nil {
			return nil, e
		}
		tx.TxIn[i].Witness = wire.TxWitness{sig, pub}
	}
	return tx, nil
}

// Spend signs the fixed template. A nil preimage deliberately leaves a claim
// incomplete for a watchtower. LockTime alone prevents its early confirmation.
func Spend(c HTLC, key *btcec.PrivateKey, recipient []byte, fee int64, refund bool, lock uint32, towerScript []byte, bounty int64, secret []byte) (*wire.MsgTx, error) {
	script, e := c.Script()
	if e != nil {
		return nil, e
	}
	pk, e := c.PkScript()
	if e != nil {
		return nil, e
	}
	op, e := Outpoint(c.TxID, c.Vout)
	if e != nil {
		return nil, e
	}
	expected := c.ClaimKey
	if refund {
		expected = c.RefundKey
		if lock < c.RefundHeight || (lock < 500000000) != (c.RefundHeight < 500000000) {
			return nil, errors.New("premature refund locktime")
		}
	}
	if hex.EncodeToString(key.PubKey().SerializeCompressed()) != expected {
		return nil, errors.New("wrong signing key")
	}
	if len(recipient) != 22 || recipient[0] != 0 || recipient[1] != 20 || fee < 1 || fee > 1000000 || bounty < 0 || bounty > c.Amount || c.Amount-fee-bounty < Dust {
		return nil, errors.New("invalid payout")
	}
	if bounty > 0 && (bounty < Dust || len(towerScript) != 22 || towerScript[0] != 0 || towerScript[1] != 20) {
		return nil, errors.New("invalid bounty")
	}
	if lock > 4000000000 {
		return nil, errors.New("locktime out of supported range")
	}
	if len(secret) != 0 {
		h := sha256.Sum256(secret)
		if len(secret) != 32 || hex.EncodeToString(h[:]) != c.Hash {
			return nil, errors.New("wrong preimage")
		}
	}
	tx := wire.NewMsgTx(2)
	tx.LockTime = lock
	in := wire.NewTxIn(&op, nil, nil)
	in.Sequence = wire.MaxTxInSequenceNum - 2
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(c.Amount-fee-bounty, recipient))
	if bounty > 0 {
		tx.AddTxOut(wire.NewTxOut(bounty, towerScript))
	}
	sig, e := sign(c.Chain, tx, 0, script, []*wire.TxOut{wire.NewTxOut(c.Amount, pk)}, key)
	if e != nil {
		return nil, e
	}
	if refund {
		in.Witness = wire.TxWitness{sig, nil, script}
	} else {
		in.Witness = wire.TxWitness{sig, secret, {1}, script}
	}
	return tx, nil
}
func FillSecret(c HTLC, tx *wire.MsgTx, secret []byte) error {
	h := sha256.Sum256(secret)
	if len(secret) != 32 || hex.EncodeToString(h[:]) != c.Hash {
		return errors.New("wrong preimage")
	}
	if len(tx.TxIn) != 1 || len(tx.TxIn[0].Witness) != 4 {
		return errors.New("not a claim template")
	}
	tx.TxIn[0].Witness[1] = bytes.Clone(secret)
	return nil
}
func VerifySignature(c HTLC, tx *wire.MsgTx, refund bool) error {
	if len(tx.TxIn) != 1 {
		return errors.New("expected exactly one input")
	}
	script, e := c.Script()
	if e != nil {
		return e
	}
	pk, e := c.PkScript()
	if e != nil {
		return e
	}
	in := tx.TxIn[0]
	op, e := Outpoint(c.TxID, c.Vout)
	if e != nil || in.PreviousOutPoint != op || in.Sequence != wire.MaxTxInSequenceNum-2 {
		return errors.New("wrong outpoint/sequence")
	}
	w := in.Witness
	expected := c.ClaimKey
	if refund {
		expected = c.RefundKey
		if len(w) != 3 || len(w[1]) != 0 || tx.LockTime < c.RefundHeight || (tx.LockTime < 500000000) != (c.RefundHeight < 500000000) {
			return errors.New("invalid refund")
		}
	} else if len(w) != 4 || !bytes.Equal(w[2], []byte{1}) {
		return errors.New("invalid claim")
	}
	if !bytes.Equal(w[len(w)-1], script) || len(w[0]) < 2 {
		return errors.New("wrong witness script")
	}
	typ := byte(1)
	if c.Chain == chain.Blake {
		typ = 0x21
	}
	if w[0][len(w[0])-1] != typ {
		return errors.New("unsafe signature hash type")
	}
	sig, e := ecdsa.ParseDERSignature(w[0][:len(w[0])-1])
	if e != nil {
		return e
	}
	keybytes, _ := hex.DecodeString(expected)
	pub, e := btcec.ParsePubKey(keybytes)
	if e != nil {
		return e
	}
	digest, e := Digest(c.Chain, tx, 0, script, []*wire.TxOut{wire.NewTxOut(c.Amount, pk)})
	if e != nil {
		return e
	}
	if !sig.Verify(digest, pub) {
		return errors.New("invalid signature")
	}
	return nil
}
func ExtractSecret(c HTLC, tx *wire.MsgTx) ([]byte, bool) {
	op, e := Outpoint(c.TxID, c.Vout)
	if e != nil {
		return nil, false
	}
	script, e := c.Script()
	if e != nil {
		return nil, false
	}
	for _, in := range tx.TxIn {
		w := in.Witness
		if in.PreviousOutPoint != op || len(w) != 4 || len(w[1]) != 32 || !bytes.Equal(w[2], []byte{1}) || !bytes.Equal(w[3], script) {
			continue
		}
		h := sha256.Sum256(w[1])
		if fmt.Sprintf("%x", h) == c.Hash {
			return bytes.Clone(w[1]), true
		}
	}
	return nil, false
}
