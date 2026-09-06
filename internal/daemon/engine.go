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
	"sync/atomic"
	"time"
)

type Engine struct {
	mu            sync.Mutex
	feeQuoteBusy  atomic.Bool
	preflightBusy atomic.Bool
	htlcBalances  map[chain.ID]int64
	htlcAvailable map[chain.ID]bool
	Config        Config
	s             State
	vault         *storage.Vault
	keys          *wallet.Keys
	identity      nostr.SecretKey
	nodes         map[chain.ID]chain.Backend
	watch         map[chain.ID]chain.Backend
	scanners      map[chain.ID]chain.SpendScanner
	towerScanners map[chain.ID]chain.SpendScanner
	receiveBook   map[chain.ID][]receiveAddress
	receiveReady  map[chain.ID]bool
	walletCoins   map[chain.ID]map[string][]chain.UTXO
	walletCursor  map[chain.ID]int
	sendCursor    string
	addresses     map[chain.ID]string
	scripts       map[chain.ID][]byte
	heights       map[chain.ID]uint32
	clocks        map[chain.ID]uint32
	balances      map[chain.ID]int64
	lastError     string
	fatal         error
}

func Open(ctx context.Context, c Config) (*Engine, error) {
	c.Network = c.Network.Normalized()
	if !c.Network.Valid() {
		return nil, errors.New("invalid network")
	}
	if c.Mode != "trader" && c.Mode != "tower" {
		return nil, errors.New("mode must be trader or tower")
	}
	if c.RescueFeeBPS < 0 || c.RescueFeeBPS > 1000 {
		return nil, errors.New("rescue fee must be 1–1000 basis points (0 uses the default)")
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
	en := &Engine{Config: c, vault: v, nodes: map[chain.ID]chain.Backend{}, watch: map[chain.ID]chain.Backend{}, scanners: map[chain.ID]chain.SpendScanner{}, addresses: map[chain.ID]string{}, scripts: map[chain.ID][]byte{}, heights: map[chain.ID]uint32{}, clocks: map[chain.ID]uint32{}, balances: map[chain.ID]int64{}}
	fail := func(err error) (*Engine, error) { en.Close(); return nil, err }
	if _, e = v.Load(&en.s); e != nil {
		return fail(e)
	}
	if en.s.Version == 0 {
		m := c.InitialMnemonic
		var e error
		if m == "" {
			m, e = wallet.NewMnemonic()
		} else {
			_, e = wallet.FromMnemonic(m)
		}
		if e != nil {
			return fail(e)
		}
		en.s = State{Version: 1, Network: c.Network, Mnemonic: m, Offers: map[string]nostr.Event{}, Book: map[string]nostr.Event{}, Swaps: map[string]*Swap{}, Outbox: map[string]*Delivery{}, Seen: map[string]string{}, TowerJobs: map[string]*TowerJob{}}
		if e = v.Save(en.s); e != nil {
			return fail(e)
		}
	}
	if c.InitialMnemonic != "" && c.InitialMnemonic != en.s.Mnemonic {
		return fail(errors.New("wallet seed differs from this profile"))
	}
	if en.s.Version != 1 {
		return fail(errors.New("unsupported state version"))
	}
	if en.s.Network.Normalized() != c.Network {
		return fail(errors.New("state belongs to a different network; use its own data directory"))
	}
	// An old pause flag must never suppress trading or rescue work after reopen.
	en.s.Paused = false
	en.keys, e = wallet.FromMnemonic(en.s.Mnemonic)
	if e != nil {
		return fail(e)
	}
	en.keys.SetNetwork(c.Network)
	key, e := en.keys.Derive(2, "nostr-identity")
	if e != nil {
		return fail(e)
	}
	en.identity = nostr.SecretKey(key.Serialize())
	en.towerScanners = map[chain.ID]chain.SpendScanner{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		cfg, ok := c.Nodes[id]
		if !ok {
			return fail(errors.New("both chains required"))
		}
		var r chain.Backend
		var e error
		if cfg.Kind == "electrum" {
			r, e = chain.NewElectrum(c.Network, id, cfg.URL, cfg.CertificateSHA256)
		} else if cfg.Kind == "rpc" || cfg.Kind == "" {
			r, e = chain.NewFor(c.Network, id, cfg.URL, cfg.Cookie)
		} else {
			e = errors.New("unknown node backend")
		}
		if e != nil {
			return fail(e)
		}
		en.nodes[id] = r
		if e = r.Check(ctx); e != nil {
			return fail(e)
		}
		en.nodes[id] = r
		if rpc, ok := r.(*chain.RPC); ok {
			en.scanners[id] = &chain.Scanner{RPC: rpc}
			en.towerScanners[id] = &chain.Scanner{RPC: rpc}
		} else {
			en.scanners[id] = r.(chain.SpendScanner)
			en.towerScanners[id] = r.(chain.SpendScanner)
		}
		if err := en.loadReceiveAddresses(ctx, id); err != nil {
			return fail(err)
		}
		if err := en.refreshChain(ctx, id); err != nil {
			return fail(err)
		}
		if c.ChainReady != nil {
			c.ChainReady(id, en.heights[id])
		}
	}
	if c.Mode == "tower" {
		en.Config.Tower.PubKey = en.identity.Public().Hex()
		en.Config.Tower.Scripts = map[chain.ID]string{}
		en.Config.Tower.Scripts = en.ownTower().Scripts
		if en.Config.Tower.BPS < 1 || en.Config.Tower.BPS > 1000 {
			return fail(errors.New("tower rate must be 1–1000 basis points"))
		}
	}
	if err := en.scrubOfferCache(); err != nil {
		return fail(err)
	}
	en.reconcileReservations()
	if err := en.save(); err != nil {
		return fail(err)
	}
	return en, nil
}
func (e *Engine) Close() error {
	for _, r := range e.nodes {
		_ = r.Close()
	}
	return e.vault.Close()
}
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
		if err := e.refreshChain(ctx, id); err != nil {
			return err
		}
	}
	e.reconcileReservations()
	return nil
}

// Complete wallet observations before publishing this chain's startup readiness.
func (e *Engine) refreshChain(ctx context.Context, id chain.ID) error {
	if e.htlcAvailable != nil {
		e.htlcAvailable[id] = false
	}
	h, err := e.nodes[id].Height(ctx)
	if err != nil {
		return err
	}
	e.heights[id] = h
	e.clocks[id] = h
	if e.Config.Network != chain.Regtest {
		stamp, err := e.nodes[id].MedianTime(ctx)
		if err != nil {
			return err
		}
		e.clocks[id] = stamp
	}
	if err := e.rotateReceiveAddress(ctx, id); err != nil {
		return err
	}
	coins, err := e.refreshWalletCoins(ctx, id)
	if err != nil {
		return err
	}
	var balance int64
	for _, coin := range coins {
		if coin.Confirmations >= e.Config.Network.Confirmations() {
			balance += int64(coin.Amount)
		}
	}
	e.balances[id] = balance
	e.refreshHTLCBalance(ctx, id)
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
	e.advanceSends(ctx)
	if err := e.advertiseTower(); err != nil {
		return err
	}
	e.pruneDiscovery()
	e.lastError = ""
	if err := e.refreshFavoriteTowers(); err != nil {
		e.lastError = "watchtower discovery: " + err.Error()
	}
	filters := []nostr.Filter{{Kinds: []nostr.Kind{transport.TowerKind}, Tags: nostr.TagMap{"t": {e.Config.Network.Namespace()}}}, {Kinds: []nostr.Kind{transport.OfferKind}, Tags: nostr.TagMap{"t": {e.Config.Network.Namespace()}}}, {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {e.identity.Public().Hex()}}}}
	for _, url := range e.Config.Relays {
		events, err := transport.PullAs(ctx, url, e.identity, filters...)
		if err != nil {
			e.lastError = fmt.Sprintf("relay %s: %v", url, err)
			continue
		}
		sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt < events[j].CreatedAt })
		for _, event := range events {
			if event.Kind == transport.TowerKind {
				e.ingestTower(event)
				continue
			}
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
	if e.Config.Mode == "trader" {
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
	// Remote registrations have their own scan cursor and time budget. Their
	// failures must not suppress our claims/refunds or durable message delivery.
	towerCtx, cancelTower := context.WithTimeout(ctx, 5*time.Second)
	e.refreshTowerJobs(towerCtx)
	towerObservations, towerErr := e.scanTower(towerCtx)
	if towerErr == nil {
		towerErr = e.advanceTower(towerCtx, towerObservations)
	}
	cancelTower()
	if towerErr != nil {
		e.lastError = "watchtower: " + towerErr.Error()
	}
	if err = e.save(); err != nil {
		return err
	}
	return e.flush(ctx)
}
func (e *Engine) ingestOffer(event nostr.Event) {
	o, err := protocol.DecodeOffer(event, time.Now().Unix())
	if err != nil || o.Network.Normalized() != e.Config.Network {
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
	var event nostr.Event
	var expires int64
	if discoveryMessage(typ) {
		pending := 0
		for _, d := range e.s.Outbox {
			if d.Expires > 0 {
				pending++
			}
		}
		if pending >= 256 {
			return errors.New("discovery queue capacity reached")
		}
		expires = time.Now().Unix() + 900
		event, err = transport.WrapExpiringFor(e.Config.Network.Namespace(), e.identity, pub, m, expires)
	} else {
		event, err = transport.WrapFor(e.Config.Network.Namespace(), e.identity, pub, m)
	}
	if err != nil {
		return err
	}
	e.s.Outbox[id] = &Delivery{Expires: expires, Type: typ, Event: event, To: to, MessageID: id, Digest: protocol.Digest(m), IsAck: typ == "ack"}
	return nil
}
func (e *Engine) queueEvent(event nostr.Event) {
	id := event.ID.Hex()
	e.s.Outbox[id] = &Delivery{Event: event, MessageID: id, IsAck: true}
}
func (e *Engine) flush(ctx context.Context) error {
	now := time.Now().Unix()
	for id, d := range e.s.Outbox {
		if d.Expires > 0 && d.Expires <= now {
			delete(e.s.Outbox, id)
			continue
		}
		if d.Type == "tower-query" && d.Published {
			continue
		}
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
				e.lastError = fmt.Sprintf("relay %s: %v", url, err)
				all = false
			}
		}
		d.Published = all
		if (d.To == "" || (d.Expires > 0 && d.Type != "tower-query")) && all {
			delete(e.s.Outbox, id)
		}
	}
	return e.save()
}
func (e *Engine) receive(event nostr.Event) error {
	from, m, err := transport.UnwrapFor(e.Config.Network.Namespace(), e.identity, event)
	if err != nil {
		return err
	}
	if discoveryMessage(m.Type) {
		e.pruneDiscovery()
		expires, err := strconv.ParseInt(transport.Tag(event, "expiration"), 10, 64)
		now := time.Now().Unix()
		if err != nil || expires <= now || expires > now+900 {
			return errors.New("expired or invalid discovery envelope")
		}
		key := from.Hex() + ":" + protocol.Digest(m)
		if e.s.DiscoverySeen[key] > now {
			return nil
		}
		if len(e.s.DiscoverySeen) >= 2000 {
			return errors.New("discovery inbox capacity reached")
		}
		if err := e.handle(from.Hex(), m); err != nil {
			return err
		}
		e.s.DiscoverySeen[key] = expires
		return e.save()
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
	raw, err := o.PublicJSON()
	if err != nil {
		return err
	}
	at := nostr.Now()
	if at <= e.s.EventTime {
		at = e.s.EventTime + 1
	}
	e.s.EventTime = at
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: at, Tags: nostr.Tags{{"d", o.ID}, {"t", e.Config.Network.Namespace()}, {"expiration", strconv.FormatInt(o.Expires, 10)}}, Content: string(raw)}
	if err = transport.Sign(&event, e.identity); err != nil {
		return err
	}
	for id, d := range e.s.Outbox {
		if d.Event.Kind == transport.OfferKind && transport.Tag(d.Event, "d") == o.ID {
			delete(e.s.Outbox, id)
		}
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
	return e.fundReserved(ctx, c, "")
}
func (e *Engine) fundReserved(ctx context.Context, c contract.HTLC, owner string) (*wire.MsgTx, error) {
	coins := e.knownCoins(c.Chain)
	reserved := e.reservedCoins(c.Chain, owner)
	var selected []chain.UTXO
	var total int64
	for _, coin := range coins {
		if coin.Confirmations < e.Config.Network.Confirmations() || reserved[chain.OutpointKey(coin.TxID, coin.Vout)] {
			continue
		}
		out, err := e.nodes[c.Chain].Output(ctx, coin.TxID, coin.Vout)
		if err != nil {
			return nil, err
		}
		if out == nil || out.Value != coin.Amount || out.Script.Hex != coin.Script || out.Confirmations < e.Config.Network.Confirmations() {
			continue
		}
		selected = append(selected, coin)
		total += int64(coin.Amount)
		change := total - c.Amount - e.fundingFee(owner)
		if change == 0 || change >= contract.Dust {
			break
		}
	}
	keys := map[string]*btcec.PrivateKey{}
	for _, address := range e.receiveBook[c.Chain] {
		keys[hex.EncodeToString(address.script)] = address.key
	}
	if policy, ok := e.s.FundingFees[owner]; ok && policy.Rate > 0 {
		scripts := [][]byte{make([]byte, 34)}
		if total > c.Amount+policy.FundingFee {
			scripts = append(scripts, e.scripts[c.Chain])
		}
		vsize, err := contract.PaymentVSize(len(selected), scripts...)
		if err != nil {
			return nil, err
		}
		minimum, err := contract.FeeForVSize(policy.Rate, vsize)
		if err != nil {
			return nil, err
		}
		if policy.FundingFee < minimum {
			return nil, errors.New("funding input count changed beyond the reviewed fee; funding remains blocked with reservations intact")
		}
	}
	tx, err := contract.FundWithKeys(c, selected, keys, e.scripts[c.Chain], e.fundingFee(owner))
	if err != nil {
		return nil, err
	}
	if c.Chain == chain.BTC {
		if err = chain.ProveBTCExclusive(ctx, e.Config.Network, e.nodes[chain.BTC], e.nodes[chain.Blake], tx); err != nil {
			return nil, err
		}
	}
	return tx, nil
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
	if out.Confirmations < e.Config.Network.Confirmations() {
		return false, nil
	}
	if c.Chain == chain.BTC {
		t, err := e.nodes[chain.BTC].Transaction(ctx, c.TxID)
		if err != nil {
			return false, err
		}
		tx, err := contract.Parse(t.Hex)
		if err != nil {
			return false, err
		}
		if err = chain.ProveBTCExclusive(ctx, e.Config.Network, e.nodes[chain.BTC], e.nodes[chain.Blake], tx); err != nil {
			return false, err
		}
	}
	return true, nil
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
		if s.Terms.Offer().Network.Normalized() == chain.Regtest {
			add(s.Long, s.Terms.Long.RefundHeight-protocol.LongBlocks)
			add(s.Short, s.Terms.Short.RefundHeight-protocol.ShortBlocks)
		} else {
			add(s.Long, s.Terms.StartHeights[s.Long.Chain])
			add(s.Short, s.Terms.StartHeights[s.Short.Chain])
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

func (e *Engine) scanTower(ctx context.Context) (map[chain.ID]map[string]chain.Observation, error) {
	points := map[chain.ID][]string{}
	starts := map[chain.ID]uint32{chain.BTC: e.heights[chain.BTC], chain.Blake: e.heights[chain.Blake]}
	add := func(c contract.HTLC, start uint32) {
		points[c.Chain] = append(points[c.Chain], chain.OutpointKey(c.TxID, c.Vout))
		if start < starts[c.Chain] {
			starts[c.Chain] = start
		}
	}
	for _, j := range e.s.TowerJobs {
		if j.Expired {
			continue
		}
		// Accepted jobs retain their signed rate when the provider changes its quote.
		if err := j.Job.Validate(e.ownTower().Scripts, j.Job.BPS); err != nil {
			j.Error = err.Error()
			continue
		}
		add(j.Job.Target, j.Job.ScanFrom)
		if j.Job.Observe != nil {
			start := j.Job.ObserveScanFrom
			if start == 0 {
				start = j.Job.ScanFrom
			}
			add(*j.Job.Observe, start)
		}
	}
	out := map[chain.ID]map[string]chain.Observation{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if len(points[id]) == 0 {
			out[id] = map[string]chain.Observation{}
			continue
		}
		result, err := e.towerScanners[id].Scan(ctx, starts[id], points[id])
		if err != nil {
			return nil, err
		}
		out[id] = result
	}
	return out, nil
}
