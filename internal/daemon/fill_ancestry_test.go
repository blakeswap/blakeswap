package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFundingAncestryIndexesBothLocalFundingRoles(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, own, _ := preparedFundingLookupFixture(t, role)
			key := "funding/" + string(own.Chain) + "/" + own.TxID
			if e.s.FillKeys[key] != s.ID {
				t.Fatal("durably prepared local funding has no exact ancestry ownership index")
			}
		})
	}
}

// Synthetic chains retain real wallet-signed funding and contract spends. The
// injected metadata supplies canonical inclusion; these are not actual-node tests.
type ancestryBackend struct {
	chain.Backend
	height       uint32
	generation   uint64
	salt         uint32
	transactions map[string]chain.Transaction
	errors       map[string]error
	outputs      map[string]*chain.TxOut
	calls        int
	broadcasts   []string
	onRead       func()
	onHash       func(uint32)
}

func (b *ancestryBackend) Generation() uint64                     { return b.generation }
func (b *ancestryBackend) Height(context.Context) (uint32, error) { return b.height, nil }
func (b *ancestryBackend) BlockHash(_ context.Context, h uint32) (string, error) {
	if b.onHash != nil {
		b.onHash(h)
	}
	return fmt.Sprintf("%064x", h+b.salt), nil
}
func (b *ancestryBackend) Transaction(_ context.Context, id string) (chain.Transaction, error) {
	b.calls++
	if b.onRead != nil {
		b.onRead()
	}
	if err := b.errors[id]; err != nil {
		return chain.Transaction{}, err
	}
	if tx, ok := b.transactions[id]; ok {
		return tx, nil
	}
	return chain.Transaction{}, &chain.RPCError{Code: -5, Message: "not found"}
}
func (b *ancestryBackend) Output(_ context.Context, id string, v uint32) (*chain.TxOut, error) {
	return b.outputs[chain.OutpointKey(id, v)], nil
}
func (b *ancestryBackend) Broadcast(_ context.Context, raw string) (string, error) {
	b.broadcasts = append(b.broadcasts, raw)
	tx, err := contract.Parse(raw)
	if err != nil {
		return "", err
	}
	return tx.TxHash().String(), nil
}

func ancestryChild(t *testing.T, e *Engine, role string, sell chain.ID, input chain.UTXO, pending ...bool) (*Swap, chain.UTXO) {
	t.Helper()
	maker, taker := e.identity, nostr.Generate()
	if role == "taker" {
		maker, taker = taker, maker
	}
	offer := protocol.Offer{Version: protocol.Version, Network: chain.Regtest, FillPolicy: protocol.FillPolicy{Mode: protocol.FillWhole, Min: 1000000, Max: 1000000}, Revision: 1, Available: 1000000, ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: sell, SellAmount: 1000000, BuyAmount: 1000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	raw, err := offer.PublicJSON()
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", chain.Regtest.Namespace()}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	id := transport.RandomID()
	local, err := e.swapKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := e.swapKeys(isolatedPeerID(id))
	if err != nil {
		t.Fatal(err)
	}
	mk, tk := local, peer
	if role == "taker" {
		mk, tk = peer, local
	}
	secret := sha256.Sum256([]byte(transport.RandomID()))
	hash := sha256.Sum256(secret[:])
	request := protocol.Request{Version: protocol.Version, Revision: 1, Quantity: 1000000, ID: id, OfferEvent: event, Taker: taker.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: tk}
	terms, err := protocol.NewTerms(request, mk, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100})
	if err != nil {
		t.Fatal(err)
	}
	s := &Swap{ID: id, Role: role, Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, Receipts: map[string]protocol.Receipt{}, Secret: hex.EncodeToString(secret[:]), LongSent: true, ShortSent: true}
	if len(pending) > 0 && pending[0] {
		if role == "maker" {
			s.ShortSent = false
		} else {
			s.LongSent = false
		}
	}
	own, incoming := &s.Short, &s.Long
	if role == "taker" {
		own, incoming = &s.Long, &s.Short
	}
	keys := map[string]*btcec.PrivateKey{}
	for _, r := range e.receiveBook[own.Chain] {
		keys[hex.EncodeToString(r.script)] = r.key
	}
	tx, err := contract.FundWithKeys(*own, []chain.UTXO{input}, keys, e.scripts[own.Chain], 2000)
	if err != nil {
		t.Fatal(err)
	}
	own.TxID = tx.TxHash().String()
	if role == "maker" {
		s.ShortFunding = contract.Hex(tx)
	} else {
		s.LongFunding = contract.Hex(tx)
	}
	peerTx := wire.NewMsgTx(2)
	peerTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
	pk, _ := incoming.PkScript()
	peerTx.AddTxOut(wire.NewTxOut(incoming.Amount, pk))
	incoming.TxID = peerTx.TxHash().String()
	if backend, ok := e.nodes[incoming.Chain].(*ancestryBackend); ok {
		backend.transactions[incoming.TxID] = chain.Transaction{TxID: incoming.TxID, Hex: contract.Hex(peerTx), Confirmations: 200}
		out := &chain.TxOut{Value: chain.Coins(incoming.Amount), Confirmations: 200}
		out.Script.Hex = hex.EncodeToString(pk)
		backend.outputs[chain.OutpointKey(incoming.TxID, incoming.Vout)] = out
	}
	e.s.Swaps[id] = s
	if e.s.FundingFees == nil {
		e.s.FundingFees = map[string]FeeSelection{}
	}
	policy := FeeSelection{FundingFee: 2000}
	e.s.FundingFees["swap/"+id] = policy
	point := CoinOutpoint{TxID: input.TxID, Vout: input.Vout}
	if e.s.CoinReservations == nil {
		e.s.CoinReservations = map[string]CoinReservation{}
	}
	e.s.CoinReservations["swap/"+id] = CoinReservation{Chain: own.Chain, Inputs: []CoinOutpoint{point}}
	if role == "maker" {
		fields := FillOrderFields{FillPolicy: offer.FillPolicy, FeeBudgets: map[chain.ID]int64{sell: 22000, sell.Other(): 2000}, BountyBudgets: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}}
		p, err := newParentOrder(offer, policy, fields, time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		p.SignedRevision, p.LastSignedAt = 1, int64(event.CreatedAt)
		reserved, f, err := p.reserveFill(request)
		if err != nil {
			t.Fatal(err)
		}
		f.Inputs = []CoinOutpoint{point}
		committed, next, err := reserved.transitionFill(*f, FillCommitted, false)
		if err != nil {
			t.Fatal(err)
		}
		if e.s.ParentOrders == nil {
			e.s.ParentOrders = map[string]*ParentOrder{}
		}
		if e.s.FillRecords == nil {
			e.s.FillRecords = map[string]*FillRecord{}
		}
		e.s.ParentOrders[offer.ID] = &committed
		e.s.FillRecords[id] = &next
		e.s.Offers[offer.ID] = event
	}
	if err := e.prepare(s, *own); err != nil {
		t.Fatal(err)
	}
	if len(tx.TxOut) != 2 {
		t.Fatal("fixture lacks exact change")
	}
	return s, chain.UTXO{TxID: own.TxID, Vout: 1, Amount: chain.Coins(tx.TxOut[1].Value), Script: hex.EncodeToString(tx.TxOut[1].PkScript), Confirmations: 200}
}

func ancestryGraph(t *testing.T, role string, sell chain.ID, pending ...bool) (*Engine, []*Swap, map[chain.ID]*ancestryBackend) {
	t.Helper()
	e, _, _ := sendFixture(t)
	identity, err := e.keys.Derive(2, "nostr-identity")
	if err != nil {
		t.Fatal(err)
	}
	e.identity = nostr.SecretKey(identity.Serialize())
	backends := map[chain.ID]*ancestryBackend{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		b := &ancestryBackend{Backend: e.nodes[id], height: 500, generation: 1, transactions: map[string]chain.Transaction{}, errors: map[string]error{}, outputs: map[string]*chain.TxOut{}}
		backends[id] = b
		e.nodes[id] = b
		e.chainFresh[id] = true
		e.chainGeneration[id] = 1
		e.heights[id] = 500
		e.clocks[id] = 500
	}
	ownChain := sell
	if role == "taker" {
		ownChain = sell.Other()
	}
	input := chain.UTXO{TxID: transport.RandomID(), Amount: 12000000, Script: hex.EncodeToString(e.scripts[ownChain]), Confirmations: 200}
	a, next := ancestryChild(t, e, role, sell, input)
	b, next := ancestryChild(t, e, role, sell, next, pending...)
	c, _ := ancestryChild(t, e, role, sell, next)
	input.TxID = transport.RandomID()
	d, _ := ancestryChild(t, e, role, sell, input)
	children := []*Swap{a, b, c, d}
	for _, s := range children {
		own, raw, _ := localFunding(s)
		backend := backends[own.Chain]
		hash, _ := backend.BlockHash(context.Background(), 300)
		backend.transactions[own.TxID] = chain.Transaction{TxID: own.TxID, Hex: raw, Height: 300, BlockHash: hash, Confirmations: 201}
	}
	return e, children, backends
}

func ancestryOutcomes(t *testing.T, e *Engine, children []*Swap) map[chain.ID]map[string]chain.Observation {
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	for _, s := range children {
		for _, c := range []contract.HTLC{s.Long, s.Short} {
			all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, true, nil)
		}
	}
	return all
}

func TestFundingAncestryChainHoldsOnlyDescendantsAndRevalidates(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(role+"/"+string(sell), func(t *testing.T) {
				e, children, nodes := ancestryGraph(t, role, sell)
				a, b, c, d := children[0], children[1], children[2], children[3]
				if len(a.FundingParents) != 0 || len(d.FundingParents) != 0 || len(b.FundingParents) != 1 || b.FundingParents[0].SwapID != a.ID || len(c.FundingParents) != 1 || c.FundingParents[0].SwapID != b.ID {
					t.Fatal("exact direct graph missing")
				}
				all := ancestryOutcomes(t, e, children)
				for _, s := range children {
					if err := e.advanceSwap(context.Background(), s, all); err != nil {
						t.Fatal(err)
					}
				}
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
				identity := protocol.Digest([]any{b.Request, b.Terms, b.ShortFunding, b.LongFunding, b.Secret, b.FundingParents, c.Request, c.Terms, c.ShortFunding, c.LongFunding, c.Secret, c.FundingParents})
				var money string
				if role == "maker" {
					money = protocol.Digest([]any{e.s.ParentOrders[e.s.FillRecords[b.ID].ParentID].Fees, e.s.ParentOrders[e.s.FillRecords[c.ID].ParentID].Fees})
				}
				own, _, _ := localFunding(b)
				node := nodes[own.Chain]
				for _, s := range []*Swap{b, c} {
					target, _, _ := localFunding(s)
					node.errors[target.TxID] = context.DeadlineExceeded
				}
				for _, s := range []*Swap{b, c} {
					if err := e.advanceSwap(context.Background(), s, all); err == nil || !s.FundingAncestryHeld {
						t.Fatal("unknown ancestor view failed to hold", err)
					}
					if s.Stage != "refunded" {
						t.Fatal("outage erased old positive display")
					}
					if e.recoverySwapResolved(s, all) {
						t.Fatal("held ancestry resolved")
					}
				}
				if d.FundingAncestryHeld || a.FundingAncestryHeld {
					t.Fatal("unrelated child held")
				}
				node.salt = 10000
				for _, s := range []*Swap{b, c} {
					if err := e.advanceSwap(context.Background(), s, all); err == nil || !s.FundingAncestryHeld {
						t.Fatal("positive fork failed to hold")
					}
					if role == "maker" && e.s.FillRecords[s.ID].Allocation.Disposition != FillCommitted {
						t.Fatal("fork retained a false final allocation")
					}
				}
				if role == "maker" && money != protocol.Digest([]any{e.s.ParentOrders[e.s.FillRecords[b.ID].ParentID].Fees, e.s.ParentOrders[e.s.FillRecords[c.ID].ParentID].Fees}) {
					t.Fatal("ancestry replenished permanent charges")
				}
				if identity != protocol.Digest([]any{b.Request, b.Terms, b.ShortFunding, b.LongFunding, b.Secret, b.FundingParents, c.Request, c.Terms, c.ShortFunding, c.LongFunding, c.Secret, c.FundingParents}) {
					t.Fatal("ancestry changed immutable child")
				}
				if err := canChangeNetwork(e.s); err == nil {
					t.Fatal("held descendants allowed monitoring to stop")
				}
				for _, s := range []*Swap{b, c} {
					target, _, _ := localFunding(s)
					delete(node.errors, target.TxID)
					tx := node.transactions[target.TxID]
					tx.BlockHash, _ = node.BlockHash(context.Background(), tx.Height)
					node.transactions[target.TxID] = tx
					if err := e.advanceSwap(context.Background(), s, all); err != nil || s.FundingAncestryHeld || !e.recoverySwapResolved(s, all) {
						t.Fatal("positive current child funding did not clear", err)
					}
				}
			})
		}
	}
}

func TestFundingAncestryRejectsUnconfirmedChangedSourceAndMixedTip(t *testing.T) {
	for _, mode := range []string{"missing", "unconfirmed", "outage", "provider", "mixed tip", "wrong transaction", "wrong block", "unknown prior block"} {
		t.Run(mode, func(t *testing.T) {
			e, children, nodes := ancestryGraph(t, "maker", chain.Blake)
			s := children[1]
			own, _, _ := localFunding(s)
			node := nodes[own.Chain]
			if err := e.refreshFundingAncestry(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				delete(node.transactions, own.TxID)
			case "unconfirmed":
				x := node.transactions[own.TxID]
				x.Confirmations = 0
				x.Height = 0
				x.BlockHash = ""
				node.transactions[own.TxID] = x
			case "outage":
				e.chainFresh[own.Chain] = false
			case "provider":
				node.onRead = func() { node.generation++ }
			case "mixed tip":
				node.onRead = func() { node.salt++ }
			case "wrong transaction":
				x := node.transactions[own.TxID]
				other, _, _ := localFunding(children[0])
				x.TxID = other.TxID
				node.transactions[own.TxID] = x
			case "wrong block":
				x := node.transactions[own.TxID]
				x.BlockHash = transport.RandomID()
				node.transactions[own.TxID] = x
			case "unknown prior block":
				node.onHash = func(h uint32) {
					if h == s.FundingAncestryAnchor.Height {
						node.generation++
					}
				}
			}
			if err := e.refreshFundingAncestry(context.Background(), s); err == nil || !s.FundingAncestryHeld || e.fundingAncestryReady(s) {
				t.Fatal("unproven funding ancestry accepted", err)
			}
			var stored State
			if _, err := e.vault.Load(&stored); err != nil || !stored.Swaps[s.ID].FundingAncestryHeld {
				t.Fatal("hold was not durable before proof failure", err)
			}
		})
	}
}

func TestFundingAncestryColdAncestorRestartAndImport(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, children, nodes := ancestryGraph(t, role, chain.Blake)
			a, b := children[0], children[1]
			all := ancestryOutcomes(t, e, children)
			for _, s := range children {
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.stageArchive("swaps", a.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			coldBefore := e.s.Capacity.Archived
			if err := validateSwapFundingParents(&e.s, engineFillReader{e}, b); err != nil {
				t.Fatal(err)
			}
			own, _, _ := localFunding(b)
			nodes[own.Chain].errors[own.TxID] = context.DeadlineExceeded
			if err := e.refreshFundingAncestry(context.Background(), b); err == nil {
				t.Fatal("unknown child funding accepted")
			}
			if e.s.Swaps[a.ID] != nil || e.s.FillKeys[fundingIdentityKey(own.Chain, a.Short.TxID)] != "" && role == "maker" || protocol.Digest(coldBefore) != protocol.Digest(e.s.Capacity.Archived) {
				t.Fatal("ancestry lookup promoted cold parent")
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			e.fundingAncestryProofs = nil
			b = e.s.Swaps[b.ID]
			if !b.FundingAncestryHeld || e.fundingAncestryReady(b) || e.recoverySwapResolved(b, all) {
				t.Fatal("restart converted explanatory state to proof")
			}
			var complete State
			records, stats, err := e.vault.LoadComplete(&complete, 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			if err := complete.ValidateArchiveCheckpoint(stats); err != nil {
				t.Fatal(err)
			}
			complete.Archive = records
			complete, err = CompleteState(complete)
			if err != nil {
				t.Fatal(err)
			}
			immutable := protocol.Digest([]any{complete.Swaps[b.ID].FundingParents, complete.Swaps[b.ID].Request, complete.Swaps[b.ID].Terms, complete.Swaps[b.ID].LongFunding, complete.Swaps[b.ID].ShortFunding, complete.Swaps[b.ID].Secret})
			if err := PrepareRecovery(&complete, time.Now().Unix(), false); err != nil {
				t.Fatal(err)
			}
			if immutable != protocol.Digest([]any{complete.Swaps[b.ID].FundingParents, complete.Swaps[b.ID].Request, complete.Swaps[b.ID].Terms, complete.Swaps[b.ID].LongFunding, complete.Swaps[b.ID].ShortFunding, complete.Swaps[b.ID].Secret}) || !complete.Swaps[b.ID].FundingAncestryHeld {
				t.Fatal("import lost exact ancestry/hold")
			}
			delete(nodes[own.Chain].errors, own.TxID)
			if err := e.refreshFundingAncestry(context.Background(), b); err != nil || b.FundingAncestryHeld {
				t.Fatal("cold ancestry could not revalidate", err)
			}
			if e.s.Swaps[a.ID] != nil {
				t.Fatal("positive proof activated cold ancestor")
			}

		})
	}
}

func TestFundingAncestryPendingPublicationAndExactRetryStayLive(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(role+"/"+string(sell), func(t *testing.T) {
				e, children, nodes := ancestryGraph(t, role, sell, true)
				s := children[1]
				own, raw, _ := localFunding(s)
				node := nodes[own.Chain]
				delete(node.transactions, own.TxID)
				// The pending publication is still within the original agreed funding window.
				e.clocks = map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100}
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				identity := fundingCustodyHash(s)
				money := protocol.Digest(e.s.FillRecords[s.ID])
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal("initial exact funding deadlocked on its own confirmation", err)
				}
				_, _, sent := localFunding(s)
				if !sent || len(node.broadcasts) != 1 || node.broadcasts[0] != raw || s.SelfClaim != "" || s.SecretObserved || !s.FundingAncestryHeld {
					t.Fatal("initial publication changed authority or cleared hold")
				}
				if err := e.advanceSwap(context.Background(), s, all); err == nil {
					t.Fatal("unconfirmed funding became positive settlement proof")
				}
				if len(node.broadcasts) != 2 || node.broadcasts[1] != raw || s.SelfClaim != "" || s.RefundLastAttempt != 0 || fundingCustodyHash(s) != identity || protocol.Digest(e.s.FillRecords[s.ID]) != money {
					t.Fatal("exact retry changed fees, quantity or private settlement authority")
				}
				node.errors[own.TxID] = chain.ErrTransactionUnobserved
				if err := e.advanceSwap(context.Background(), s, all); err == nil || len(node.broadcasts) != 2 {
					t.Fatal("unknown publication was treated as absent")
				}
			})
		}
	}
}

func TestFundingAncestryHoldPreservesPublicSecretRescue(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			t.Run(role+"/"+string(sell), func(t *testing.T) {
				e, children, nodes := ancestryGraph(t, role, sell)
				s := children[1]
				own, _, _ := localFunding(s)
				incoming := s.Long
				if role == "taker" {
					incoming = s.Short
				}
				nodes[own.Chain].errors[own.TxID] = context.DeadlineExceeded
				secret, _ := hex.DecodeString(s.Secret)
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				all[own.Chain][chain.OutpointKey(own.TxID, own.Vout)] = recoverySpend(t, e, s, own, false, secret)
				if err := e.advanceSwap(context.Background(), s, all); err == nil {
					t.Fatal("ancestry uncertainty was not reported")
				}
				if !s.SecretObserved || !s.FundingAncestryHeld || s.SelfClaim == "" || len(nodes[incoming.Chain].broadcasts) != 1 || nodes[incoming.Chain].broadcasts[0] != s.SelfClaim {
					t.Fatal("funding ancestry held the already public secret rescue")
				}
				if len(nodes[own.Chain].broadcasts) != 0 || s.RefundLastAttempt != 0 {
					t.Fatal("ancestry hold authorized another spend")
				}
			})
		}
	}
}

func TestFundingAncestryCompleteCheckpointRejectsMissingOrForgedEdges(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, mode := range []string{"missing edge", "wrong child", "wrong output", "duplicate edge", "missing index", "wrong index", "extra index"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				e, children, _ := ancestryGraph(t, role, chain.Blake)
				a, b, d := children[0], children[1], children[3]
				own, _, _ := localFunding(a)
				switch mode {
				case "missing edge":
					b.FundingParents = nil
					b.FundingAncestryHeld = false
				case "wrong child":
					b.FundingParents[0].SwapID = d.ID
				case "wrong output":
					b.FundingParents[0].Vout = own.Vout
				case "duplicate edge":
					b.FundingParents = append(b.FundingParents, b.FundingParents[0])
				case "missing index":
					delete(e.s.FillKeys, fundingIdentityKey(own.Chain, own.TxID))
				case "wrong index":
					e.s.FillKeys[fundingIdentityKey(own.Chain, own.TxID)] = d.ID
				case "extra index":
					e.s.FillKeys[fundingIdentityKey(own.Chain, transport.RandomID())] = a.ID
				}
				if err := ValidateCompleteFillState(&e.s); err == nil {
					t.Fatal("incomplete or forged ancestry accepted")
				}
			})
		}
	}
}

func TestFundingAncestryCannotRewriteSignedFundingAndDoesNotStarveLaterChild(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, children, nodes := ancestryGraph(t, role, chain.Blake)
			b, c, d := children[1], children[2], children[3]
			own, _, _ := localFunding(b)
			node := nodes[own.Chain]
			node.errors[own.TxID] = context.DeadlineExceeded
			for i := 0; i < 6; i++ {
				if err := e.refreshFundingAncestry(context.Background(), b); err == nil {
					t.Fatal("bad first child accepted")
				}
				if err := e.refreshFundingAncestry(context.Background(), c); err != nil || !e.fundingAncestryReady(c) {
					t.Fatal("later confirmed descendant starved", err)
				}
			}
			before := protocol.Digest(e.s)
			ownD, raw, _ := localFunding(d)
			tx, err := contract.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			tx.LockTime++
			delete(e.s.FillKeys, fundingIdentityKey(ownD.Chain, ownD.TxID))
			if role == "maker" {
				d.ShortFunding = contract.Hex(tx)
				d.Short.TxID = tx.TxHash().String()
			} else {
				d.LongFunding = contract.Hex(tx)
				d.Long.TxID = tx.TxHash().String()
			}
			if err := e.save(); err == nil {
				t.Fatal("saved funding identity could be rewritten")
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil || protocol.Digest(saved) != before {
				t.Fatal("rejected funding rewrite changed committed state", err)
			}
		})
	}
}

func TestFundingAncestryArchiveRequiresCurrentProofAndColdActivationChecksEdges(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, children, _ := ancestryGraph(t, role, chain.BTC)
			all := ancestryOutcomes(t, e, children)
			for asset, rows := range all {
				for key, o := range rows {
					o.Confirmations = 200
					o.Height = 300
					rows[key] = o
				}
				if err := e.refreshArchiveCheckpoint(context.Background(), asset); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range children {
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal(err)
				}
			}
			b := children[1]
			// A new scan has no transferable ancestry proof, even though the old
			// display and old positive hold flag have not yet been refreshed.
			e.fundingAncestryProofs = nil
			if err := e.compactArchive(context.Background(), all, nil); err != nil {
				t.Fatal(err)
			}
			if e.s.Swaps[b.ID] == nil {
				t.Fatal("archived descendant using a preceding scan's funding evidence")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := e.refreshFundingAncestry(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			if err := e.compactArchive(context.Background(), all, nil); err != nil {
				t.Fatal(err)
			}
			if e.s.Swaps[b.ID] != nil {
				t.Fatal("current proven settled descendant did not archive")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			before := protocol.Digest([]any{e.s, e.pendingArchive()})
			e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
				row, ok, err := e.vault.ReadArchive(kind, id)
				if ok && kind == "swaps" && id == b.ID {
					var core Swap
					if er := json.Unmarshal(row.Data, &core); er != nil {
						return row, false, er
					}
					core.FundingParents[0].Vout++
					row.Data, err = json.Marshal(core)
				}
				return row, ok, err
			}
			if ok, err := e.activateArchived("swaps", b.ID); err == nil || ok {
				t.Fatal("promoted cold descendant with forged funding edge")
			}
			if before != protocol.Digest([]any{e.s, e.pendingArchive()}) {
				t.Fatal("failed cold edge validation changed ownership")
			}
			e.archiveRead = nil
			if ok, err := e.activateArchived("swaps", b.ID); err != nil || !ok {
				t.Fatalf("valid cold activation: %v", err)
			}
			if e.s.Swaps[children[0].ID] != nil {
				t.Fatal("activating child also promoted funding ancestor")
			}
			if !e.s.Swaps[b.ID].FundingAncestryHeld || e.fundingAncestryReady(e.s.Swaps[b.ID]) {
				t.Fatal("activation reused prior cold funding evidence")
			}
			raw, _ := json.Marshal(map[string]string{"kind": "swap", "id": b.ID, "expected_wallet": e.Config.Name, "expected_network": string(e.Config.Network.Normalized())})
			detail, err := e.recordDetail(raw)
			if err != nil || !detail.MonitoringRequired || detail.Swap == nil || !strings.Contains(detail.Swap.Error, "funding ancestry") {
				t.Fatalf("held detail omitted current monitoring: %+v %v", detail, err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFundingAncestryReopenStartsHeldAndRejectsForgedColdIndex(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, children, _ := ancestryGraph(t, role, chain.BTC)
			b := children[1]
			if err := e.refreshFundingAncestry(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			for _, s := range children {
				if err := e.stageArchive("swaps", s.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
				t.Fatal("valid cold source", err)
			}
			// This index is not referenced by a signed child. A complete cold traversal
			// must still reject it rather than inspect only keys discovered from cores.
			key := fundingIdentityKey(chain.BTC, protocol.Digest("forged cold funding"))
			data, _ := json.Marshal(b.ID)
			row := storage.ArchiveRecord{Kind: "fill_keys", ID: key, Data: data}
			if err := e.archiveDelta(row, true); err != nil {
				t.Fatal(err)
			}
			if _, err := e.vault.CommitArchive(e.s, storage.ArchiveBatch{Put: []storage.ArchiveRecord{row}}, 0); err != nil {
				t.Fatal(err)
			}
			if err := ValidateVaultProtocolState(e.vault, &e.s); err == nil {
				t.Fatal("completed reader skipped forged cold funding ownership")
			}
			if err := e.archiveDelta(row, false); err != nil {
				t.Fatal(err)
			}
			if _, err := e.vault.CommitArchive(e.s, storage.ArchiveBatch{Delete: []storage.ArchiveKey{{Kind: "fill_keys", ID: key}}}, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := e.activateArchived("swaps", b.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.refreshFundingAncestry(context.Background(), e.s.Swaps[b.ID]); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "state.db")
			if err := e.vault.Backup(path); err != nil {
				t.Fatal(err)
			}
			password := filepath.Join(filepath.Dir(path), "password")
			if err := os.WriteFile(password, []byte("receive-test-password"), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) }))
			defer server.Close()
			config := Config{Network: chain.Regtest, Mode: "trader", Name: "ancestry-reopen", DataDir: filepath.Dir(path), PasswordFile: password, Relays: []string{"ws://127.0.0.1:1"}, Nodes: map[chain.ID]NodeConfig{chain.BTC: {URL: server.URL}, chain.Blake: {URL: server.URL}}}
			reopened, err := Open(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			actual := reopened.s.Swaps[b.ID]
			if actual == nil || !actual.FundingAncestryHeld || reopened.fundingAncestryReady(actual) {
				t.Fatal("reopen reused saved positive ancestry evidence")
			}
			if fundingCustodyHash(actual) != fundingCustodyHash(b) || actual.Secret != b.Secret {
				t.Fatal("reopen changed immutable custody")
			}
			if reopened.s.Swaps[children[0].ID] != nil {
				t.Fatal("reopen reactivated cold funding ancestor")
			}
			if err := canChangeNetwork(reopened.s); err == nil {
				t.Fatal("offline reopened ancestry hold allowed network switch")
			}
			var saved State
			if _, err := reopened.vault.Load(&saved); err != nil || !saved.Swaps[b.ID].FundingAncestryHeld {
				t.Fatal("reopen hold not durable", err)
			}
		})
	}
}
