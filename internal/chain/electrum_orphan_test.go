package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

type orphanObservationBackend struct {
	*failoverBackend
	electrum *Electrum
}

func (b *orphanObservationBackend) Transaction(ctx context.Context, id string) (Transaction, error) {
	return b.electrum.Transaction(ctx, id)
}

func orphanObservationFixture(t *testing.T) (string, string) {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{Sequence: 0xfffffffd, Witness: wire.TxWitness{[]byte("retained witness")}})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		t.Fatal(err)
	}
	return tx.TxHash().String(), hex.EncodeToString(encoded.Bytes())
}

func TestElectrumOrphanKnowledgeKeepsEndpointHealthyAndRemainsUnknown(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		t.Run(string(id), func(t *testing.T) {
			txid, raw := orphanObservationFixture(t)
			el := electrumFixture(t, Regtest, id, func(method string, _ []json.RawMessage) any {
				switch method {
				case "blockchain.transaction.get":
					return raw
				case "blockchain.scripthash.get_history":
					return []any{}
				default:
					t.Errorf("unexpected method %s", method)
					return nil
				}
			})
			b := &orphanObservationBackend{failoverBackend: &failoverBackend{height: 100, hash: "current"}, electrum: el}
			pool := testPool(b)
			defer pool.Close()
			if _, err := pool.Height(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := pool.Generation()
			observed, err := pool.Transaction(context.Background(), txid)
			if !errors.Is(err, ErrTransactionUnobserved) || TransactionNotFound(err) || observed.Hex != raw || observed.Height != 0 || observed.BlockHash != "" || observed.Confirmations != 0 {
				t.Fatalf("unknown raw knowledge became absence/publication proof: record=%+v err=%v", observed, err)
			}
			if _, err := pool.Height(context.Background()); err != nil {
				t.Fatal("ordinary unknown inclusion poisoned a healthy source", err)
			}
			if _, err := pool.Broadcast(context.Background(), raw); err != nil {
				t.Fatal("exact previously authorized retry was blocked", err)
			}
			if pool.Generation() != before || b.broadcasts != 1 || b.raw != raw || pool.Status().Endpoints[0].Error != "" {
				t.Fatal("source or retry changed", pool.Status(), b.broadcasts)
			}
		})
	}
}

func TestElectrumUnknownInclusionRejectsMalformedHistoryAndRetainsRawHook(t *testing.T) {
	txid, raw := orphanObservationFixture(t)
	other := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name       string
		history    any
		unknown    bool
		mempool    bool
		invalidRaw bool
		wrongTxID  bool
	}{
		{name: "empty", history: []historyItem{}, unknown: true},
		{name: "other-valid", history: []historyItem{{TxID: other, Height: 1}}, unknown: true},
		{name: "mempool", history: []historyItem{{TxID: txid, Height: 0}}, mempool: true},
		{name: "mempool-parent", history: []historyItem{{TxID: txid, Height: -1}}, mempool: true},
		{name: "same-duplicate", history: []historyItem{{TxID: txid, Height: 0}, {TxID: strings.ToUpper(txid), Height: 0}}, mempool: true},
		{name: "malformed-id", history: []historyItem{{TxID: "not-an-id", Height: 1}}},
		{name: "malformed-height", history: []historyItem{{TxID: other, Height: -2}}},
		{name: "overflow-height", history: []historyItem{{TxID: other, Height: 1 << 32}}},
		{name: "conflicting-heights", history: []historyItem{{TxID: other, Height: 1}, {TxID: other, Height: 2}}},
		{name: "malformed-response", history: "not-an-array"},
		{name: "null-response", history: nil},
		{name: "missing-height", history: []map[string]any{{"tx_hash": other}}},
		{name: "null-entry", history: []any{nil}},
		{name: "invalid-merkle-proof", history: []historyItem{{TxID: txid, Height: 1}}},
		{name: "invalid-raw", history: []historyItem{}, invalidRaw: true},
		{name: "wrong-raw-id", history: []historyItem{}, wrongTxID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			el := electrumFixture(t, Regtest, BTC, func(method string, _ []json.RawMessage) any {
				switch method {
				case "blockchain.transaction.get":
					if tc.invalidRaw {
						return "00"
					}
					return raw
				case "blockchain.scripthash.get_history":
					return tc.history
				case "blockchain.transaction.get_merkle":
					return map[string]any{"block_height": 2}
				default:
					t.Errorf("unexpected method %s", method)
					return nil
				}
			})
			lookup := txid
			if tc.wrongTxID {
				lookup = other
			}
			hook := false
			_, err := el.transaction(context.Background(), lookup, func(tx Transaction) error {
				hook = true
				if tx.Hex != raw {
					t.Fatal("witness bytes changed before metadata")
				}
				return nil
			})
			if hook != (!tc.invalidRaw && !tc.wrongTxID) {
				t.Fatal("raw hook was skipped after valid bytes or received unverified bytes")
			}
			if errors.Is(err, ErrTransactionUnobserved) != tc.unknown || (err == nil) != tc.mempool || TransactionNotFound(err) {
				t.Fatal("invalid or unobserved history was misclassified", err)
			}
		})
	}
}
