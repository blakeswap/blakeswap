package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
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
				offer := h.command("maker", "offer.create", walletWholeParams(chain.BTC, 1000000, 2000000, 2000, 0, 0)).(protocol.Offer)
				h.tick("maker", "taker")
				id := h.command("taker", "swap.take", map[string]any{"maker": offer.Maker, "id": offer.ID, "quantity": offer.SellAmount, "parent_revision": offer.Revision}).(map[string]string)["id"]
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

// Capture the actual durable signing checkpoint before publication is authorized.
// The backup is independent: later publication must never rewind the live engine.
func capturePreparedFundingSnapshot(t *testing.T, e *Engine, s *Swap, path string) {
	t.Helper()
	_, raw, sent := localFunding(s)
	if raw == "" || sent || len(s.SelfRefunds) != len(protocol.RescueFees) {
		t.Fatal("snapshot is not a complete prepared-but-unpublished checkpoint")
	}
	kind := "long-funded"
	if s.Role == "maker" {
		kind = "short-funded"
	}
	for _, d := range e.s.Outbox {
		if d.Type == kind {
			t.Fatal("snapshot already contains its funding publication")
		}
	}
	var durable State
	if _, err := e.vault.Load(&durable); err != nil {
		t.Fatal(err)
	}
	saved := durable.Swaps[s.ID]
	if saved == nil || protocol.Digest(saved) != protocol.Digest(s) {
		t.Fatal("prepared funding is not the exact durable signing checkpoint")
	}
	if err := e.vault.Backup(path); err != nil {
		t.Fatal(err)
	}
}

func TestRealPreparedFundingSnapshotRecoversAfterDeadline(t *testing.T) {
	for _, role := range []string{"taker", "maker"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, 0)
			o := h.command("maker", "offer.create", walletWholeParams(chain.BTC, 1000000, 2000000, 2000, 0, 0)).(protocol.Offer)
			h.tick("maker", "taker")
			id := h.command("taker", "swap.take", map[string]any{"maker": o.Maker, "id": o.ID, "quantity": o.SellAmount, "parent_revision": o.Revision}).(map[string]string)["id"]
			h.tick("taker", "maker")
			// Deliver the exact saved acceptance without ticking the taker's
			// funding loop. The relay later delivers the same idempotent event.
			for _, d := range h.engines["maker"].s.Outbox {
				if d.Type == "accepted" {
					if err := h.engines["taker"].receive(d.Event); err != nil {
						t.Fatal(err)
					}
				}
			}
			if h.swap("taker", id).Terms == nil {
				t.Fatal("missing durable accepted terms")
			}
			if role == "maker" {
				h.until("long funding", func() bool { return h.swap("taker", id).LongSent }, func() { h.tick("taker") })
				h.minePending()
				for _, d := range h.engines["taker"].s.Outbox {
					if d.Type == "long-funded" {
						if err := h.engines["maker"].receive(d.Event); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			e, s := h.engines[role], h.swap(role, id)
			own, phase := s.Long, "fund-long"
			if role == "maker" {
				own, phase = s.Short, "fund-short"
			}
			if err := e.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.gate(s.Terms, phase); err != nil {
				t.Fatal(err)
			}
			if role == "maker" {
				if ready, err := e.funded(h.ctx, s.Long); err != nil || !ready {
					t.Fatal("maker preparation requires actual confirmed incoming funding", ready, err)
				}
			}
			// Execute the same signing/commit/prepare operations as advanceSwap,
			// stopping at their real save boundary before recordFunding.
			tx, err := e.fundReserved(h.ctx, own, "swap/"+id)
			if err != nil {
				t.Fatal(err)
			}
			if role == "maker" {
				if err := e.commitMakerFill(s, tx); err != nil {
					t.Fatal(err)
				}
				s.ShortFunding, s.Short.TxID = contract.Hex(tx), tx.TxHash().String()
				own = s.Short
			} else {
				s.LongFunding, s.Long.TxID = contract.Hex(tx), tx.TxHash().String()
				own = s.Long
			}
			if err := e.prepare(s, own); err != nil {
				t.Fatal(err)
			}
			restored := h.configs[role]
			restored.DataDir = t.TempDir()
			restored.Socket = filepath.Join(restored.DataDir, "daemon.sock")
			backupPath := filepath.Join(restored.DataDir, "state.db")
			capturePreparedFundingSnapshot(t, e, s, backupPath)
			captured, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}
			capturedHash := sha256.Sum256(captured)
			clear(captured)
			password, err := os.ReadFile(restored.PasswordFile)
			if err != nil {
				t.Fatal(err)
			}
			restored.PasswordFile = filepath.Join(restored.DataDir, "pass")
			err = os.WriteFile(restored.PasswordFile, password, 0600)
			clear(password)
			if err != nil {
				t.Fatal(err)
			}
			raw := contract.Hex(tx)
			// Both real transactions publish through the ordinary loop. The
			// old captured database remains byte-for-byte untouched throughout.
			h.until("both funding publications", func() bool { return h.swap("taker", id).LongSent && h.swap("maker", id).ShortSent }, func() {
				h.tick("taker")
				h.minePending()
				h.tick("maker")
			})
			h.minePending()
			h.offline("maker")
			h.offline("taker")
			captured, err = os.ReadFile(backupPath)
			if err != nil || sha256.Sum256(captured) != capturedHash {
				t.Fatal("later publication changed the independent prepared checkpoint", err)
			}
			clear(captured)
			h.configs[role] = restored
			h.mine(own.Chain, own.RefundHeight+1-h.height(own.Chain))
			h.online(role)
			h.tick(role)
			recovered := h.swap(role, id)
			_, recoveredRaw, sent := localFunding(recovered)
			if recovered.Stage != "refunding" || !sent || recoveredRaw != raw {
				t.Fatal("prepared funding recovery failed", recovered.Stage, recovered.Error)
			}
			kind := "long-funded"
			if role == "maker" {
				kind = "short-funded"
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
				t.Fatal("prepared snapshot refund did not confirm", h.swap(role, id).Error)
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

// The prepared-but-not-marked-sent checkpoint is created directly, before any
// later publication exists. It is not a rollback of an already charged engine.
// Current accepted terms use distinct local/peer keys, and a maker's signed
// funding/refunds retain their committed allocation and permanent fee charges.
func preparedFundingLookupFixture(t *testing.T, role string) (*Engine, *Swap, contract.HTLC, string) {
	t.Helper()
	e, _, _ := sendFixture(t)
	maker, taker := e.identity, nostr.Generate()
	if role == "taker" {
		maker, taker = taker, maker
	}
	offer := protocol.Offer{Version: protocol.Version, Network: chain.Regtest, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}, Revision: 1, Available: 1000000, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: chain.BTC, SellAmount: 1000000, BuyAmount: 2000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	raw, err := offer.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	id := transport.RandomID()
	localKeys, err := e.swapKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	peerKeys, err := e.swapKeys(isolatedPeerID(id))
	if err != nil {
		t.Fatal(err)
	}
	makerKeys, takerKeys := localKeys, peerKeys
	if role == "taker" {
		makerKeys, takerKeys = peerKeys, localKeys
	}
	secret, _ := hex.DecodeString(transport.RandomID())
	hash := sha256.Sum256(secret)
	request := protocol.Request{Version: protocol.Version, Revision: offer.Revision, Quantity: offer.SellAmount, ID: id, OfferEvent: event, Taker: taker.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: takerKeys}
	terms, err := protocol.NewTerms(request, makerKeys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100})
	if err != nil {
		t.Fatal(err)
	}
	s := &Swap{ID: id, Role: role, Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, Receipts: map[string]protocol.Receipt{}}
	if role == "taker" {
		s.Secret = hex.EncodeToString(secret)
	}
	own := &s.Long
	if role == "maker" {
		own = &s.Short
	}
	// This test supplies synthetic funding inclusion, exactly as before; the
	// output and resulting real signed refund bundle bind the agreed HTLC.
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
	e.s.Swaps[id] = s
	policy := FeeSelection{FundingFee: 2000}
	e.s.FundingFees = map[string]FeeSelection{"swap/" + id: policy}
	point := CoinOutpoint{TxID: tx.TxIn[0].PreviousOutPoint.Hash.String(), Vout: tx.TxIn[0].PreviousOutPoint.Index}
	e.s.CoinReservations = map[string]CoinReservation{"swap/" + id: {Chain: own.Chain, Inputs: []CoinOutpoint{point}}}
	if role == "maker" {
		fields := FillOrderFields{FillPolicy: offer.FillPolicy, FeeBudgets: map[chain.ID]int64{offer.Sell: 22000, offer.Sell.Other(): 2000}, BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}}
		parent, err := newParentOrder(offer, policy, fields, time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		parent.SignedRevision, parent.LastSignedAt = offer.Revision, int64(event.CreatedAt)
		reserved, child, err := parent.reserveFill(request)
		if err != nil {
			t.Fatal(err)
		}
		child.Inputs = []CoinOutpoint{point}
		committed, allocation, err := reserved.transitionFill(*child, FillCommitted, false)
		if err != nil {
			t.Fatal(err)
		}
		e.s.ParentOrders = map[string]*ParentOrder{offer.ID: &committed}
		e.s.FillRecords = map[string]*FillRecord{id: &allocation}
		e.s.Offers[offer.ID] = event
		e.s.OfferTowers = map[string]protocol.Tower{offer.ID: {}}
	}
	if err := e.retainSwapIdentity(s); err != nil {
		t.Fatal(err)
	}
	if err := e.prepare(s, *own); err != nil {
		t.Fatal(err)
	}
	return e, s, *own, funding
}

func TestPreparedFundingReconciliationHonorsDeadlineAndLookupErrors(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, outcome := range []string{"known", "missing", "unavailable", "unobserved"} {
			t.Run(role+"/"+outcome, func(t *testing.T) {
				e, s, own, funding := preparedFundingLookupFixture(t, role)
				id := s.ID
				e.heights = map[chain.ID]uint32{chain.BTC: s.Terms.Short.RefundHeight + 1, chain.Blake: s.Terms.Long.RefundHeight + 1}
				e.clocks = e.heights
				backend := &fundingLookupBackend{tx: chain.Transaction{TxID: own.TxID, Hex: funding}}
				if outcome == "missing" {
					backend.err = &chain.RPCError{Code: -5, Message: "transaction not found"}
				}
				if outcome == "unavailable" {
					backend.err = context.DeadlineExceeded
				}
				if outcome == "unobserved" {
					backend.err = chain.ErrTransactionUnobserved
				}
				e.nodes = map[chain.ID]chain.Backend{chain.BTC: backend, chain.Blake: backend}
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				err := e.advanceSwap(context.Background(), s, all)
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
