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
	broadcast   func(string) (string, error)
	transaction func(context.Context, string) (chain.Transaction, error)
	spent       bool
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
func (b *sendBackend) Transaction(ctx context.Context, id string) (chain.Transaction, error) {
	if b.transaction != nil {
		return b.transaction(ctx, id)
	}
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
		before := e.Status().Funds[id]
		preflight, err := e.preflightFunds(h.ctx, fundsRequest(id, selected))
		if err != nil || !preflight.Sufficient || (id == chain.BTC && preflight.State != "proven") {
			t.Fatal("real preflight", preflight, err)
		}
		offer := h.command("maker", "offer.create", map[string]any{"sell": id, "sell_amount": 1000000, "buy_amount": 2000000}).(protocol.Offer)
		held := e.Status().Funds[id]
		if held.TotalConfirmed != before.TotalConfirmed || held.UnlockedConfirmed >= before.UnlockedConfirmed || held.ReservedConfirmed == 0 {
			t.Fatal("offer partition", before, held)
		}
		preflight, err = e.preflightFunds(h.ctx, fundsRequest(id, selected))
		if err != nil || preflight.Sufficient {
			t.Fatal("held real input passed preflight", preflight, err)
		}
		raw, _ := json.Marshal(p)
		if _, err := e.Command(h.ctx, Request{Method: "wallet.send", Params: raw}); err == nil {
			t.Fatal("spent order-locked funds")
		}
		h.command("maker", "offer.cancel", map[string]string{"id": offer.ID})
		if released := e.Status().Funds[id]; released.UnlockedConfirmed != before.UnlockedConfirmed || released.ReservedConfirmed != 0 {
			t.Fatal("cancel partition", released)
		}
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

func TestPendingSendsRotateAfterTimeout(t *testing.T) {
	e, b, _ := sendFixture(t)
	e.s.Sends = map[string]*WalletSend{}
	for _, id := range []string{"a", "b", "c"} {
		e.s.Sends[id] = &WalletSend{PublicSend: PublicSend{ID: id, Chain: chain.Blake, TxID: id}}
	}
	var observed []string
	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		b.transaction = func(ctx context.Context, id string) (chain.Transaction, error) {
			observed = append(observed, id)
			cancel() // This lookup exhausts the cycle's entire budget.
			return chain.Transaction{}, ctx.Err()
		}
		e.advanceSends(ctx)
		cancel()
	}
	if strings.Join(observed, ",") != "a,b,c,a,b,c" {
		t.Fatal("slow early send starved later sends", observed)
	}
}

func TestPendingTakeExpiryUnlocksCoinsAndRefusesLateAcceptance(t *testing.T) {
	e, _, _ := sendFixture(t)
	maker, _, _ := sendFixture(t)
	offer := protocol.Offer{ID: transport.RandomID(), Maker: maker.identity.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 200000, Expires: time.Now().Unix() + 3600, Status: "open"}
	if err := maker.publishOffer(offer); err != nil {
		t.Fatal(err)
	}
	event := maker.s.Offers[offer.ID]
	e.s.Book[offer.Maker+":"+offer.ID] = event
	raw, _ := json.Marshal(map[string]string{"maker": offer.Maker, "id": offer.ID})
	result, err := e.Command(context.Background(), Request{Method: "swap.take", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]string)["id"]
	swap := e.s.Swaps[id]
	makerKeys, err := maker.swapKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	terms, err := protocol.NewTerms(swap.Request, makerKeys, e.heights)
	if err != nil {
		t.Fatal(err)
	}
	if e.expirePendingRequest(swap, offer.Expires-1) || !e.publicCoins()[0].Reserved {
		t.Fatal("released request before deadline")
	}
	if !e.expirePendingRequest(swap, offer.Expires) || e.publicCoins()[0].Reserved {
		t.Fatal("expired request kept coins locked")
	}
	for _, delivery := range e.s.Outbox {
		if delivery.Type == "request" {
			t.Fatal("expired request still retries")
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vault.Load(&e.s); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(terms)
	if err := e.handle(offer.Maker, transport.Message{Type: "accepted", SwapID: id, Body: body}); err != nil {
		t.Fatal(err)
	}
	e.reconcileReservations()
	if e.s.Swaps[id].Terms != nil || e.s.Swaps[id].Stage != "expired before acceptance" || e.publicCoins()[0].Reserved {
		t.Fatal("late acceptance revived released funds")
	}
	if err := e.CanChangeNetwork(); err != nil {
		t.Fatal("expired request blocks network change", err)
	}
}

func TestMakerReservationExpiresWhenTakerNeverFunds(t *testing.T) {
	e, _, _ := sendFixture(t)
	raw, _ := json.Marshal(map[string]any{"sell": "blake", "sell_amount": 100000, "buy_amount": 200000})
	result, err := e.Command(context.Background(), Request{Method: "offer.create", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	offer := result.(protocol.Offer)
	taker := nostr.Generate()
	id := transport.RandomID()
	keys, _ := e.swapKeys(id)
	request := protocol.Request{ID: id, OfferEvent: e.s.Offers[offer.ID], Taker: taker.Public().Hex(), Hash: strings.Repeat("12", 32), Keys: keys}
	raw, _ = json.Marshal(request)
	if err := e.handle(taker.Public().Hex(), transport.Message{Type: "request", SwapID: id, Body: raw}); err != nil {
		t.Fatal(err)
	}
	swap := e.s.Swaps[id]
	if !e.publicCoins()[0].Reserved {
		t.Fatal("accepted maker funds unlocked")
	}
	for _, c := range []chain.ID{chain.BTC, chain.Blake} {
		e.heights[c] = swap.Long.RefundHeight + 100
		e.clocks[c] = e.heights[c]
	}
	if err := e.advanceSwap(context.Background(), swap, nil); err == nil {
		t.Fatal("expired funding gate not reached")
	}
	e.reconcileReservations()
	if swap.Stage != "expired before maker funding" || e.publicCoins()[0].Reserved || swap.ShortFunding != "" {
		t.Fatal("abandoned maker reservation kept funds locked")
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vault.Load(&e.s); err != nil {
		t.Fatal(err)
	}
	swap = e.s.Swaps[id]
	for _, c := range []chain.ID{chain.BTC, chain.Blake} {
		e.heights[c] = 200
		e.clocks[c] = 200
	}
	if err := e.advanceSwap(context.Background(), swap, nil); err != nil {
		t.Fatal(err)
	}
	if swap.Stage != "expired before maker funding" || swap.ShortFunding != "" {
		t.Fatal("expired maker revived after clock rollback")
	}
	if err := e.CanChangeNetwork(); err != nil {
		t.Fatal(err)
	}
}
