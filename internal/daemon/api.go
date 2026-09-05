package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"path/filepath"
	"sort"
	"time"
)

func (e *Engine) Status() Status { e.mu.Lock(); defer e.mu.Unlock(); return e.status() }
func (e *Engine) status() Status {
	s := Status{Network: e.Config.Network, Name: e.Config.Name, Mode: e.Config.Mode, PubKey: e.identity.Public().Hex(), Addresses: map[chain.ID]string{}, Balances: map[chain.ID]int64{}, Heights: map[chain.ID]uint32{}, Paused: e.s.Paused, Orders: []protocol.Offer{}, Swaps: []PublicSwap{}, TowerJobs: []map[string]any{}, LastError: e.lastError, Tower: e.Config.Tower}
	for id, addr := range e.addresses {
		s.Addresses[id] = addr
		s.Balances[id] = e.balances[id]
		s.Heights[id] = e.heights[id]
	}
	for _, event := range e.s.Book {
		o, err := protocol.DecodeOffer(event, time.Now().Unix())
		if err == nil {
			s.Orders = append(s.Orders, o)
		}
	}
	sort.Slice(s.Orders, func(i, j int) bool { return s.Orders[i].ID < s.Orders[j].ID })
	for _, swap := range e.s.Swaps {
		p := PublicSwap{ID: swap.ID, Role: swap.Role, Stage: swap.Stage, Error: swap.Error, Long: swap.Long, Short: swap.Short, LongSpend: swap.LongSpend, ShortSpend: swap.ShortSpend, LongConfirmations: swap.LongConfirmations, ShortConfirmations: swap.ShortConfirmations, TowerPaid: swap.TowerPaid, TowerReady: towerReady(swap), SecretRevealed: swap.SecretExposed}
		p.TowerPayments = map[chain.ID]int64{}
		for id, amount := range swap.TowerPayments {
			p.TowerPayments[id] = amount
		}
		if swap.Terms != nil {
			p.TowerEnabled = swap.Terms.Offer().TowerBPS > 0
			p.TowerReady = p.TowerEnabled && len(swap.Jobs) > 0 && towerReady(swap)
			p.Takeover = swap.Terms.Takeover
			p.RevealBefore = swap.Terms.RevealBefore
		} else {
			p.TowerReady = false
		}
		s.Swaps = append(s.Swaps, p)
	}
	sort.Slice(s.Swaps, func(i, j int) bool { return s.Swaps[i].ID < s.Swaps[j].ID })
	for _, state := range e.s.TowerJobs {
		s.TowerJobs = append(s.TowerJobs, map[string]any{"id": state.Job.ID, "swap_id": state.Job.SwapID, "kind": state.Job.Kind, "chain": state.Job.Target.Chain, "eligible_height": state.Job.Lock, "broadcast": state.Broadcast, "confirmations": state.Confirmed, "secret_observed": state.Secret != "", "error": state.Error})
	}
	for _, d := range e.s.Outbox {
		if !d.IsAck {
			s.PendingMessages++
		}
	}
	return s
}
func (e *Engine) Command(ctx context.Context, req Request) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckCommandNetwork(req, e.Config.Network, false); err != nil {
		return nil, err
	}
	if e.fatal != nil {
		return nil, e.fatal
	}
	switch req.Method {
	case "status":
		return e.status(), nil
	case "pause":
		var p struct {
			Paused bool `json:"paused"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		e.s.Paused = p.Paused
		return e.status(), e.save()
	case "offer.create":
		if e.Config.Mode != "trader" {
			return nil, errors.New("tower cannot trade")
		}
		var o protocol.Offer
		if err := json.Unmarshal(req.Params, &o); err != nil {
			return nil, err
		}
		o.Network = e.Config.Network
		o.ID = transport.RandomID()
		o.Maker = e.identity.Public().Hex()
		o.Status = "open"
		o.Reservation = ""
		if o.Expires == 0 {
			o.Expires = time.Now().Unix() + 24*3600
		}
		if err := o.Validate(time.Now().Unix()); err != nil {
			return nil, err
		}
		if o.TowerBPS > 0 && (o.TowerBPS != e.Config.Tower.BPS || !protocol.Hex32(e.Config.Tower.PubKey)) {
			return nil, errors.New("tower quote does not match configured provider")
		}
		if err := e.refresh(ctx); err != nil {
			return nil, err
		}
		if e.balances[o.Sell] < o.SellAmount+protocol.FundingFee {
			return nil, errors.New("insufficient confirmed balance")
		}
		if len(e.s.Offers) >= 1000 {
			return nil, errors.New("order capacity reached")
		}
		if err := e.publishOffer(o); err != nil {
			return nil, err
		}
		return o, e.save()
	case "offer.cancel":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		event, ok := e.s.Offers[p.ID]
		if !ok {
			return nil, errors.New("unknown own offer")
		}
		o, err := protocol.DecodeOffer(event, int64(event.CreatedAt))
		if err != nil {
			return nil, err
		}
		if o.Status != "open" {
			return nil, errors.New("only unreserved offers can be cancelled; committed swaps settle or refund")
		}
		o.Status = "cancelled"
		if err = e.publishOffer(o); err != nil {
			return nil, err
		}
		return o, e.save()
	case "swap.take":
		if e.Config.Mode != "trader" || e.s.Paused {
			return nil, errors.New("trader is paused or unavailable")
		}
		var p struct {
			Maker string `json:"maker"`
			ID    string `json:"id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		event, ok := e.s.Book[p.Maker+":"+p.ID]
		if !ok {
			return nil, errors.New("offer not in verified orderbook")
		}
		o, err := protocol.DecodeOffer(event, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		if o.Status != "open" || o.Maker == e.identity.Public().Hex() {
			return nil, errors.New("offer not available to take")
		}
		if o.TowerBPS > 0 && (o.TowerBPS != e.Config.Tower.BPS || !protocol.Hex32(e.Config.Tower.PubKey)) {
			return nil, errors.New("tower quote mismatch")
		}
		if len(e.s.Swaps) >= 1000 {
			return nil, errors.New("swap capacity")
		}
		id := transport.RandomID()
		secret, err := hex.DecodeString(transport.RandomID())
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(secret)
		keys, err := e.swapKeys(id)
		if err != nil {
			return nil, err
		}
		request := protocol.Request{ID: id, OfferEvent: event, Taker: e.identity.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: keys}
		s := &Swap{ID: id, Role: "taker", Request: request, Secret: hex.EncodeToString(secret), Receipts: map[string]protocol.Receipt{}, Stage: "request queued"}
		e.s.Swaps[id] = s
		if err = e.queue(o.Maker, "request", id, request); err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, e.save()
	case "regtest.mine":
		if e.Config.Network != chain.Regtest {
			return nil, errors.New("mining is only available on regtest")
		}
		var p struct {
			Chain  chain.ID `json:"chain"`
			Blocks uint32   `json:"blocks"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, err
			}
		}
		if p.Blocks == 0 {
			p.Blocks = 2
		}
		if p.Blocks > 200 {
			return nil, errors.New("mine limit is 200 blocks")
		}
		ids := []chain.ID{chain.BTC, chain.Blake}
		if p.Chain != "" {
			if !p.Chain.Valid() {
				return nil, errors.New("unknown chain")
			}
			ids = []chain.ID{p.Chain}
		}
		for _, id := range ids {
			if _, ok := e.nodes[id].(*chain.RPC); !ok {
				return nil, errors.New("mining requires full-node RPC")
			}
			var addr string
			if err := e.nodes[id].(*chain.RPC).WithWallet("faucet").Call(ctx, "getnewaddress", &addr); err != nil {
				return nil, err
			}
			if err := e.nodes[id].(*chain.RPC).Call(ctx, "generatetoaddress", nil, p.Blocks, addr); err != nil {
				return nil, err
			}
		}
		return true, e.refresh(ctx)
	case "regtest.faucet":
		if e.Config.Network != chain.Regtest {
			return nil, errors.New("faucet is only available on regtest")
		}
		var p struct {
			Chain  chain.ID `json:"chain"`
			Amount int64    `json:"amount"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		if !p.Chain.Valid() || p.Amount < 100000 || p.Amount > 1000000000 {
			return nil, errors.New("invalid faucet amount")
		}
		if _, ok := e.nodes[p.Chain].(*chain.RPC); !ok {
			return nil, errors.New("faucet requires full-node RPC")
		}
		var txid string
		if err := e.nodes[p.Chain].(*chain.RPC).WithWallet("faucet").Call(ctx, "sendtoaddress", &txid, e.addresses[p.Chain], chain.Coins(p.Amount)); err != nil {
			return nil, err
		}
		return map[string]string{"txid": txid}, nil
	case "wallet.recovery":
		return map[string]string{"mnemonic": e.s.Mnemonic, "warning": "The mnemonic restores keys. Preserve an encrypted state backup for pending swap secrets and signed transactions."}, nil
	case "wallet.backup":
		path := filepath.Join(e.Config.DataDir, "backup-"+time.Now().UTC().Format("20060102T150405.000000000")+".db")
		if err := e.save(); err != nil {
			return nil, err
		}
		if err := e.vault.Backup(path); err != nil {
			return nil, err
		}
		return map[string]string{"path": path}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}
