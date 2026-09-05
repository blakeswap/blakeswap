package daemon

import (
	"blakeswap/internal/chain"
	"blakeswap/internal/protocol"
	"blakeswap/internal/transport"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (e *Engine) Status() Status { e.mu.Lock(); defer e.mu.Unlock(); return e.status() }
func (e *Engine) status() Status {
	s := Status{Name: e.Config.Name, Mode: e.Config.Mode, PubKey: e.identity.Public().Hex(), Addresses: map[chain.ID]string{}, Balances: map[chain.ID]int64{}, Heights: map[chain.ID]uint32{}, Paused: e.s.Paused, Orders: []protocol.Offer{}, Swaps: []PublicSwap{}, TowerJobs: []map[string]any{}, LastError: e.lastError, Tower: e.Config.Tower}
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
			var addr string
			if err := e.nodes[id].WithWallet("faucet").Call(ctx, "getnewaddress", &addr); err != nil {
				return nil, err
			}
			if err := e.nodes[id].Call(ctx, "generatetoaddress", nil, p.Blocks, addr); err != nil {
				return nil, err
			}
		}
		return true, e.refresh(ctx)
	case "regtest.faucet":
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
		var txid string
		if err := e.nodes[p.Chain].WithWallet("faucet").Call(ctx, "sendtoaddress", &txid, e.addresses[p.Chain], chain.Coins(p.Amount)); err != nil {
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

// One versioned JSON request/response per connection; per-user socket permissions
// are the local authentication boundary. No wallet HTTP server or browser origin.
func (e *Engine) Serve(ctx context.Context) error {
	path := e.Config.Socket
	if !filepath.IsAbs(path) {
		return errors.New("socket path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing to replace a non-socket file")
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return errors.New("daemon socket already active")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	go func() { <-ctx.Done(); listener.Close() }()
	limit := make(chan struct{}, 16)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case limit <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func() {
			defer func() { <-limit; conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
			scanner := bufio.NewScanner(conn)
			scanner.Buffer(make([]byte, 4096), 128*1024)
			if !scanner.Scan() {
				return
			}
			var req Request
			resp := Response{}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				resp.Error = "invalid JSON request"
			} else {
				result, err := e.Command(ctx, req)
				if err != nil {
					resp.Error = err.Error()
				} else {
					resp.Result = result
				}
			}
			_ = json.NewEncoder(conn).Encode(resp)
		}()
	}
}
func Call(ctx context.Context, socket string, req Request) (json.RawMessage, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	dec := json.NewDecoder(bufio.NewReaderSize(conn, 4096))
	if err = dec.Decode(&resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.Error) != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}
