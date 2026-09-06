package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

type sendBackend struct {
	*receiveBackend
	broadcast func(string) (string, error)
	spent     bool
}

func (b *sendBackend) Output(_ context.Context, id string, vout uint32) (*chain.TxOut, error) {
	if b.spent {
		return nil, nil
	}
	for _, coin := range b.coins {
		if coin.TxID == id && coin.Vout == vout {
			out := &chain.TxOut{Value: coin.Amount, Confirmations: coin.Confirmations}
			out.Script.Hex = coin.Script
			return out, nil
		}
	}
	return nil, nil
}
func (b *sendBackend) Transaction(context.Context, string) (chain.Transaction, error) {
	return chain.Transaction{}, &chain.RPCError{Code: -5, Message: "transaction not found"}
}
func (b *sendBackend) Broadcast(_ context.Context, raw string) (string, error) {
	return b.broadcast(raw)
}

func sendFixture(t *testing.T) (*Engine, *sendBackend, SendRequest) {
	e, backends := receiveEngine(t)
	e.Config.Mode = "trader"
	e.s.Offers = map[string]nostr.Event{}
	e.s.Book = map[string]nostr.Event{}
	e.s.Swaps = map[string]*Swap{}
	e.s.Outbox = map[string]*Delivery{}
	e.s.Seen = map[string]string{}
	id := chain.Blake
	b := &sendBackend{receiveBackend: backends[id]}
	b.coins = []chain.UTXO{{TxID: strings.Repeat("12", 32), Vout: 0, Amount: 1000000, Script: hex.EncodeToString(e.scripts[id]), Confirmations: 2}}
	e.nodes[id] = b
	request := SendRequest{ID: transport.RandomID(), Chain: id, Destination: e.addresses[chain.BTC], Amount: 900000, Fee: 1500, Inputs: []CoinOutpoint{{b.coins[0].TxID, 0}}, ExpectedNetwork: "regtest"}
	return e, b, request
}
func TestSendPersistsBeforeBroadcastAndRetriesIdenticalTransaction(t *testing.T) {
	e, b, p := sendFixture(t)
	var original string
	broadcasts := 0
	b.broadcast = func(raw string) (string, error) {
		var saved State
		if _, err := e.vault.Load(&saved); err != nil {
			t.Fatal(err)
		}
		if saved.Sends[p.ID] == nil || saved.Sends[p.ID].Raw != raw {
			t.Fatal("broadcast before durable send")
		}
		broadcasts++
		original = raw
		return "", context.DeadlineExceeded
	}
	raw, _ := json.Marshal(p)
	result, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	sent := result.(PublicSend)
	if sent.Submitted || sent.Error == "" || broadcasts != 1 {
		t.Fatal("ambiguous broadcast not retained", sent)
	}
	if !e.publicCoins()[0].Reserved {
		t.Fatal("pending send released its inputs")
	}
	if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw}); err != nil || broadcasts != 1 {
		t.Fatal("non-idempotent send", err)
	}
	p.Amount--
	raw, _ = json.Marshal(p)
	if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw}); err == nil {
		t.Fatal("request ID changed details")
	}
	b.broadcast = func(raw string) (string, error) {
		if raw != original {
			t.Fatal("retry changed transaction")
		}
		tx, _ := contract.Parse(raw)
		return tx.TxHash().String(), nil
	}
	e.s.Sends[p.ID].LastAttempt = 0
	e.advanceSends(context.Background())
	if !e.s.Sends[p.ID].Submitted {
		t.Fatal("did not retry saved transaction")
	}
}
func TestSendRejectsLockedSpentWrongNetworkAndInvalidFee(t *testing.T) {
	for _, scenario := range []string{"locked", "spent", "network", "fee", "duplicate", "wrong-address-network"} {
		t.Run(scenario, func(t *testing.T) {
			e, b, p := sendFixture(t)
			b.broadcast = func(string) (string, error) { t.Fatal("invalid send broadcast"); return "", nil }
			switch scenario {
			case "locked":
				if err := e.refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				o := protocol.Offer{ID: transport.RandomID(), Maker: e.identity.Public().Hex(), Sell: chain.Blake, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 3600, Status: "open", Network: chain.Regtest}
				if err := e.publishOffer(o); err != nil {
					t.Fatal(err)
				}
			case "spent":
				b.spent = true
			case "network":
				p.ExpectedNetwork = "mainnet"
			case "fee":
				p.Fee = -1
			case "duplicate":
				p.Inputs = append(p.Inputs, p.Inputs[0])
			case "wrong-address-network":
				p.Destination = "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
			}
			raw, _ := json.Marshal(p)
			if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw}); err == nil {
				t.Fatal("invalid send accepted")
			}
			if len(e.s.Sends) != 0 {
				t.Fatal("invalid request reserved coins")
			}
		})
	}
}
func TestOpenOrderLocksPersistUntilCancellation(t *testing.T) {
	e, b, p := sendFixture(t)
	b.broadcast = func(raw string) (string, error) {
		tx, err := contract.Parse(raw)
		if err != nil {
			return "", err
		}
		return tx.TxHash().String(), nil
	}
	offerRaw, _ := json.Marshal(map[string]any{"sell": "blake", "sell_amount": 100000, "buy_amount": 200000})
	result, err := e.Command(context.Background(), Request{Method: "offer.create", Params: offerRaw})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	coins := e.publicCoins()
	if len(coins) != 1 || !coins[0].Reserved {
		t.Fatal("open order did not lock selected UTXO")
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s = saved
	e.reconcileReservations()
	raw, _ := json.Marshal(p)
	if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw}); err == nil {
		t.Fatal("restart lost open-order lock")
	}
	cancel, _ := json.Marshal(map[string]string{"id": offer.ID})
	if _, err := e.Command(context.Background(), Request{Method: "offer.cancel", Params: cancel}); err != nil {
		t.Fatal(err)
	}
	if e.publicCoins()[0].Reserved {
		t.Fatal("cancellation did not release coins")
	}
	if _, err := e.Command(context.Background(), Request{Method: "wallet.send", Params: raw}); err != nil {
		t.Fatal("cancelled coins not sendable", err)
	}
}
func TestRealSendsHonorCoinControlFeesAndOrderLocks(t *testing.T) {
	h := newHarness(t, 0)
	e := h.engines["maker"]
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		var selected []CoinOutpoint
		var total int64
		for _, coin := range e.publicCoins() {
			if coin.Chain == id && !coin.Reserved {
				selected = append(selected, CoinOutpoint{coin.TxID, coin.Vout})
				total += coin.Amount
			}
		}
		p := SendRequest{ID: transport.RandomID(), Chain: id, Destination: h.engines["taker"].addresses[id], Amount: 1000000, Fee: 3500, Inputs: selected, ExpectedNetwork: "regtest"}
		offer := h.command("maker", "offer.create", map[string]any{"sell": id, "sell_amount": 1000000, "buy_amount": 2000000}).(protocol.Offer)
		raw, _ := json.Marshal(p)
		if _, err := e.Command(h.ctx, Request{Method: "wallet.send", Params: raw}); err == nil {
			t.Fatal("spent order-locked funds")
		}
		h.command("maker", "offer.cancel", map[string]string{"id": offer.ID})
		result, err := e.Command(h.ctx, Request{Method: "wallet.send", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		sent := result.(PublicSend)
		if !sent.Submitted {
			t.Fatal("real send not broadcast", sent.Error)
		}
		tx, err := contract.Parse(e.s.Sends[p.ID].Raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(tx.TxIn) != len(selected) || tx.TxOut[0].Value != p.Amount || tx.TxOut[1].Value != total-p.Amount-p.Fee {
			t.Fatal("manual amount/fee/coin control not honored")
		}
		h.mine(id, 2)
		e.advanceSends(h.ctx)
		if e.s.Sends[p.ID].Confirmations < 2 {
			t.Fatal("send not confirmed")
		}
	}
}
