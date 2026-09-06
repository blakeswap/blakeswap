package contract

import (
	"errors"
	"github.com/btcsuite/btcd/wire"
)

// VirtualSize rounds weight upward, including SegWit marker/flag and witness.
func VirtualSize(tx *wire.MsgTx) int64 {
	return int64((tx.SerializeSizeStripped()*3 + tx.SerializeSize() + 3) / 4)
}

// PaymentVSize bounds native P2WPKH input signatures by 73 bytes, including the
// hash type. Output scripts and CompactSize lengths are accounted for exactly.
func PaymentVSize(inputs int, scripts ...[]byte) (int64, error) {
	if inputs < 1 || inputs > 50 || len(scripts) < 1 || len(scripts) > 2 {
		return 0, errors.New("unsupported payment shape")
	}
	tx := wire.NewMsgTx(2)
	for i := 0; i < inputs; i++ {
		in := wire.NewTxIn(&wire.OutPoint{}, nil, wire.TxWitness{make([]byte, 73), make([]byte, 33)})
		tx.AddTxIn(in)
	}
	for _, script := range scripts {
		tx.AddTxOut(wire.NewTxOut(0, script))
	}
	return VirtualSize(tx), nil
}

func FeeForVSize(rate, vsize int64) (int64, error) {
	if rate < 1 || rate > 1000000000 || vsize < 1 || vsize > 100000 {
		return 0, errors.New("invalid fee rate or virtual size")
	}
	return (rate*vsize + 999) / 1000, nil
}
