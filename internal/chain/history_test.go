package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func historyReceipt(t *testing.T, n uint32) (string, Transaction) {
	t.Helper()
	address, err := btcutil.NewAddressWitnessPubKeyHash(make([]byte, 20), Regtest.Params())
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(address)
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(2)
	tx.LockTime = n
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(100001+int64(n), script))
	var raw strings.Builder
	if err := tx.Serialize(&raw); err != nil {
		t.Fatal(err)
	}
	return address.EncodeAddress(), Transaction{Hex: hex.EncodeToString([]byte(raw.String())), TxID: tx.TxHash().String(), Confirmations: 2, BlockHash: strings.Repeat("a", 64)}
}
func TestRPCHistoryIncludesSpentReceiptsAndPreservesBlockTime(t *testing.T) {
	address, a := historyReceipt(t, 1)
	_, b := historyReceipt(t, 2)
	txs := map[string]Transaction{a.TxID: a, b.TxID: b}
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("test:cookie"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "listreceivedbyaddress":
			result = []map[string]any{{"address": address, "txids": []string{b.TxID, a.TxID, a.TxID}}}
		case "getrawtransaction":
			var id string
			json.Unmarshal(req.Params[0], &id)
			result = txs[id]
		case "getblockheader":
			result = map[string]any{"height": 10, "time": 1234567890}
		default:
			t.Error("history must not depend on current UTXOs", req.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": 1})
	}))
	defer server.Close()
	rpc, err := New(BTC, server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	page, err := rpc.WithWallet("watch").AddressHistory(context.Background(), address, "", 1)
	if err != nil || len(page.Transactions) != 1 || page.Complete || page.Next == "" || page.Source != historySource("rpc", server.URL) {
		t.Fatal(page, err)
	}
	second, err := rpc.WithWallet("watch").AddressHistory(context.Background(), address, page.Next, 1)
	if err != nil || len(second.Transactions) != 1 || !second.Complete || second.Next != "" {
		t.Fatal(second, err)
	}
	got := []string{page.Transactions[0].TxID, second.Transactions[0].TxID}
	want := []string{a.TxID, b.TxID}
	sort.Strings(want)
	if got[0] != want[0] || got[1] != want[1] || page.Transactions[0].BlockTime != 1234567890 || page.Transactions[0].Height != 10 {
		t.Fatal(got, want, page)
	}
}
func TestHistoryPaginationRejectsUnboundedAndMalformedIDs(t *testing.T) {
	good := strings.Repeat("a", 64)
	for _, test := range []struct {
		ids   []string
		limit int
	}{{[]string{good}, 0}, {[]string{good}, 21}, {[]string{"bad"}, 1}, {make([]string, 10001), 1}} {
		if _, _, err := historyPageIDs(test.ids, "", test.limit); err == nil {
			t.Fatal("unsafe history page accepted")
		}
	}
}
