package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/wire"
)

type fundingCrashBackend struct {
	chain.Backend
	before   func(string)
	accepted bool
}

func (b *fundingCrashBackend) Broadcast(ctx context.Context, raw string) (string, error) {
	b.before(raw)
	if b.accepted {
		if _, err := b.Backend.Broadcast(ctx, raw); err != nil {
			return "", err
		}
	}
	panic("simulated funding crash")
}

func TestRealFundingPublicationSurvivesCrash(t *testing.T) {
	for _, role := range []string{"taker", "maker"} {
		for _, accepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/accepted=%v", role, accepted), func(t *testing.T) {
				h := newHarness(t, 0)
				offer := h.command("maker", "offer.create", map[string]any{"sell": chain.BTC, "sell_amount": 1000000, "buy_amount": 2000000, "tower_bps": 0}).(protocol.Offer)
				h.tick("maker", "taker")
				id := h.command("taker", "swap.take", map[string]string{"maker": offer.Maker, "id": offer.ID}).(map[string]string)["id"]
				h.tick("taker", "maker")
				if role == "maker" {
					h.tick("taker")
					h.minePending()
				}
				e := h.engines[role]
				fundingChain := chain.Blake
				if role == "maker" {
					fundingChain = chain.BTC
				}
				crashed := false
				e.nodes[fundingChain] = &fundingCrashBackend{Backend: e.nodes[fundingChain], accepted: accepted, before: func(raw string) {
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					s := saved.Swaps[id]
					if s == nil || (role == "taker" && (!s.LongSent || s.LongFunding != raw)) || (role == "maker" && (!s.ShortSent || s.ShortFunding != raw)) {
						t.Fatal("publication not durable before broadcast")
					}
					if len(s.SelfRefunds) != len(protocol.RescueFees) {
						t.Fatal("refunds not durable before broadcast")
					}
					kind := "long-funded"
					if role == "maker" {
						kind = "short-funded"
					}
					found := false
					for _, d := range saved.Outbox {
						if d.Type == kind {
							found = true
						}
					}
					if !found {
						t.Fatal("funding notification not durable before broadcast")
					}
					crashed = true
				}}
				func() {
					defer func() {
						if r := recover(); r != "simulated funding crash" {
							t.Fatalf("unexpected crash: %v", r)
						}
					}()
					_ = e.Tick(h.ctx)
				}()
				if !crashed {
					t.Fatal("funding broadcast was not reached")
				}
				s := h.swap(role, id)
				own := s.Long
				if role == "maker" {
					own = s.Short
				}
				raw := s.LongFunding
				if role == "maker" {
					raw = s.ShortFunding
				}
				h.offline(role)
				// The peer cannot help. Recovery occurs beyond both funding and refund gates.
				other := "maker"
				if role == "maker" {
					other = "taker"
				}
				h.offline(other)
				h.mine(own.Chain, own.RefundHeight+1-h.height(own.Chain))
				h.online(role)
				h.tick(role)
				recovered := h.swap(role, id)
				if recovered.Stage != "refunding" {
					t.Fatal("did not resume refund", recovered.Stage, recovered.Error)
				}
				funding, err := contract.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = h.nodes[own.Chain].Transaction(h.ctx, funding.TxHash().String()); err != nil {
					t.Fatal("funding not recovered", err)
				}
				h.minePending()
				h.tick(role)
				if h.swap(role, id).Stage != "refunded" {
					t.Fatal("refund did not confirm", h.swap(role, id).Error)
				}
			})
		}
	}
}

func TestRealLegacyFundingSnapshotRecoversAfterDeadline(t *testing.T) {
	for _, role := range []string{"taker", "maker"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, 0)
			id := h.fundBoth(chain.BTC, 0)
			h.online(role)
			s := h.swap(role, id)
			own := s.Long
			kind := "long-funded"
			if role == "maker" {
				own = s.Short
				kind = "short-funded"
				s.ShortSent = false
			} else {
				s.LongSent = false
			}
			for key, d := range h.engines[role].s.Outbox {
				if d.Type == kind {
					delete(h.engines[role].s.Outbox, key)
				}
			}
			if err := h.engines[role].save(); err != nil {
				t.Fatal(err)
			}
			h.offline("maker")
			h.offline("taker")
			h.mine(own.Chain, own.RefundHeight+1-h.height(own.Chain))
			h.online(role)
			h.tick(role)
			recovered := h.swap(role, id)
			if recovered.Stage != "refunding" || (role == "maker" && !recovered.ShortSent) || (role == "taker" && !recovered.LongSent) {
				t.Fatal("legacy funding recovery failed", recovered.Stage, recovered.Error)
			}
			found := false
			for _, d := range h.engines[role].s.Outbox {
				if d.Type == kind {
					found = true
				}
			}
			if !found {
				t.Fatal("lost peer funding notification was not restored")
			}
			h.minePending()
			h.tick(role)
			if h.swap(role, id).Stage != "refunded" {
				t.Fatal("legacy refund did not confirm", h.swap(role, id).Error)
			}
		})
	}
}

type fundingLookupBackend struct {
	chain.Backend
	tx         chain.Transaction
	err        error
	broadcasts []string
}

func (b *fundingLookupBackend) Transaction(context.Context, string) (chain.Transaction, error) {
	return b.tx, b.err
}
func (b *fundingLookupBackend) Output(context.Context, string, uint32) (*chain.TxOut, error) {
	return nil, nil
}
func (b *fundingLookupBackend) Broadcast(_ context.Context, raw string) (string, error) {
	b.broadcasts = append(b.broadcasts, raw)
	tx, _ := contract.Parse(raw)
	return tx.TxHash().String(), nil
}

func TestPreparedFundingReconciliationHonorsDeadlineAndLookupErrors(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, outcome := range []string{"known", "missing", "unavailable"} {
			t.Run(role+"/"+outcome, func(t *testing.T) {
				e := discoveryEngine(t)
				mnemonic, err := wallet.NewMnemonic()
				if err != nil {
					t.Fatal(err)
				}
				keys, err := wallet.FromMnemonic(mnemonic)
				if err != nil {
					t.Fatal(err)
				}
				e.keys = keys
				maker, taker := nostr.Generate(), nostr.Generate()
				offer := protocol.Offer{ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 1000000, BuyAmount: 2000000, Expires: time.Now().Unix() + 3600, Status: "open"}
				raw, _ := json.Marshal(offer)
				event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", transport.Namespace}}, Content: string(raw)}
				if err := transport.Sign(&event, maker); err != nil {
					t.Fatal(err)
				}
				id := transport.RandomID()
				pubkeys := map[chain.ID]string{}
				for _, c := range []chain.ID{chain.BTC, chain.Blake} {
					key, err := e.swapKey(c, id)
					if err != nil {
						t.Fatal(err)
					}
					pubkeys[c] = hex.EncodeToString(key.PubKey().SerializeCompressed())
				}
				request := protocol.Request{ID: id, OfferEvent: event, Taker: taker.Public().Hex(), Hash: transport.RandomID(), Keys: pubkeys}
				terms, err := protocol.NewTerms(request, pubkeys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100}, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				s := &Swap{ID: id, Role: role, Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short}
				own := &s.Long
				if role == "maker" {
					own = &s.Short
				}
				tx := wire.NewMsgTx(2)
				tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
				pk, err := own.PkScript()
				if err != nil {
					t.Fatal(err)
				}
				tx.AddTxOut(wire.NewTxOut(own.Amount, pk))
				own.TxID = tx.TxHash().String()
				funding := contract.Hex(tx)
				if role == "maker" {
					s.ShortFunding = funding
				} else {
					s.LongFunding = funding
				}
				e.s.Swaps = map[string]*Swap{id: s}
				if err := e.prepare(s, *own); err != nil {
					t.Fatal(err)
				}
				e.heights = map[chain.ID]uint32{chain.BTC: terms.Short.RefundHeight + 1, chain.Blake: terms.Long.RefundHeight + 1}
				e.clocks = e.heights
				backend := &fundingLookupBackend{tx: chain.Transaction{TxID: own.TxID, Hex: funding}}
				if outcome == "missing" {
					backend.err = &chain.RPCError{Code: -5, Message: "transaction not found"}
				}
				if outcome == "unavailable" {
					backend.err = context.DeadlineExceeded
				}
				e.nodes = map[chain.ID]chain.Backend{chain.BTC: backend, chain.Blake: backend}
				err = e.advanceSwap(context.Background(), s, nil)
				if outcome == "known" {
					if err != nil || s.Stage != "refunding" || len(backend.broadcasts) != 1 || backend.broadcasts[0] != s.SelfRefunds[0] {
						t.Fatalf("known funding did not reach refund: %s %v", s.Stage, err)
					}
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					durable := saved.Swaps[id]
					if (role == "maker" && !durable.ShortSent) || (role == "taker" && !durable.LongSent) || len(saved.Outbox) != 1 {
						t.Fatal("recovered publication or notification not durable")
					}
				} else if err == nil || s.ShortSent || s.LongSent || len(backend.broadcasts) != 0 || len(e.s.Outbox) != 0 {
					t.Fatalf("unobserved funding escaped deadline/error gate: %v", err)
				}
			})
		}
	}
}
