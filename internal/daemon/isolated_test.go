package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/wire"
)

type switchingEstimateBackend struct {
	chain.Backend
	id         chain.ID
	onEstimate func()
}

type recoveryClockBackend struct{ chain.Backend }

type callbackRecoveryScanner struct {
	observations map[string]chain.Observation
	beforeReturn func()
}

type blockedTowerScanner struct{ calls int }

func (s *blockedTowerScanner) Scan(ctx context.Context, _ uint32, _ []string) (map[string]chain.Observation, error) {
	s.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

type liveTowerScanner struct{ calls int }

func (s *liveTowerScanner) Scan(ctx context.Context, _ uint32, _ []string) (map[string]chain.Observation, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]chain.Observation{}, nil
}

func TestIsolatedTowerWorkBudgetsKeepHealthyChainProgressing(t *testing.T) {
	e, s, b, secret := isolatedFixture(t, "maker")
	target, observe := s.Long, s.Short
	if target.Chain != chain.Blake || observe.Chain != chain.BTC {
		t.Fatal("fixture ordering changed")
	}
	tower := e.ownTower()
	s.Protection = &tower
	job, err := e.makeJob(s, target, "claim", &observe, s.Terms.Takeover)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Swaps = map[string]*Swap{}
	state := &TowerJob{Job: job, Secret: hex.EncodeToString(secret), FundingSeen: true}
	e.s.TowerJobs = map[string]*TowerJob{job.ID: state}
	b.coins = nil
	e.nodes[chain.BTC] = &recoveryClockBackend{Backend: e.watch[chain.BTC]}
	e.nodes[chain.Blake] = &recoveryClockBackend{Backend: b}
	e.Config.Relays = nil
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	blocked, live := &blockedTowerScanner{}, &liveTowerScanner{}
	e.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: blocked, chain.Blake: live}
	broadcasts := 0
	b.broadcast = func(raw string) (string, error) {
		broadcasts++
		tx, err := contract.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := contract.ExtractSecret(target, tx); !ok {
			t.Fatal("wrong recovery transaction")
		}
		return tx.TxHash().String(), nil
	}
	for cycle := 0; cycle < 2; cycle++ {
		state.LastAttempt = 0
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = e.Tick(ctx)
		cancel()
		if broadcasts != cycle+1 || live.calls != cycle+1 || blocked.calls != cycle+1 {
			t.Fatal("slow BTC tower work starved healthy Blake recovery", broadcasts, live.calls, blocked.calls)
		}
	}
	state.LastAttempt = 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = e.Tick(ctx)
	if ctx.Err() == nil || broadcasts != 2 {
		t.Fatal("overall worker deadline did not stop recovery publication")
	}
}

func (s *callbackRecoveryScanner) Scan(context.Context, uint32, []string) (map[string]chain.Observation, error) {
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	return s.observations, nil
}

func TestIsolatedAcceptedScanKeepsWitnessAcrossLaterSourceChange(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, timing := range []string{"before_accept", "during_second_scan"} {
			t.Run(role+"/"+timing, func(t *testing.T) {
				sell := chain.BTC
				if role == "maker" {
					sell = chain.Blake
				}
				e, s, _, secret := isolatedFixtureSell(t, role, sell)
				incoming, own := s.Short, s.Long
				if role == "maker" {
					incoming, own = s.Long, s.Short
				}
				if incoming.Chain != chain.BTC {
					t.Fatal("fixture must scan incoming first")
				}
				key, _ := e.swapKey(incoming.Chain, s.ID)
				claim, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], 2000, false, 0, nil, 0, secret)
				if err != nil {
					t.Fatal(err)
				}
				e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
				first := &callbackRecoveryScanner{observations: map[string]chain.Observation{chain.OutpointKey(incoming.TxID, incoming.Vout): {Tx: claim, TxID: claim.TxHash().String()}}}
				second := &callbackRecoveryScanner{observations: map[string]chain.Observation{}}
				if timing == "before_accept" {
					first.beforeReturn = func() { e.chainFresh[chain.BTC] = false }
				} else {
					second.beforeReturn = func() {
						var saved State
						if _, err := e.vault.Load(&saved); err != nil {
							t.Fatal(err)
						}
						if !saved.Swaps[s.ID].IncomingClaimSeen {
							t.Fatal("accepted witness was not durable before second-chain IO")
						}
						e.chainFresh[chain.BTC] = false
					}
				}
				e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: first, chain.Blake: second}
				_, _ = e.scan(context.Background())
				var restored State
				if _, err := e.vault.Load(&restored); err != nil {
					t.Fatal(err)
				}
				e.s = restored
				s = e.s.Swaps[s.ID]
				if e.fresh(chain.BTC) || !s.SecretObserved || !s.IncomingClaimSeen {
					t.Fatal("source demotion erased immutable witnessed knowledge")
				}
				e.chainFresh[chain.BTC] = true
				e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &settlementFeeScanner{observations: map[string]chain.Observation{}}, chain.Blake: &settlementFeeScanner{observations: map[string]chain.Observation{}}}
				if err := e.checkRefundAcceleration(context.Background(), s, own); err == nil || !strings.Contains(err.Error(), "previously claimed") {
					t.Fatal("later witness disappearance enabled refund", err)
				}
			})
		}
	}
}

func TestIsolatedTowerRemembersObserveOnlyWitnessBeforeTargetReturns(t *testing.T) {
	e, s, b, secret := isolatedFixture(t, "maker")
	tower := e.ownTower()
	s.Protection = &tower
	target, observe := s.Long, s.Short
	job, err := e.makeJob(s, target, "claim", &observe, s.Terms.Takeover)
	if err != nil {
		t.Fatal(err)
	}
	e.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	key, _ := e.swapKey(observe.Chain, s.ID)
	claim, err := contract.Spend(observe, key, e.scripts[observe.Chain], 2000, false, 0, nil, 0, secret)
	if err != nil {
		t.Fatal(err)
	}
	e.chainFresh[target.Chain], e.chainFresh[observe.Chain] = false, true
	e.towerScanners = map[chain.ID]chain.SpendScanner{observe.Chain: &settlementFeeScanner{observations: map[string]chain.Observation{chain.OutpointKey(observe.TxID, observe.Vout): {Tx: claim, TxID: claim.TxHash().String()}}}}
	all, err := e.scanTower(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if _, err := e.vault.Load(&restored); err != nil {
		t.Fatal(err)
	}
	if restored.TowerJobs[job.ID] == nil || restored.TowerJobs[job.ID].Secret != hex.EncodeToString(secret) {
		t.Fatal("tower discarded witness while target unavailable")
	}
	if err := e.advanceTower(context.Background(), all); err != nil {
		t.Fatal(err)
	}
	e.s = restored
	e.chainFresh[target.Chain], e.chainFresh[observe.Chain] = true, false
	broadcasts := 0
	b.broadcast = func(raw string) (string, error) {
		broadcasts++
		tx, err := contract.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := contract.ExtractSecret(target, tx); !ok || tx.TxOut[1].Value != protocol.Bounty(target.Amount, job.BPS) {
			t.Fatal("restored tower changed authorized claim")
		}
		return tx.TxHash().String(), nil
	}
	if err := e.advanceTower(context.Background(), map[chain.ID]map[string]chain.Observation{target.Chain: {}}); err != nil || broadcasts != 1 {
		t.Fatal("target-only recovery forgot prior witness", err, broadcasts)
	}
}

func (b *recoveryClockBackend) Height(context.Context) (uint32, error) { return 500, nil }

func TestIsolatedTerminalHistorySurvivesOutageButReopensOnReorg(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, stage := range []string{"completed", "refunded"} {
			for _, reorg := range []bool{false, true} {
				t.Run(role+"/"+stage+"/"+map[bool]string{false: "outage", true: "reorg"}[reorg], func(t *testing.T) {
					e, s, b, secret := isolatedFixture(t, role)
					target := s.Short
					if role == "maker" {
						target = s.Long
					}
					key, _ := e.swapKey(target.Chain, s.ID)
					lock := uint32(0)
					if stage == "refunded" {
						lock = target.RefundHeight
						secret = nil
					}
					tx, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, stage == "refunded", lock, nil, 0, secret)
					if err != nil {
						t.Fatal(err)
					}
					s.Stage, s.LongSpend, s.ShortSpend, s.LongConfirmations, s.ShortConfirmations = stage, strings.Repeat("1", 64), strings.Repeat("2", 64), 2, 2
					if target.Chain == s.Long.Chain {
						s.LongSpend = tx.TxHash().String()
					} else {
						s.ShortSpend = tx.TxHash().String()
					}
					if err := e.CanChangeNetwork(); err != nil {
						t.Fatal("fixture is not terminal", err)
					}
					confirmations := 2
					if reorg {
						confirmations = 0
					}
					all := map[chain.ID]map[string]chain.Observation{target.Chain: {chain.OutpointKey(target.TxID, target.Vout): {Tx: tx, TxID: tx.TxHash().String(), Confirmations: confirmations}}}
					b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
					_ = e.advanceSwap(context.Background(), s, all)
					if !reorg && (s.Stage != stage || e.CanChangeNetwork() != nil) {
						t.Fatal("unrelated outage invented active obligation", s.Stage)
					}
					if reorg && (terminalSwapStage(s.Stage) || e.CanChangeNetwork() == nil) {
						t.Fatal("fresh reorg evidence failed to reopen obligation", s.Stage)
					}
				})
			}
		}
	}
}

func TestIsolatedWitnessPersistsBeforeUnrelatedRecoveryFailure(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, action := range []string{"funding_lookup", "manual_refund"} {
			t.Run(role+"/"+action, func(t *testing.T) {
				e, s, b, secret := isolatedFixture(t, role)
				own, incoming := s.Long, s.Short
				if role == "maker" {
					own, incoming = s.Short, s.Long
				}
				e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
				all := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
				for _, target := range []contract.HTLC{own, incoming} {
					key, err := e.swapKey(target.Chain, s.ID)
					if err != nil {
						t.Fatal(err)
					}
					tx, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, false, 0, nil, 0, secret)
					if err != nil {
						t.Fatal(err)
					}
					all[target.Chain][chain.OutpointKey(target.TxID, target.Vout)] = chain.Observation{Tx: tx, TxID: tx.TxHash().String()}
				}
				if action == "funding_lookup" {
					if err := e.advanceSwap(context.Background(), s, all); err == nil {
						t.Fatal("fixture did not fail unrelated funding lookup")
					}
				} else {
					e.nodes[own.Chain] = &recoveryClockBackend{Backend: b}
					e.scanners = map[chain.ID]chain.SpendScanner{own.Chain: &settlementFeeScanner{observations: all[own.Chain]}, incoming.Chain: &settlementFeeScanner{observations: all[incoming.Chain]}}
					base, _ := contract.Parse(s.SelfRefunds[0])
					params, _ := json.Marshal(BumpRequest{ID: s.ID, Kind: "refund", Fee: 6000, ExpectedTxID: base.TxHash().String()})
					if _, err := e.bumpTransaction(context.Background(), params); err == nil || !strings.Contains(err.Error(), "claim") {
						t.Fatal("fixture did not reject manual refund", err)
					}
				}
				// No final Tick/save follows the error. Reopen exactly what the failed
				// action persisted, then remove every spend from the next observation.
				var restored State
				if _, err := e.vault.Load(&restored); err != nil {
					t.Fatal(err)
				}
				e.s = restored
				s = e.s.Swaps[s.ID]
				if !s.SecretObserved || !s.SecretExposed || !s.IncomingClaimSeen {
					t.Fatal("failed action forgot a witnessed claim before returning")
				}
				e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &settlementFeeScanner{observations: map[string]chain.Observation{}}, chain.Blake: &settlementFeeScanner{observations: map[string]chain.Observation{}}}
				if err := e.checkRefundAcceleration(context.Background(), s, own); err == nil || !strings.Contains(err.Error(), "previously claimed") {
					t.Fatal("witness disappearance enabled refund after restart", err)
				}
			})
		}
	}
}

func (b *switchingEstimateBackend) EstimateFee(context.Context, uint32) chain.FeeEstimate {
	b.onEstimate()
	return chain.FeeEstimate{Chain: b.id, State: "available", Rate: 1000, Timestamp: time.Now().Unix()}
}

func TestIsolatedTowerFeeSelectionCannotReuseChangedSourceEvidence(t *testing.T) {
	e, s, b, _ := isolatedFixture(t, "maker")
	tower := e.ownTower()
	s.Protection = &tower
	job, err := e.makeJob(s, s.Short, "refund", nil, s.Short.RefundHeight+protocol.RefundDelay(chain.Regtest))
	if err != nil {
		t.Fatal(err)
	}
	e.chainFresh[chain.BTC], e.chainFresh[chain.Blake] = true, true
	e.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job}}
	e.nodes[job.Target.Chain] = &switchingEstimateBackend{Backend: b, id: job.Target.Chain, onEstimate: func() { e.chainFresh[job.Target.Chain] = false }}
	b.broadcast = func(string) (string, error) {
		t.Fatal("tower broadcast after fee estimate invalidated source")
		return "", nil
	}
	if err := e.advanceTower(context.Background(), map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}); err != nil {
		t.Fatal(err)
	}
	state := e.s.TowerJobs[job.ID]
	if !strings.Contains(state.Error, "source changed") || state.LastAttempt != 0 || len(state.Variants) != 0 {
		t.Fatal("tower did not preserve recovery gate", state.Error)
	}
}

func TestIsolatedManualAccelerationCannotPublishPrivateClaim(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			target := s.Short
			if role == "maker" {
				target = s.Long
			}
			key, _ := e.swapKey(target.Chain, s.ID)
			claim, err := contract.Spend(target, key, e.scripts[target.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			s.OwnerFeeCap, s.SelfClaim, s.SecretExposed = 20000, contract.Hex(claim), true
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			s = saved.Swaps[s.ID]
			b.broadcast = func(string) (string, error) {
				t.Fatal("manual bump revealed private secret during peer outage")
				return "", nil
			}
			params, _ := json.Marshal(BumpRequest{ID: s.ID, Kind: "claim", Fee: 6000, ExpectedTxID: claim.TxHash().String()})
			if _, err := e.bumpTransaction(context.Background(), params); err == nil {
				t.Fatal("private prepared claim bypassed gate")
			}
			if len(s.SelfClaims) != 0 || s.SecretObserved {
				t.Fatal("blocked acceleration changed publication authority")
			}
		})
	}
}

func TestIsolatedMempoolClaimKeepsAuthorizedVariantsAndDestination(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, cap := range []int64{0, 20000} {
			t.Run(role+"/"+map[int64]string{0: "legacy", 20000: "authorized"}[cap], func(t *testing.T) {
				e, s, b, secret := isolatedFixture(t, role)
				target := s.Short
				if role == "maker" {
					target = s.Long
				}
				key, _ := e.swapKey(target.Chain, s.ID)
				claim, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, false, 0, nil, 0, secret)
				if err != nil {
					t.Fatal(err)
				}
				s.OwnerFeeCap, s.SelfClaim, s.SecretExposed, s.SecretObserved, s.ClaimAttempt = cap, contract.Hex(claim), true, true, 3
				original := s.SelfClaim
				e.scripts[target.Chain] = []byte{0x51} // A rotated receive address must not redirect saved variants.
				b.spent = true
				broadcasts := 0
				b.broadcast = func(raw string) (string, error) {
					broadcasts++
					tx, err := contract.Parse(raw)
					if err != nil {
						t.Fatal(err)
					}
					wantFee := int64(2000)
					if cap > 0 {
						wantFee = 6000
					}
					if target.Amount-tx.TxOut[0].Value != wantFee || !bytes.Equal(tx.TxOut[0].PkScript, claim.TxOut[0].PkScript) {
						t.Fatal("variant changed fee consent or destination")
					}
					var stored State
					if _, err := e.vault.Load(&stored); err != nil {
						t.Fatal(err)
					}
					persisted := stored.Swaps[s.ID]
					if persisted.SelfClaims[persisted.ClaimVariant] != raw || persisted.OwnerFeeCap != cap || persisted.SelfClaim != original {
						t.Fatal("variant was not durable")
					}
					return tx.TxHash().String(), nil
				}
				all := map[chain.ID]map[string]chain.Observation{target.Chain: {chain.OutpointKey(target.TxID, target.Vout): {Tx: claim, TxID: claim.TxHash().String()}}}
				if err := e.advanceIsolatedSwap(context.Background(), s, all); err != nil || broadcasts != 1 {
					t.Fatal(err, broadcasts)
				}
				if !s.IncomingClaimSeen {
					t.Fatal("incoming witness guard lost")
				}
			})
		}
	}
}

func isolatedFixture(t *testing.T, role string) (*Engine, *Swap, *sendBackend, []byte) {
	return isolatedFixtureSell(t, role, chain.BTC)
}
func isolatedFixtureSell(t *testing.T, role string, sell chain.ID) (*Engine, *Swap, *sendBackend, []byte) {
	t.Helper()
	e, b, _ := sendFixture(t)
	maker, taker := nostr.Generate(), nostr.Generate()
	offer := protocol.Offer{ID: transport.RandomID(), Maker: maker.Public().Hex(), Sell: sell, SellAmount: 1000000, BuyAmount: 2000000, Expires: time.Now().Unix() + 3600, Status: "open"}
	raw, _ := offer.PublicJSON()
	event := nostr.Event{Kind: transport.OfferKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", offer.ID}, {"t", transport.Namespace}}, Content: string(raw)}
	if err := transport.Sign(&event, maker); err != nil {
		t.Fatal(err)
	}
	id := transport.RandomID()
	keys, err := e.swapKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(strings.Repeat("s", 32))
	hash := sha256.Sum256(secret)
	req := protocol.Request{ID: id, OfferEvent: event, Taker: taker.Public().Hex(), Hash: hex.EncodeToString(hash[:]), Keys: keys}
	terms, err := protocol.NewTerms(req, keys, map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 100})
	if err != nil {
		t.Fatal(err)
	}
	s := &Swap{ID: id, Role: role, Request: req, Terms: &terms, Long: terms.Long, Short: terms.Short, Secret: hex.EncodeToString(secret), LongSent: true, ShortSent: true}
	for _, c := range []*contract.HTLC{&s.Long, &s.Short} {
		funding := wire.NewMsgTx(2)
		funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
		script, _ := c.PkScript()
		funding.AddTxOut(wire.NewTxOut(c.Amount, script))
		c.TxID = funding.TxHash().String()
		if c == &s.Long {
			s.LongFunding = contract.Hex(funding)
		} else {
			s.ShortFunding = contract.Hex(funding)
		}
	}
	own, incoming := s.Long, s.Short
	if role == "maker" {
		own, incoming = s.Short, s.Long
	}
	pk, _ := incoming.PkScript()
	b.coins = []chain.UTXO{{TxID: incoming.TxID, Vout: incoming.Vout, Amount: chain.Coins(incoming.Amount), Script: hex.EncodeToString(pk), Confirmations: 2}}
	e.nodes = map[chain.ID]chain.Backend{incoming.Chain: b, own.Chain: &fundingLookupBackend{err: context.DeadlineExceeded}}
	e.scanners = map[chain.ID]chain.SpendScanner{incoming.Chain: &settlementFeeScanner{observations: map[string]chain.Observation{}}, own.Chain: &settlementFeeScanner{observations: map[string]chain.Observation{}}}
	e.chainFresh = map[chain.ID]bool{incoming.Chain: true, own.Chain: false}
	e.heights = map[chain.ID]uint32{chain.BTC: 500, chain.Blake: 500}
	e.clocks = e.heights
	e.s.Swaps = map[string]*Swap{id: s}
	if err := e.prepare(s, own); err != nil {
		t.Fatal(err)
	}
	return e, s, b, secret
}
func TestIsolatedNeverRevealsPrivateOrPreparedSecretAndNeverRefunds(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, prepared := range []bool{false, true} {
			t.Run(role+"/prepared="+map[bool]string{true: "yes", false: "no"}[prepared], func(t *testing.T) {
				e, s, b, secret := isolatedFixture(t, role)
				incoming := s.Short
				if role == "maker" {
					incoming = s.Long
				}
				if prepared {
					key, _ := e.swapKey(incoming.Chain, s.ID)
					tx, err := contract.Spend(incoming, key, e.scripts[incoming.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
					if err != nil {
						t.Fatal(err)
					}
					s.SelfClaim = contract.Hex(tx)
					s.SecretExposed = true
					// Crash after signing/saving but before the first broadcast. Reload the
					// exact durable state; these private bytes are still not public knowledge.
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					var saved State
					if _, err := e.vault.Load(&saved); err != nil {
						t.Fatal(err)
					}
					e.s = saved
					s = e.s.Swaps[s.ID]
				}
				b.broadcast = func(string) (string, error) { t.Fatal("degraded first revelation/refund"); return "", nil }
				if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {}}); err == nil {
					t.Fatal("private secret accepted")
				}
				if s.SecretObserved {
					t.Fatal("private secret relabeled observed")
				}
			})
		}
	}
}
func TestIsolatedClaimsWithObservedWitnessAndPersistsIncomingClaimGuard(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, s, b, secret := isolatedFixture(t, role)
			incoming, own := s.Short, s.Long
			if role == "maker" {
				incoming, own = s.Long, s.Short
			}
			e.chainFresh[own.Chain] = true
			e.chainFresh[incoming.Chain] = false
			key, _ := e.swapKey(own.Chain, s.ID)
			witness, err := contract.Spend(own, key, e.scripts[own.Chain], protocol.RescueFees[0], false, 0, nil, 0, secret)
			if err != nil {
				t.Fatal(err)
			}
			observations := map[chain.ID]map[string]chain.Observation{own.Chain: {chain.OutpointKey(own.TxID, own.Vout): {Tx: witness, TxID: witness.TxHash().String()}}}
			_ = e.advanceIsolatedSwap(context.Background(), s, observations)
			if !s.SecretObserved || !s.SecretExposed || s.IncomingClaimSeen {
				t.Fatal("wrong secret provenance")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			var saved State
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			e.s = saved
			s = e.s.Swaps[s.ID]
			e.chainFresh[own.Chain] = false
			e.chainFresh[incoming.Chain] = true
			broadcasts := 0
			b.broadcast = func(raw string) (string, error) {
				broadcasts++
				tx, err := contract.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := contract.ExtractSecret(incoming, tx); !ok {
					t.Fatal("not a claim")
				}
				return tx.TxHash().String(), nil
			}
			if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {}}); err != nil || broadcasts != 1 {
				t.Fatal("observed-secret recovery failed", err, broadcasts)
			}
			claim, _ := contract.Parse(s.SelfClaim)
			_ = e.advanceIsolatedSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {chain.OutpointKey(incoming.TxID, incoming.Vout): {Tx: claim, TxID: claim.TxHash().String(), Confirmations: 2}}})
			if !s.IncomingClaimSeen {
				t.Fatal("incoming claim guard missing")
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if _, err := e.vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			if !saved.Swaps[s.ID].IncomingClaimSeen {
				t.Fatal("guard not durable")
			}
			// The incoming witness can disappear during a reorg. Even with both chains
			// healthy and the refund deadline passed, it must not authorize a refund.
			e.chainFresh[own.Chain] = true
			b.spent = true
			ownBackend := &fundingLookupBackend{tx: chain.Transaction{}}
			e.nodes[own.Chain] = ownBackend
			_ = e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{incoming.Chain: {}, own.Chain: {}})
			if len(ownBackend.broadcasts) != 0 {
				t.Fatal("refunded after incoming claim reorg")
			}
		})
	}
}
func TestIsolatedTargetEvidenceAndSafeSendProgress(t *testing.T) {
	e, s, b, _ := isolatedFixture(t, "maker")
	s.SecretObserved = true
	b.broadcast = func(string) (string, error) { t.Fatal("claim without target evidence"); return "", nil }
	if err := e.advanceSwap(context.Background(), s, nil); err == nil {
		t.Fatal("missing scan treated as absence")
	}
	b.spent = true
	if err := e.advanceSwap(context.Background(), s, map[chain.ID]map[string]chain.Observation{s.Long.Chain: {}}); err == nil {
		t.Fatal("missing output accepted")
	}
	// An existing signed wallet send needs only its own chain. Missing peer data
	// never permits a new send, but cannot suppress exact transaction recovery.
	e, b, req := sendFixture(t)
	b.broadcast = func(string) (string, error) { return "", errors.New("ambiguous") }
	raw, _ := json.Marshal(req)
	if _, err := e.sendCoins(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	e.chainFresh = map[chain.ID]bool{chain.BTC: false, chain.Blake: true}
	b.broadcast = func(raw string) (string, error) { tx, _ := contract.Parse(raw); return tx.TxHash().String(), nil }
	e.s.Sends[req.ID].LastAttempt = 0
	e.advanceSends(context.Background())
	if !e.s.Sends[req.ID].Submitted {
		t.Fatal("healthy signed send stalled on peer outage")
	}
}

func TestMissingReadinessFailsClosed(t *testing.T) {
	e := &Engine{heights: map[chain.ID]uint32{chain.BTC: 1000, chain.Blake: 1000}}
	if e.fresh(chain.BTC) || e.eligible(chain.BTC, 1) || e.gate(nil, "reveal") == nil {
		t.Fatal("uninitialized observations authorized an action")
	}
}
