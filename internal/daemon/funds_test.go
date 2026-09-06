package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type fundsBackend struct {
	chain.Backend
	outputs map[string]*chain.TxOut
	err     error
	entered chan struct{}
	release chan struct{}
}

func (b *fundsBackend) Height(context.Context) (uint32, error) { return 200, b.err }
func (b *fundsBackend) Output(ctx context.Context, id string, vout uint32) (*chain.TxOut, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.outputs[chain.OutpointKey(id, vout)], b.err
}
func fundsRequest(id chain.ID, inputs []CoinOutpoint) Request {
	raw, _ := json.Marshal(map[string]any{"chain": id, "amount": 100000, "fee": 2000, "inputs": inputs, "expected_network": "regtest"})
	return Request{Method: "wallet.preflight", Params: raw}
}
func TestFundsPartitionAndReservationOwners(t *testing.T) {
	e, _ := receiveEngine(t)
	coin := chain.UTXO{TxID: "1111111111111111111111111111111111111111111111111111111111111111", Amount: 200000, Confirmations: 2, Script: hex.EncodeToString(e.scripts[chain.BTC])}
	e.walletCoins = map[chain.ID]map[string][]chain.UTXO{chain.BTC: {}, chain.Blake: {}}
	e.walletCoins[chain.BTC][coin.Script] = []chain.UTXO{coin, {TxID: "pending", Amount: 900, Script: coin.Script}}
	check := func(unlocked, reserved, pending int64) {
		t.Helper()
		b := e.chainBalances(e.publicCoins())[chain.BTC]
		if b.UnlockedConfirmed != unlocked || b.ReservedConfirmed != reserved || b.Unconfirmed != pending || b.TotalConfirmed != unlocked+reserved {
			t.Fatalf("partition: %+v", b)
		}
	}
	check(200000, 0, 900)
	offer := protocol.Offer{ID: "order", Maker: e.identity.Public().Hex(), Sell: chain.BTC, SellAmount: 100000, BuyAmount: 100000, Expires: time.Now().Unix() + 100, Status: "open"}
	raw, _ := json.Marshal(offer)
	e.s.Offers = map[string]nostr.Event{"order": {Content: string(raw)}}
	if err := e.reserveCoins("offer/order", chain.BTC, 102000); err != nil {
		t.Fatal(err)
	}
	check(0, 200000, 900)
	e.s.CoinReservations["swap/pending"] = CoinReservation{chain.BTC, []CoinOutpoint{{coin.TxID, 0}}}
	check(0, 200000, 900)
	holds := e.publicCoins()[0].Holds
	if len(holds) != 2 || !holds[0].Cancellable || holds[0].ID != "order" {
		t.Fatalf("owners: %+v", holds)
	}
	delete(e.s.CoinReservations, "offer/order")
	check(0, 200000, 900)
	delete(e.s.CoinReservations, "swap/pending")
	check(200000, 0, 900)
	// Signed funding retains a reservation even after the unsigned intention is removed.
	tx := wire.NewMsgTx(2)
	point, _ := chain.WireOutpoint(coin.TxID, 0)
	tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	tx.AddTxOut(wire.NewTxOut(100000, []byte{0x51}))
	e.s.Sends = map[string]*WalletSend{"send": {PublicSend: PublicSend{ID: "send", Chain: chain.BTC}, Raw: contract.Hex(tx)}}
	check(0, 200000, 900)
	e.walletCoins[chain.BTC][coin.Script] = []chain.UTXO{{TxID: "change", Amount: 98000, Script: coin.Script}}
	check(0, 0, 98000)
	e.walletCoins[chain.BTC][coin.Script][0].Confirmations = 2
	check(98000, 0, 0)
	// Reorg resurrects the original signed inputs: they remain held.
	e.walletCoins[chain.BTC][coin.Script] = []chain.UTXO{coin}
	check(0, 200000, 0)
}

func TestFundsPreflightFreshSelectionAndNetworkIsolation(t *testing.T) {
	e, b, p := sendFixture(t)
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := fundsRequest(chain.Blake, nil)
	got, err := e.preflightFunds(context.Background(), req)
	if err != nil || !got.Sufficient || got.State != "not_applicable" || len(got.Inputs) != 1 {
		t.Fatal(got, err)
	}
	if err := e.reserveCoins("offer/held", chain.Blake, 102000); err != nil {
		t.Fatal(err)
	}
	got, err = e.preflightFunds(context.Background(), req)
	if err != nil || got.Sufficient {
		t.Fatal("reserved input accepted", got, err)
	}
	delete(e.s.CoinReservations, "offer/held")
	b.spent = true
	got, err = e.preflightFunds(context.Background(), req)
	if err != nil || got.Sufficient {
		t.Fatal("spent input accepted", got, err)
	}
	b.spent = false
	got, err = e.preflightFunds(context.Background(), fundsRequest(chain.Blake, append(p.Inputs, p.Inputs...)))
	if err != nil || got.Sufficient {
		t.Fatal("duplicate accepted", got, err)
	}
	req.Params = []byte(`{"chain":"blake","amount":100000,"fee":2000,"expected_network":"mainnet"}`)
	if _, err = e.preflightFunds(context.Background(), req); err == nil {
		t.Fatal("wrong network accepted")
	}
	other, _, _ := sendFixture(t)
	got, err = other.preflightFunds(context.Background(), fundsRequest(chain.Blake, p.Inputs))
	if err != nil || got.Sufficient {
		t.Fatal("other wallet used snapshot", got, err)
	}
}

func TestFundsPreflightDoesNotHoldSettlementLockOrCacheBackendErrors(t *testing.T) {
	e, b, _ := sendFixture(t)
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	coin := b.coins[0]
	out := &chain.TxOut{Value: coin.Amount, Confirmations: 2}
	out.Script.Hex = coin.Script
	backend := &fundsBackend{outputs: map[string]*chain.TxOut{chain.OutpointKey(coin.TxID, 0): out}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e.nodes[chain.Blake] = backend
	done := make(chan FundsPreflight, 1)
	go func() {
		result, _ := e.preflightFunds(context.Background(), fundsRequest(chain.Blake, nil))
		done <- result
	}()
	<-backend.entered
	if !e.mu.TryLock() {
		t.Fatal("preflight holds settlement lock")
	}
	e.mu.Unlock()
	checking, err := e.preflightFunds(context.Background(), fundsRequest(chain.Blake, nil))
	if err != nil || checking.State != "checking" {
		t.Fatal(checking, err)
	}
	close(backend.release)
	if got := <-done; !got.Sufficient {
		t.Fatal(got)
	}
	backend.entered = nil
	backend.err = errors.New("offline")
	got, _ := e.preflightFunds(context.Background(), fundsRequest(chain.Blake, nil))
	if got.State != "unavailable" || got.Sufficient {
		t.Fatal(got)
	}
	backend.err = nil
	got, _ = e.preflightFunds(context.Background(), fundsRequest(chain.Blake, nil))
	if !got.Sufficient {
		t.Fatal("error cached", got)
	}
}

func TestHTLCPrincipalSeparateUnavailableAndReorg(t *testing.T) {
	e, _ := receiveEngine(t)
	c := contract.HTLC{Chain: chain.BTC, TxID: "funding", Amount: 100000}
	e.s.Swaps = map[string]*Swap{"swap": {ID: "swap", Role: "taker", Long: c}, "duplicate": {Role: "taker", Long: c}}
	backend := &fundsBackend{outputs: map[string]*chain.TxOut{"funding:0": {Value: 100000}}}
	e.nodes[chain.BTC] = backend
	e.refreshHTLCBalance(context.Background(), chain.BTC)
	b := e.chainBalances(nil)[chain.BTC]
	if !b.HTLCAvailable || b.HTLCLocked != 100000 || b.TotalConfirmed != 0 {
		t.Fatal(b)
	}
	backend.err = errors.New("offline")
	if err := e.refreshChain(context.Background(), chain.BTC); err == nil {
		t.Fatal("failed chain did not report error")
	}
	if e.chainBalances(nil)[chain.BTC].HTLCAvailable {
		t.Fatal("failed refresh retained available contract observation")
	}
	e.refreshHTLCBalance(context.Background(), chain.BTC)
	if e.chainBalances(nil)[chain.BTC].HTLCAvailable {
		t.Fatal("lookup failure inferred balance")
	}
	backend.err = nil
	delete(backend.outputs, "funding:0")
	e.refreshHTLCBalance(context.Background(), chain.BTC)
	if b = e.chainBalances(nil)[chain.BTC]; !b.HTLCAvailable || b.HTLCLocked != 0 {
		t.Fatal(b)
	}
	backend.outputs["funding:0"] = &chain.TxOut{Value: 100000}
	e.refreshHTLCBalance(context.Background(), chain.BTC)
	if e.chainBalances(nil)[chain.BTC].HTLCLocked != 100000 {
		t.Fatal("reorg principal not restored")
	}
}

func TestRealFundsContractPrincipalAndPendingChange(t *testing.T) {
	h := newHarness(t, 0)
	swapID := h.fundBoth(chain.BTC, 0)
	h.online("taker")
	for _, name := range []string{"maker", "taker"} {
		e := h.engines[name]
		// Observe funds only: advancing the protocol would reveal and claim.
		if err := e.refresh(h.ctx); err != nil {
			t.Fatal(err)
		}
		swap := e.s.Swaps[swapID]
		c := swap.Short
		if name == "taker" {
			c = swap.Long
		}
		b := e.Status().Funds[c.Chain]
		if !b.HTLCAvailable || b.HTLCLocked != c.Amount || b.ReservedConfirmed != 0 || b.TotalConfirmed != 100000000-c.Amount-protocol.FundingFee {
			t.Fatalf("%s funded categories: %+v", name, b)
		}
		t.Logf("%s own %s principal=%d confirmed deposit=%d", name, c.Chain, b.HTLCLocked, b.TotalConfirmed)
	}
}

type fundsProofBackend struct {
	*fundsBackend
	txs       map[string]chain.Transaction
	coinbases map[uint32]chain.Transaction
}

func (b *fundsProofBackend) Transaction(_ context.Context, id string) (chain.Transaction, error) {
	if tx, ok := b.txs[id]; ok {
		return tx, nil
	}
	return chain.Transaction{}, errors.New("indexer unavailable")
}
func (b *fundsProofBackend) Coinbase(_ context.Context, height uint32) (chain.Transaction, error) {
	if tx, ok := b.coinbases[height]; ok {
		return tx, nil
	}
	return chain.Transaction{}, errors.New("indexer unavailable")
}
func TestFundsBTCReadinessDistinguishesProofReorgAndUnavailable(t *testing.T) {
	e, _ := receiveEngine(t)
	e.Config.Network = chain.Testnet
	height := chain.Testnet.ForkHeight() + 100
	coinbase := func(tag byte) chain.Transaction {
		tx := wire.NewMsgTx(2)
		script, _ := txscript.NewScriptBuilder().AddInt64(int64(height)).AddData([]byte{tag}).Script()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: ^uint32(0)}, script, nil))
		tx.AddTxOut(wire.NewTxOut(200000, e.scripts[chain.BTC]))
		return chain.Transaction{TxID: tx.TxHash().String(), Hex: contract.Hex(tx), Height: height, Confirmations: 200}
	}
	coin, other := coinbase(1), coinbase(2)
	script := hex.EncodeToString(e.scripts[chain.BTC])
	out := &chain.TxOut{Value: 200000, Confirmations: 200}
	out.Script.Hex = script
	btc := &fundsProofBackend{fundsBackend: &fundsBackend{outputs: map[string]*chain.TxOut{chain.OutpointKey(coin.TxID, 0): out}}, txs: map[string]chain.Transaction{coin.TxID: coin}, coinbases: map[uint32]chain.Transaction{height: coin}}
	blake := &fundsProofBackend{fundsBackend: &fundsBackend{}, coinbases: map[uint32]chain.Transaction{height: other}}
	e.nodes[chain.BTC] = btc
	e.nodes[chain.Blake] = blake
	e.walletCoins = map[chain.ID]map[string][]chain.UTXO{chain.BTC: {script: {{TxID: coin.TxID, Amount: 200000, Confirmations: 200, Script: script}}}}
	req := Request{Method: "wallet.preflight", Params: []byte(`{"chain":"btc","amount":100000,"fee":2000,"expected_network":"testnet"}`)}
	check := func(state string) {
		t.Helper()
		got, err := e.preflightFunds(context.Background(), req)
		if err != nil || got.State != state || !got.Sufficient {
			t.Fatal(got, err)
		}
	}
	check("proven")
	blake.coinbases[height] = coin
	check("not_proven")
	delete(blake.coinbases, height)
	check("unavailable")
	blake.coinbases[height] = other
	check("proven")
	// No public readiness value is stored in the engine or consumed by signing.
	blake.coinbases[height] = coin
	check("not_proven")
	if len(e.s.CoinReservations) != 0 || len(e.s.Sends) != 0 || len(e.s.Swaps) != 0 {
		t.Fatal("preflight mutated obligations")
	}
}
