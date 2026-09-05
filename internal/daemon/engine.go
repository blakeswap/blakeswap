package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Engine struct {
	mu        sync.Mutex
	Config    Config
	s         State
	vault     *storage.Vault
	keys      *wallet.Keys
	identity  nostr.SecretKey
	nodes     map[chain.ID]*chain.RPC
	watch     map[chain.ID]*chain.RPC
	scanners  map[chain.ID]*chain.Scanner
	addresses map[chain.ID]string
	scripts   map[chain.ID][]byte
	heights   map[chain.ID]uint32
	balances  map[chain.ID]int64
	lastError string
	fatal     error
}

func Open(ctx context.Context, c Config) (*Engine, error) {
	if c.Mode != "trader" && c.Mode != "tower" {
		return nil, errors.New("mode must be trader or tower")
	}
	if len(c.Relays) < 1 || len(c.Relays) > 3 {
		return nil, errors.New("configure one to three relay URLs")
	}
	password, e := os.ReadFile(c.PasswordFile)
	if e != nil {
		return nil, e
	}
	defer clear(password)
	v, e := storage.Open(filepath.Join(c.DataDir, "state.db"), bytes.TrimSpace(password))
	if e != nil {
		return nil, e
	}
	en := &Engine{Config: c, vault: v, nodes: map[chain.ID]*chain.RPC{}, watch: map[chain.ID]*chain.RPC{}, scanners: map[chain.ID]*chain.Scanner{}, addresses: map[chain.ID]string{}, scripts: map[chain.ID][]byte{}, heights: map[chain.ID]uint32{}, balances: map[chain.ID]int64{}}
	fail := func(err error) (*Engine, error) { v.Close(); return nil, err }
	if _, e = v.Load(&en.s); e != nil {
		return fail(e)
	}
	if en.s.Version == 0 {
		m, e := wallet.NewMnemonic()
		if e != nil {
			return fail(e)
		}
		en.s = State{Version: 1, Mnemonic: m, Offers: map[string]nostr.Event{}, Book: map[string]nostr.Event{}, Swaps: map[string]*Swap{}, Outbox: map[string]*Delivery{}, Seen: map[string]string{}, TowerJobs: map[string]*TowerJob{}}
		if e = v.Save(en.s); e != nil {
			return fail(e)
		}
	}
	if en.s.Version != 1 {
		return fail(errors.New("unsupported state version"))
	}
	en.keys, e = wallet.FromMnemonic(en.s.Mnemonic)
	if e != nil {
		return fail(e)
	}
	key, e := en.keys.Derive(2, "nostr-identity")
	if e != nil {
		return fail(e)
	}
	en.identity = nostr.SecretKey(key.Serialize())
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		cfg, ok := c.Nodes[id]
		if !ok {
			return fail(errors.New("both chains required"))
		}
		r, e := chain.New(id, cfg.URL, cfg.Cookie)
		if e != nil {
			return fail(e)
		}
		if e = r.Check(ctx); e != nil {
			return fail(e)
		}
		en.nodes[id] = r
		en.scanners[id] = &chain.Scanner{RPC: r}
		key, e := en.keys.Spending(id, "deposit")
		if e != nil {
			return fail(e)
		}
		addr, script, e := wallet.Address(key.PubKey())
		if e != nil {
			return fail(e)
		}
		en.addresses[id] = addr
		en.scripts[id] = script
		w, e := r.Observe(ctx, "blakeswap-"+en.identity.Public().Hex()[:20], []string{addr})
		if e != nil {
			return fail(e)
		}
		en.watch[id] = w
	}
	if c.Mode == "tower" {
		en.Config.Tower.PubKey = en.identity.Public().Hex()
		en.Config.Tower.Scripts = map[chain.ID]string{}
		for id, script := range en.scripts {
			en.Config.Tower.Scripts[id] = hex.EncodeToString(script)
		}
		if en.Config.Tower.BPS < 1 || en.Config.Tower.BPS > 1000 {
			return fail(errors.New("tower rate must be 1–1000 basis points"))
		}
	}
	if e = en.refresh(ctx); e != nil {
		return fail(e)
	}
	return en, nil
}
func (e *Engine) Close() error { return e.vault.Close() }
func (e *Engine) save() error {
	if e.fatal != nil {
		return e.fatal
	}
	if err := e.vault.Save(e.s); err != nil {
		e.fatal = fmt.Errorf("durability failure; execution stopped: %w", err)
		return e.fatal
	}
	return nil
}
func (e *Engine) refresh(ctx context.Context) error {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		h, err := e.nodes[id].Height(ctx)
		if err != nil {
			return err
		}
		e.heights[id] = h
		coins, err := e.watch[id].Unspent(ctx, []string{e.addresses[id]})
		if err != nil {
			return err
		}
		var balance int64
		for _, coin := range coins {
			if coin.Confirmations >= protocol.Confirmations {
				balance += int64(coin.Amount)
			}
		}
		e.balances[id] = balance
	}
	return nil
}
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := e.Tick(ctx); err != nil {
			e.mu.Lock()
			e.lastError = err.Error()
			fatal := e.fatal
			e.mu.Unlock()
			if fatal != nil {
				return fatal
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (e *Engine) Tick(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil {
		return e.fatal
	}
	if err := e.refresh(ctx); err != nil {
		return err
	}
	if e.s.Paused {
		return nil
	}
	e.lastError = ""
	filters := []nostr.Filter{{Kinds: []nostr.Kind{transport.OfferKind}, Tags: nostr.TagMap{"t": {transport.Namespace}}}, {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {e.identity.Public().Hex()}}}}
	for _, url := range e.Config.Relays {
		events, err := transport.Pull(ctx, url, filters...)
		if err != nil {
			e.lastError = err.Error()
			continue
		}
		sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt < events[j].CreatedAt })
		for _, event := range events {
			if event.Kind == transport.OfferKind {
				e.ingestOffer(event)
				continue
			}
			if err = e.receive(event); err != nil {
				e.lastError = "mailbox: " + err.Error()
			}
		}
	}
	observations, err := e.scan(ctx)
	if err != nil {
		return err
	}
	if e.Config.Mode == "tower" {
		err = e.advanceTower(ctx, observations)
	} else {
		ids := make([]string, 0, len(e.s.Swaps))
		for id := range e.s.Swaps {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			swap := e.s.Swaps[id]
			if swap.Stage != "rejected" {
				swap.Error = ""
			}
			if err := e.advanceSwap(ctx, swap, observations); err != nil {
				swap.Error = err.Error()
			}
		}
	}
	if err != nil {
		return err
	}
	if err = e.save(); err != nil {
		return err
	}
	return e.flush(ctx)
}
func (e *Engine) ingestOffer(event nostr.Event) {
	o, err := protocol.DecodeOffer(event, time.Now().Unix())
	if err != nil {
		return
	}
	key := o.Maker + ":" + o.ID
	old, exists := e.s.Book[key]
	if !exists || event.CreatedAt > old.CreatedAt || (event.CreatedAt == old.CreatedAt && event.ID.Hex() < old.ID.Hex()) {
		e.s.Book[key] = event
	}
}
func (e *Engine) queue(to, typ, swapID string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	id := protocol.Digest([]string{to, typ, swapID, string(raw)})
	if e.s.Outbox[id] != nil {
		return nil
	}
	pub, err := nostr.PubKeyFromHex(to)
	if err != nil {
		return err
	}
	m := transport.Message{Version: 1, ID: id, Type: typ, SwapID: swapID, Body: raw}
	event, err := transport.Wrap(e.identity, pub, m)
	if err != nil {
		return err
	}
	e.s.Outbox[id] = &Delivery{Event: event, To: to, MessageID: id, Digest: protocol.Digest(m), IsAck: typ == "ack"}
	return nil
}
func (e *Engine) queueEvent(event nostr.Event) {
	id := event.ID.Hex()
	e.s.Outbox[id] = &Delivery{Event: event, MessageID: id, IsAck: true}
}
func (e *Engine) flush(ctx context.Context) error {
	now := time.Now().Unix()
	for id, d := range e.s.Outbox {
		interval := int64(5)
		if d.IsAck && d.Published {
			interval = 60
		}
		if now-d.LastAttempt < interval {
			continue
		}
		d.LastAttempt = now
		all := true
		for _, url := range e.Config.Relays {
			if err := transport.Publish(ctx, url, d.Event); err != nil {
				e.lastError = err.Error()
				all = false
			}
		}
		d.Published = all
		if d.To == "" && all {
			delete(e.s.Outbox, id)
		}
	}
	return e.save()
}
func (e *Engine) receive(event nostr.Event) error {
	from, m, err := transport.Unwrap(e.identity, event)
	if err != nil {
		return err
	}
	if len(e.s.Seen) > 10000 || len(e.s.Outbox) > 10000 {
		return errors.New("mailbox capacity reached")
	}
	digest := protocol.Digest(m)
	seenKey := from.Hex() + ":" + m.ID
	if previous := e.s.Seen[seenKey]; previous != "" {
		if previous != digest {
			return errors.New("message ID reused with different contents")
		}
		if m.Type != "ack" {
			return e.queue(from.Hex(), "ack", m.SwapID, map[string]string{"id": m.ID, "digest": digest})
		}
		return nil
	}
	if m.Type == "ack" {
		var a struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
		}
		if err = json.Unmarshal(m.Body, &a); err != nil {
			return err
		}
		if delivery := e.s.Outbox[a.ID]; delivery != nil && delivery.To == from.Hex() && delivery.Digest == a.Digest && !delivery.IsAck {
			delete(e.s.Outbox, a.ID)
		}
	} else {
		if err = e.handle(from.Hex(), m); err != nil {
			return err
		}
		if err = e.queue(from.Hex(), "ack", m.SwapID, map[string]string{"id": m.ID, "digest": digest}); err != nil {
			return err
		}
	}
	e.s.Seen[seenKey] = digest
	return e.save()
}
func (e *Engine) publishOffer(o protocol.Offer) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	at := nostr.Now()
	if at <= e.s.EventTime {
		at = e.s.EventTime + 1
	}
	e.s.EventTime = at
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: at, Tags: nostr.Tags{{"d", o.ID}, {"t", transport.Namespace}, {"expiration", strconv.FormatInt(o.Expires, 10)}}, Content: string(raw)}
	if err = transport.Sign(&event, e.identity); err != nil {
		return err
	}
	e.s.Offers[o.ID] = event
	e.ingestOffer(event)
	e.queueEvent(event)
	return nil
}
func (e *Engine) swapKey(id chain.ID, swapID string) (*btcec.PrivateKey, error) {
	return e.keys.Spending(id, "swap/"+swapID)
}
func (e *Engine) swapKeys(swapID string) (map[chain.ID]string, error) {
	keys := map[chain.ID]string{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		key, err := e.swapKey(id, swapID)
		if err != nil {
			return nil, err
		}
		keys[id] = hex.EncodeToString(key.PubKey().SerializeCompressed())
	}
	return keys, nil
}
func (e *Engine) fund(ctx context.Context, c contract.HTLC) (*wire.MsgTx, error) {
	coins, err := e.watch[c.Chain].Unspent(ctx, []string{e.addresses[c.Chain]})
	if err != nil {
		return nil, err
	}
	reserved := map[string]bool{}
	for _, s := range e.s.Swaps {
		raw := s.LongFunding
		fundingChain := s.Long.Chain
		if s.Role == "maker" {
			raw = s.ShortFunding
			fundingChain = s.Short.Chain
		}
		if fundingChain != c.Chain {
			continue
		}
		for _, raw := range []string{raw} {
			if raw == "" {
				continue
			}
			tx, err := contract.Parse(raw)
			if err != nil {
				return nil, err
			}
			for _, in := range tx.TxIn {
				reserved[chain.OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)] = true
			}
		}
	}
	var selected []chain.UTXO
	var total int64
	for _, coin := range coins {
		if coin.Confirmations < protocol.Confirmations || reserved[chain.OutpointKey(coin.TxID, coin.Vout)] {
			continue
		}
		out, err := e.nodes[c.Chain].Output(ctx, coin.TxID, coin.Vout)
		if err != nil {
			return nil, err
		}
		if out == nil || out.Value != coin.Amount || out.Script.Hex != coin.Script || out.Confirmations < protocol.Confirmations {
			continue
		}
		selected = append(selected, coin)
		total += int64(coin.Amount)
		change := total - c.Amount - protocol.FundingFee
		if change == 0 || change >= contract.Dust {
			break
		}
	}
	key, err := e.keys.Spending(c.Chain, "deposit")
	if err != nil {
		return nil, err
	}
	return contract.Fund(c, selected, key, protocol.FundingFee)
}
func (e *Engine) broadcast(ctx context.Context, id chain.ID, raw string) error {
	tx, err := contract.Parse(raw)
	if err != nil {
		return err
	}
	r := e.nodes[id]
	if _, err = r.Broadcast(ctx, raw); err != nil {
		known, lookup := r.Transaction(ctx, tx.TxHash().String())
		if lookup == nil && known.Confirmations >= 0 {
			return nil
		}
		return err
	}
	return nil
}
func (e *Engine) funded(ctx context.Context, c contract.HTLC) (bool, error) {
	if c.TxID == "" {
		return false, nil
	}
	out, err := e.nodes[c.Chain].Output(ctx, c.TxID, c.Vout)
	if err != nil {
		return false, err
	}
	if out == nil {
		return false, nil
	}
	pk, err := c.PkScript()
	if err != nil {
		return false, err
	}
	if int64(out.Value) != c.Amount || out.Script.Hex != hex.EncodeToString(pk) {
		return false, errors.New("on-chain funding output differs from agreed HTLC")
	}
	return out.Confirmations >= protocol.Confirmations, nil
}
func (e *Engine) scan(ctx context.Context) (map[chain.ID]map[string]chain.Observation, error) {
	points := map[chain.ID][]string{}
	starts := map[chain.ID]uint32{chain.BTC: e.heights[chain.BTC], chain.Blake: e.heights[chain.Blake]}
	add := func(c contract.HTLC, start uint32) {
		if c.TxID == "" {
			return
		}
		points[c.Chain] = append(points[c.Chain], chain.OutpointKey(c.TxID, c.Vout))
		if start < starts[c.Chain] {
			starts[c.Chain] = start
		}
	}
	for _, s := range e.s.Swaps {
		if s.Terms == nil {
			continue
		}
		add(s.Long, s.Terms.Long.RefundHeight-protocol.LongBlocks)
		add(s.Short, s.Terms.Short.RefundHeight-protocol.ShortBlocks)
	}
	for _, j := range e.s.TowerJobs {
		add(j.Job.Target, j.Job.ScanFrom)
		if j.Job.Observe != nil {
			add(*j.Job.Observe, j.Job.ScanFrom)
		}
	}
	out := map[chain.ID]map[string]chain.Observation{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		result, err := e.scanners[id].Scan(ctx, starts[id], points[id])
		if err != nil {
			return nil, err
		}
		out[id] = result
	}
	return out, nil
}
