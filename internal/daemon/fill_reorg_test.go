package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Real acceptance and local funding use disjoint assigned inputs. Peer funding
// and current chain observations are explicit disposable signed fixtures; these
// controls do not claim actual-node inclusion evidence.
func fundedFillPair(t *testing.T, sell chain.ID, beforeFunding ...func(*Engine)) (*Engine, []*Swap, [][]byte) {
	t.Helper()
	e, maker, now := fillAdmissionEngine(t, sell)
	e.s.Seen = map[string]string{}
	var children []*Swap
	var secrets [][]byte
	for index, quantity := range []int64{400000, 600000} {
		if index != 0 {
			p := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
			next, event, err := e.prepareParentPublication(*p, now+1)
			if err != nil || event == nil {
				t.Fatal("publication", err)
			}
			*p = next
			e.stageOffer(parentPublicOffer(next), *event)
		}
		r := admissionRequest(t, e, maker, quantity)
		peer := nostr.Generate()
		r.Taker = peer.Public().Hex()
		keys, err := e.swapKeys(isolatedPeerID(r.ID))
		if err != nil {
			t.Fatal(err)
		}
		r.Keys = keys
		secret := sha256.Sum256([]byte(transport.RandomID()))
		hash := sha256.Sum256(secret[:])
		r.Hash = hex.EncodeToString(hash[:])
		if err := applyFillRequest(t, e, r, now+int64(index)); err != nil {
			t.Fatal(err)
		}
		s := e.s.Swaps[r.ID]
		if s == nil {
			t.Fatal("child not accepted")
		}
		funding := wire.NewMsgTx(2)
		point, _ := chain.WireOutpoint(transport.RandomID(), 0)
		funding.AddTxIn(wire.NewTxIn(&point, nil, nil))
		pk, err := s.Long.PkScript()
		if err != nil {
			t.Fatal(err)
		}
		funding.AddTxOut(wire.NewTxOut(s.Long.Amount, pk))
		body, _ := json.Marshal(fundingMessage{TermsHash: protocol.Digest(s.Terms), Raw: contract.Hex(funding)})
		event, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), transport.Message{Version: transport.MessageVersion, ID: transport.RandomID(), Type: "long-funded", SwapID: s.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.receive(event); err != nil {
			t.Fatal(err)
		}
		children = append(children, s)
		secrets = append(secrets, secret[:])
	}
	for _, capture := range beforeFunding {
		capture(e)
	}
	b := &fundsBackend{outputs: map[string]*chain.TxOut{}}
	for _, coin := range e.knownCoins(sell) {
		out := &chain.TxOut{Value: coin.Amount, Confirmations: coin.Confirmations}
		out.Script.Hex = coin.Script
		b.outputs[chain.OutpointKey(coin.TxID, coin.Vout)] = out
	}
	e.nodes[sell] = b
	for _, s := range children {
		tx, err := e.fundReserved(context.Background(), s.Short, "swap/"+s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.commitMakerFill(s, tx); err != nil {
			t.Fatal(err)
		}
		s.ShortFunding, s.Short.TxID = contract.Hex(tx), tx.TxHash().String()
		if err := e.prepare(s, s.Short); err != nil {
			t.Fatal(err)
		}
		s.ShortSent = true
	}
	e.nodes[sell] = &fundingLookupBackend{}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, children, secrets
}

func fillPairOutcomes(t *testing.T, e *Engine, children []*Swap, secrets [][]byte, firstRefund bool) map[chain.ID]map[string]chain.Observation {
	t.Helper()
	all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	for i, s := range children {
		refund := firstRefund
		if i != 0 {
			refund = !refund
		}
		for _, c := range []contract.HTLC{s.Long, s.Short} {
			all[c.Chain][chain.OutpointKey(c.TxID, c.Vout)] = recoverySpend(t, e, s, c, refund, secrets[i])
		}
	}
	return all
}

func TestParentFillReorgDemotesOnlyContradictedChildBeforeLookupFailure(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		for _, refund := range []bool{false, true} {
			for _, change := range []string{"missing", "depth", "outage", "incomplete"} {
				name := string(sell) + "/claim/" + change
				if refund {
					name = string(sell) + "/refund/" + change
				}
				t.Run(name, func(t *testing.T) {
					e, children, secrets := fundedFillPair(t, sell)
					all := fillPairOutcomes(t, e, children, secrets, refund)
					for _, s := range children {
						if err := e.advanceSwap(context.Background(), s, all); err != nil {
							t.Fatal(err)
						}
					}
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					first, sibling := children[0], children[1]
					f := e.s.FillRecords[first.ID]
					p := e.s.ParentOrders[f.ParentID]
					original, siblingDigest := f.Allocation, protocol.Digest(e.s.FillRecords[sibling.ID])
					money := protocol.Digest([]any{p.Fees, p.Bounties})
					identity := protocol.Digest([]any{first.Request, first.Terms, first.LongFunding, first.ShortFunding, f.Inputs, first.Secret, first.SecretObserved})
					target := first.Long
					point := chain.OutpointKey(target.TxID, target.Vout)
					switch change {
					case "missing":
						delete(all[target.Chain], point)
					case "depth":
						obs := all[target.Chain][point]
						obs.Confirmations = 0
						all[target.Chain][point] = obs
					case "outage":
						e.chainFresh[target.Chain] = false
						delete(all, target.Chain)
					case "incomplete":
						all[target.Chain] = nil
					}
					e.nodes[sell] = &fundingLookupBackend{err: context.DeadlineExceeded}
					_ = e.advanceSwap(context.Background(), first, all)
					contradicted := change == "missing" || change == "depth"
					if contradicted && (f.Allocation.Disposition != FillCommitted || p.Quantities.Committed != 400000 || terminalSwapStage(first.Stage)) {
						t.Fatal("fresh child contradiction did not demote before unrelated lookup", f.Allocation, p.Quantities, first.Stage)
					}
					if !contradicted && f.Allocation != original {
						t.Fatal("unknown observation changed allocation")
					}
					if protocol.Digest(e.s.FillRecords[sibling.ID]) != siblingDigest || protocol.Digest([]any{p.Fees, p.Bounties}) != money || protocol.Digest([]any{first.Request, first.Terms, first.LongFunding, first.ShortFunding, f.Inputs, first.Secret, first.SecretObserved}) != identity {
						t.Fatal("reorg changed sibling, consumed charges or immutable child knowledge")
					}
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					if saved.FillRecords[first.ID].Allocation != f.Allocation {
						t.Fatal("demotion was not durable before lookup failure")
					}
					e.chainFresh[target.Chain] = true
					e.nodes[sell] = &fundingLookupBackend{}
					all = fillPairOutcomes(t, e, children, secrets, refund)
					if err := e.advanceSwap(context.Background(), first, all); err != nil {
						t.Fatal(err)
					}
					if f.Allocation != original || protocol.Digest([]any{p.Fees, p.Bounties}) != money {
						t.Fatal("fresh repair changed conserved amount or charged twice")
					}
				})
			}
		}
	}
}

func TestParentFillRestoredPositiveOutcomesUpdateHeldLedger(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, sell)
			markRestored(t, e)
			p := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
			money := protocol.Digest([]any{p.Fees, p.Bounties})
			all := fillPairOutcomes(t, e, children, secrets, false)
			for _, s := range children {
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal(err)
				}
			}
			if p.Quantities.Filled != 400000 || p.Quantities.Released != 600000 || p.Quantities.Committed != 0 {
				t.Fatal("positive restored settlement left accounting committed", p.Quantities)
			}
			if !p.RestoreHold || len(e.s.Offers) != 0 || protocol.Digest([]any{p.Fees, p.Bounties}) != money {
				t.Fatal("restored proof resumed grant or credited charges")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := e.stageArchive("swaps", children[0].ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if _, err := e.activateArchived("swaps", children[0].ID); err != nil {
				t.Fatal(err)
			}
			s := e.s.Swaps[children[0].ID]
			e.nodes[s.Long.Chain] = &fundingLookupBackend{}
			delete(all[s.Long.Chain], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
			_ = e.advanceSwap(context.Background(), s, all)
			if p.Quantities.Committed != 400000 || p.Quantities.Released != 600000 || protocol.Digest([]any{p.Fees, p.Bounties}) != money || !p.RestoreHold {
				t.Fatal("restored cold reorg changed sibling/charges or failed to demote", p.Quantities)
			}
		})
	}
}

func TestParentFillOutcomeRefusesInvalidProofAndChangedSource(t *testing.T) {
	e, children, secrets := fundedFillPair(t, chain.BTC)
	all := fillPairOutcomes(t, e, children, secrets, false)
	s := children[0]
	for _, child := range children {
		if err := e.advanceSwap(context.Background(), child, all); err != nil {
			t.Fatal(err)
		}
	}
	point := chain.OutpointKey(s.Long.TxID, s.Long.Vout)
	original := all[s.Long.Chain][point]
	for _, problem := range []string{"missing transaction", "wrong transaction", "invalid signature"} {
		before := protocol.Digest(e.s)
		obs := original
		obs.Confirmations = 0
		switch problem {
		case "missing transaction":
			obs.Tx = nil
		case "wrong transaction":
			obs.TxID = protocol.Digest("wrong transaction")
		case "invalid signature":
			obs.Tx = obs.Tx.Copy()
			obs.Tx.TxIn[0].Witness[0] = []byte{1, 2}
		}
		all[s.Long.Chain][point] = obs
		if err := e.reconcileFillContradiction(s, all); err == nil || protocol.Digest(e.s) != before {
			t.Fatal("invalid evidence changed accounting", problem, err)
		}
	}
	all[s.Long.Chain][point] = original
	pool, err := chain.NewFailover(chain.Regtest, s.Long.Chain, chain.Endpoint{Kind: "rpc", URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	previousSource := e.nodes[s.Long.Chain]
	e.nodes[s.Long.Chain] = pool
	e.chainGeneration[s.Long.Chain] = pool.Generation() + 1 // No requests; mismatch means this scan is stale.
	before := protocol.Digest(e.s)
	delete(all[s.Long.Chain], point)
	if err := e.reconcileFillContradiction(s, all); err != nil || protocol.Digest(e.s) != before {
		t.Fatal("old source scan changed accounting", err)
	}
	e.nodes[s.Long.Chain] = previousSource
	otherPoint := chain.OutpointKey(s.Short.TxID, s.Short.Vout)
	bad := all[s.Short.Chain][otherPoint]
	bad.Tx = nil
	all[s.Short.Chain][otherPoint] = bad
	if err := e.reconcileFillContradiction(s, all); err == nil || e.s.FillRecords[s.ID].Allocation.Disposition != FillCommitted {
		t.Fatal("unrelated malformed leg suppressed a positive contradiction", err)
	}
	var saved State
	if _, err := e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.FillRecords[s.ID].Allocation.Disposition != FillCommitted {
		t.Fatal("positive contradiction was not saved before reporting other-leg failure")
	}
}

type fillOutcomeLookup struct {
	*fundingLookupBackend
	calls int
}

func (b *fillOutcomeLookup) Transaction(ctx context.Context, id string) (chain.Transaction, error) {
	b.calls++
	return b.fundingLookupBackend.Transaction(ctx, id)
}

func TestParentFillFailedDemotionSaveStopsIOAndReopensOneCheckpoint(t *testing.T) {
	e, children, secrets := fundedFillPair(t, chain.Blake)
	all := fillPairOutcomes(t, e, children, secrets, false)
	for _, child := range children {
		if err := e.advanceSwap(context.Background(), child, all); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	s := children[0]
	parentID := e.s.FillRecords[s.ID].ParentID
	before := protocol.Digest([]any{e.s.ParentOrders[parentID].Quantities, e.s.ParentOrders[parentID].Fees, e.s.FillRecords})
	point := chain.OutpointKey(s.Long.TxID, s.Long.Vout)
	delete(all[s.Long.Chain], point)
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	lookup := &fillOutcomeLookup{fundingLookupBackend: &fundingLookupBackend{err: context.DeadlineExceeded}}
	e.nodes[s.Short.Chain] = lookup
	if err := e.advanceSwap(context.Background(), s, all); err == nil || e.fatal == nil || lookup.calls != 0 {
		t.Fatal("failed demotion save reached later IO", err, lookup.calls)
	}
	v, err := storage.OpenExisting(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var saved State
	if _, err := v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if protocol.Digest([]any{saved.ParentOrders[parentID].Quantities, saved.ParentOrders[parentID].Fees, saved.FillRecords}) != before {
		t.Fatal("failed commit split parent/child/charge checkpoint")
	}
	e.vault, e.s, e.fatal = v, saved, nil
	s = e.s.Swaps[s.ID]
	if err := e.advanceSwap(context.Background(), s, all); err == nil || lookup.calls != 1 {
		t.Fatal("retry did not reach unchanged injected lookup error", err, lookup.calls)
	}
	if _, err := v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.FillRecords[s.ID].Allocation.Disposition != FillCommitted || saved.ParentOrders[parentID].Quantities.Committed != 400000 || saved.ParentOrders[parentID].Quantities.Released != 600000 {
		t.Fatal("retry did not commit the exact child demotion")
	}
}

func TestParentFillRestoredPrecommitChargesOnlyOnPositiveOwnProof(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			var prior []byte
			e, children, secrets := fundedFillPair(t, sell, func(e *Engine) {
				var err error
				prior, err = json.Marshal(e.s)
				if err != nil {
					t.Fatal(err)
				}
			})
			all := fillPairOutcomes(t, e, children, secrets, false)
			// This models a pre-commit export followed by independently obtained exact
			// public funding/spend evidence. It does not claim a new discovery transport.
			var restored State
			if err := json.Unmarshal(prior, &restored); err != nil {
				t.Fatal(err)
			}
			for _, later := range children {
				earlier := restored.Swaps[later.ID]
				bound, err := bindFunding(earlier.Terms.Short, later.ShortFunding)
				if err != nil {
					t.Fatal(err)
				}
				earlier.Short = bound // Public outpoint knowledge, no saved own signing permission.
			}
			// Install the exported checkpoint in an independent private vault;
			// rewinding a live funded Engine would erase permanent local charges.
			e = conservationRestoredEngine(t, e, restored)
			markRestored(t, e)
			p := e.s.ParentOrders[e.s.FillRecords[children[0].ID].ParentID]
			for _, problem := range []string{"absence", "incomplete", "mempool", "bad signature"} {
				s := e.s.Swaps[children[0].ID]
				f := e.s.FillRecords[s.ID]
				point := chain.OutpointKey(s.Short.TxID, s.Short.Vout)
				good := all[s.Short.Chain][point]
				bad := good
				switch problem {
				case "absence":
					delete(all[s.Short.Chain], point)
				case "incomplete":
					all[s.Short.Chain] = nil
				case "mempool":
					bad.Confirmations = 0
					all[s.Short.Chain][point] = bad
				case "bad signature":
					bad.Tx = bad.Tx.Copy()
					bad.Tx.TxIn[0].Witness[0] = []byte{1, 2}
					all[s.Short.Chain][point] = bad
				}
				before := protocol.Digest(e.s)
				if err := e.settleRestoredMakerFill(s, FillFilled, all); err == nil || protocol.Digest(e.s) != before || f.Allocation.Disposition != FillReserved {
					t.Fatal("uncertain evidence charged or released imported quantity", problem, err)
				}
				all = fillPairOutcomes(t, e, children, secrets, false)
			}
			for i, later := range children {
				s := e.s.Swaps[later.ID]
				want := FillFilled
				if i != 0 {
					want = FillReleased
				}
				if err := e.advanceSwap(context.Background(), s, all); err != nil || e.s.FillRecords[s.ID].Allocation.Disposition != want {
					t.Fatal("current proof did not account for exercised imported grant", err)
				}
				if s.ShortFunding != "" || s.ShortSent {
					t.Fatal("public proof invented local funding bytes or publication permission")
				}
			}
			if p.Fees[sell].Consumed != 53000 || p.Fees[sell].Reserved != 0 || p.Quantities.Filled != 400000 || p.Quantities.Released != 600000 || !p.RestoreHold {
				t.Fatal("imported proof changed quantity or permanent fee accounting", p)
			}
			before := protocol.Digest([]any{p.Quantities, p.Fees, p.Bounties, e.s.FillRecords})
			for _, later := range children {
				if err := e.advanceSwap(context.Background(), e.s.Swaps[later.ID], all); err != nil {
					t.Fatal(err)
				}
			}
			if protocol.Digest([]any{p.Quantities, p.Fees, p.Bounties, e.s.FillRecords}) != before {
				t.Fatal("same proof consumed imported authorization twice")
			}
		})
	}
}

func TestReviewFillAcceptsConsensusValidPeerSequence(t *testing.T) {
	for _, restored := range []bool{false, true} {
		name := "ordinary"
		if restored {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, chain.BTC)
			s := children[0]
			all := fillPairOutcomes(t, e, children, secrets, false)
			target := s.Short // BTC own contract, claimed by the counterparty.
			point := chain.OutpointKey(target.TxID, target.Vout)
			obs := all[target.Chain][point]
			tx := obs.Tx.Copy()
			tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 1
			script, err := target.Script()
			if err != nil {
				t.Fatal(err)
			}
			pk, err := target.PkScript()
			if err != nil {
				t.Fatal(err)
			}
			key := isolatedSpendKey(t, e, s, target, false)
			spent := wire.NewTxOut(target.Amount, pk)
			digest, err := contract.Digest(target.Chain, tx, 0, script, []*wire.TxOut{spent})
			if err != nil {
				t.Fatal(err)
			}
			tx.TxIn[0].Witness[0] = append(ecdsa.Sign(key, digest).Serialize(), byte(txscript.SigHashAll))
			fetch := txscript.NewCannedPrevOutputFetcher(pk, target.Amount)
			hashes := txscript.NewTxSigHashes(tx, fetch)
			vm, err := txscript.NewEngine(pk, tx, 0, txscript.StandardVerifyFlags, nil, hashes, target.Amount, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if err = vm.Execute(); err != nil {
				t.Fatal("fixture is not consensus-valid", err)
			}
			obs.Tx, obs.TxID = tx, tx.TxHash().String()
			all[target.Chain][point] = obs
			if restored {
				markRestored(t, e)
			}
			err = e.advanceSwap(context.Background(), s, all)
			if err != nil || s.Stage != "completed" || e.s.FillRecords[s.ID].Allocation.Disposition != FillFilled {
				t.Fatalf("valid confirmed spend permanently rejected: err=%v stage=%q allocation=%s", err, s.Stage, e.s.FillRecords[s.ID].Allocation.Disposition)
			}
		})
	}
}

func TestReviewPeerSequenceCannotSuppressMakerRescue(t *testing.T) {
	for _, restored := range []bool{false} {
		name := "ordinary"
		if restored {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, chain.BTC)
			s := children[0]
			all := fillPairOutcomes(t, e, children, secrets, false)
			target := s.Short // BTC own contract, claimed by the counterparty.
			point := chain.OutpointKey(target.TxID, target.Vout)
			obs := all[target.Chain][point]
			tx := obs.Tx.Copy()
			tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 1
			script, err := target.Script()
			if err != nil {
				t.Fatal(err)
			}
			pk, err := target.PkScript()
			if err != nil {
				t.Fatal(err)
			}
			key := isolatedSpendKey(t, e, s, target, false)
			spent := wire.NewTxOut(target.Amount, pk)
			digest, err := contract.Digest(target.Chain, tx, 0, script, []*wire.TxOut{spent})
			if err != nil {
				t.Fatal(err)
			}
			tx.TxIn[0].Witness[0] = append(ecdsa.Sign(key, digest).Serialize(), byte(txscript.SigHashAll))
			fetch := txscript.NewCannedPrevOutputFetcher(pk, target.Amount)
			hashes := txscript.NewTxSigHashes(tx, fetch)
			vm, err := txscript.NewEngine(pk, tx, 0, txscript.StandardVerifyFlags, nil, hashes, target.Amount, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if err = vm.Execute(); err != nil {
				t.Fatal("fixture is not consensus-valid", err)
			}
			obs.Tx, obs.TxID = tx, tx.TxHash().String()
			all[target.Chain][point] = obs
			delete(all[s.Long.Chain], chain.OutpointKey(s.Long.TxID, s.Long.Vout))
			backend := &fundingLookupBackend{}
			e.nodes[s.Long.Chain] = backend
			if restored {
				markRestored(t, e)
			}
			err = e.advanceSwap(context.Background(), s, all)
			if err != nil || s.SelfClaim == "" || len(backend.broadcasts) == 0 {
				t.Fatalf("peer claim suppressed maker rescue: err=%v secret_observed=%v claim_saved=%v broadcasts=%d", err, s.SecretObserved, s.SelfClaim != "", len(backend.broadcasts))
			}
		})
	}
}
