package api

import (
	"encoding/csv"
	"strings"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func (h *reviewedTradeFixture) activity(name string, query *pb.ActivityQuery) *pb.ActivityPage {
	h.t.Helper()
	query.ExpectedWallet = name
	query.ExpectedNetwork = "regtest"
	page, err := h.clients[name].ListActivity(h.contexts[name], query)
	if err != nil {
		h.t.Fatal(err)
	}
	return page
}

func TestRealActivityHistoryThroughTypedAPI(t *testing.T) {
	h := newReviewedTradeFixture(t)
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			status := h.status("maker")
			var coin *pb.WalletCoin
			for _, c := range status.Coins {
				if c.Chain == string(id) && !c.Reserved && c.Confirmations >= 2 {
					coin = c
					break
				}
			}
			if coin == nil {
				t.Fatal("missing fixture coin")
			}
			oldAddress := coin.Address
			request := &pb.SendCoinsRequest{Id: transport.RandomID(), Chain: string(id), Destination: h.status("taker").Addresses[string(id)], Amount: 500000, Fee: 6000, MaxFee: 20000, Inputs: []*pb.Outpoint{{Txid: coin.Txid, Vout: coin.Vout}}, ExpectedNetwork: "regtest"}
			sent, err := h.clients["maker"].SendCoins(h.contexts["maker"], request)
			if err != nil {
				t.Fatal(err)
			}
			bump, err := h.clients["maker"].BumpTransaction(h.contexts["maker"], &pb.BumpRequest{Id: request.Id, Kind: "send", Fee: 12000, ExpectedTxid: sent.Txid, ExpectedNetwork: "regtest"})
			if err != nil {
				t.Fatal(err)
			}
			h.mine(id, 2)
			var send *pb.ActivityRecord
			for i := 0; i < 15; i++ {
				h.tick()
				for _, a := range h.activity("maker", &pb.ActivityQuery{Chain: string(id)}).Records {
					if a.Id == "send/"+request.Id {
						send = a
					}
				}
				if send != nil && send.Status == "confirmed" {
					break
				}
			}
			if send == nil || send.Status != "confirmed" || send.Txid != bump.Txid || send.Fee != 12000 || len(send.Variants) != 2 {
				t.Fatal(send)
			}
			page := h.activity("maker", &pb.ActivityQuery{Chain: string(id)})
			foundSpent, foundChange := false, false
			for _, a := range page.Records {
				if a.Kind == "receive" && a.Txid == coin.Txid {
					foundSpent = true
					if a.CreatedAt != 0 || a.CreatedSource != "unknown" || !a.Movement {
						t.Fatal(a)
					}
				}
				if a.Kind == "receive" && a.Txid == bump.Txid {
					foundChange = true
					if a.Classification != "change" || a.Movement || a.GroupId != send.GroupId {
						t.Fatal(a)
					}
				}
			}
			if !foundSpent || !foundChange {
				t.Fatal("spent deposit/change missing", page)
			}
			frozen := h.activity("maker", &pb.ActivityQuery{Chain: string(id), Limit: 1})
			var late string
			if err := h.nodes[id].WithWallet("faucet").Call(h.ctx, "sendtoaddress", &late, oldAddress, chain.Coins(123456)); err != nil {
				t.Fatal(err)
			}
			h.mine(id, 2)
			// Advance the bounded address pass without creating competing API snapshots.
			for i := 0; i < 10; i++ {
				h.tick()
			}
			foundLate := false
			for _, a := range h.activity("maker", &pb.ActivityQuery{Chain: string(id)}).Records {
				if a.Txid == late && a.Kind == "receive" {
					foundLate = true
				}
			}
			if !foundLate {
				t.Fatal("retired receive address payment omitted")
			}
			next := h.activity("maker", &pb.ActivityQuery{Chain: string(id), Snapshot: frozen.Snapshot, Cursor: frozen.NextCursor, Limit: 500})
			if next.Total != frozen.Total {
				t.Fatal("arrival changed snapshot pagination")
			}
			for _, a := range next.Records {
				if a.Txid == late {
					t.Fatal("arrival entered old snapshot")
				}
			}
			h.restart("maker")
			h.tick()
			if after := h.activity("maker", &pb.ActivityQuery{Chain: string(id), Kind: "send"}); len(after.Records) != 1 || after.Records[0].Id != send.Id {
				t.Fatal("restart duplicated send lineage", after)
			}
			confirmed, err := h.nodes[id].Transaction(h.ctx, bump.Txid)
			if err != nil {
				t.Fatal(err)
			}
			if err = h.nodes[id].Call(h.ctx, "invalidateblock", nil, confirmed.BlockHash); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[id].Call(h.ctx, "reconsiderblock", nil, confirmed.BlockHash); err != nil {
					t.Error(err)
				}
			}()
			h.tick()
			demoted := h.activity("maker", &pb.ActivityQuery{Chain: string(id), Kind: "send"}).Records[0]
			if demoted.Status == "confirmed" || len(demoted.History) == 0 {
				t.Fatal("reorg did not demote activity", demoted)
			}
			if err = h.nodes[id].Call(h.ctx, "reconsiderblock", nil, confirmed.BlockHash); err != nil {
				t.Fatal(err)
			}
			h.tick()
			current := h.activity("maker", &pb.ActivityQuery{Chain: string(id)})
			exported, err := h.clients["maker"].ExportActivity(h.contexts["maker"], &pb.ActivityQuery{ExpectedWallet: "maker", ExpectedNetwork: "regtest", Chain: string(id), Snapshot: current.Snapshot, Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(strings.NewReader(exported.Csv)).ReadAll()
			if err != nil || len(rows) != len(current.Records)+1 {
				t.Fatal(rows, err)
			}
			if strings.Contains(exported.Csv, "http://") || strings.Contains(exported.Csv, "mnemonic") || strings.Contains(exported.Csv, "password") {
				t.Fatal("private data in export")
			}
			t.Logf("%s spent deposit %s, replacement lineage %s, retired-address receipt %s, reorg and CSV verified", id, coin.Txid, request.Id, late)
		})
	}
}
