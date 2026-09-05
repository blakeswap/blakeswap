package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
)

func claimJob(t *testing.T, e *Engine) protocol.Job {
	t.Helper()
	claim, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	refund, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	target := contract.HTLC{Chain: chain.BTC, Hash: transport.RandomID(), ClaimKey: hex.EncodeToString(claim.PubKey().SerializeCompressed()), RefundKey: hex.EncodeToString(refund.PubKey().SerializeCompressed()), RefundHeight: 200, Amount: 2000000, TxID: transport.RandomID()}
	observed := target
	observed.Chain = chain.Blake
	observed.TxID = transport.RandomID()
	own := e.ownTower()
	payout := e.scripts[chain.Blake]
	j := protocol.Job{Network: chain.Regtest, ID: transport.RandomID(), SwapID: transport.RandomID(), Owner: e.identity.Public().Hex(), TermsHash: transport.RandomID(), Kind: "claim", Target: target, Observe: &observed, ScanFrom: 1, Lock: 150, BPS: own.BPS, Payout: hex.EncodeToString(payout), TowerScript: own.Scripts[chain.BTC]}
	towerScript, _ := hex.DecodeString(j.TowerScript)
	for _, fee := range protocol.RescueFees {
		tx, err := contract.Spend(target, claim, payout, fee, false, j.Lock, towerScript, protocol.Bounty(target.Amount, j.BPS), nil)
		if err != nil {
			t.Fatal(err)
		}
		j.Templates = append(j.Templates, contract.Hex(tx))
	}
	if err := j.Validate(own.Scripts, own.BPS); err != nil {
		t.Fatal(err)
	}
	return j
}

type recordingScanner struct {
	points []string
	err    error
}

func (s *recordingScanner) Scan(_ context.Context, _ uint32, points []string) (map[string]chain.Observation, error) {
	s.points = append([]string(nil), points...)
	return map[string]chain.Observation{}, s.err
}
func TestRemoteTowerOutpointsCannotPoisonLocalScanner(t *testing.T) {
	e := discoveryEngine(t)
	job := claimJob(t, e)
	altered := *job.Observe
	altered.Vout = ^uint32(0)
	bad := job
	bad.Observe = &altered
	if bad.Validate(e.ownTower().Scripts, e.ownTower().BPS) == nil {
		t.Fatal("out-of-range observed output accepted")
	}
	local := &recordingScanner{}
	remote := &recordingScanner{err: errors.New("invalid remote history")}
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: local, chain.Blake: &recordingScanner{}}
	e.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: remote, chain.Blake: remote}
	e.heights = map[chain.ID]uint32{chain.BTC: 120, chain.Blake: 120}
	localTx := transport.RandomID()
	e.s.Swaps = map[string]*Swap{"local": {Terms: &protocol.Terms{}, Long: contract.HTLC{Chain: chain.BTC, TxID: localTx, RefundHeight: 200}}}
	e.s.TowerJobs = map[string]*TowerJob{job.ID: {Job: job}}
	if _, err := e.scan(context.Background()); err != nil {
		t.Fatal("remote failure reached local scan", err)
	}
	if len(local.points) != 1 || local.points[0] != chain.OutpointKey(localTx, 0) {
		t.Fatal("remote registration entered local scan", local.points)
	}
	if _, err := e.scanTower(context.Background()); err == nil {
		t.Fatal("fixture did not exercise remote failure")
	}
	if len(local.points) != 1 || local.points[0] != chain.OutpointKey(localTx, 0) {
		t.Fatal("remote scan replaced local cursor")
	}
}

type towerLookup struct {
	chain.Backend
	tx  chain.Transaction
	err error
}

func (b *towerLookup) Transaction(context.Context, string) (chain.Transaction, error) {
	return b.tx, b.err
}
func TestNeverFundedRegistrationsExpireWithoutForgettingFundedJobs(t *testing.T) {
	e := discoveryEngine(t)
	job := claimJob(t, e)
	lookup := &towerLookup{err: &chain.RPCError{Code: -5, Message: "transaction not found"}}
	e.nodes = map[chain.ID]chain.Backend{chain.BTC: lookup}
	e.clocks = map[chain.ID]uint32{chain.BTC: 199}
	e.heights = e.clocks
	state := &TowerJob{Job: job}
	e.s.TowerJobs = map[string]*TowerJob{job.ID: state}
	e.refreshTowerJobs(context.Background())
	if state.Expired || e.CanChangeNetwork() == nil {
		t.Fatal("unfunded job retired before funding window ended")
	}
	e.clocks[chain.BTC] = job.Target.RefundHeight + protocol.RefundGrace
	e.refreshTowerJobs(context.Background())
	if !state.Expired || e.CanChangeNetwork() != nil {
		t.Fatal("abandoned registration blocks network forever")
	}
	lookup.err = errors.New("connection unavailable")
	e.refreshTowerJobs(context.Background())
	if state.Expired || e.CanChangeNetwork() == nil {
		t.Fatal("lookup failure discarded an obligation")
	}
	lookup.err = nil
	lookup.tx = chain.Transaction{TxID: job.Target.TxID}
	e.refreshTowerJobs(context.Background())
	lookup.err = &chain.RPCError{Code: -5, Message: "transaction not found"}
	e.refreshTowerJobs(context.Background())
	if !state.FundingSeen || state.Expired || e.CanChangeNetwork() == nil {
		t.Fatal("previously funded obligation was forgotten")
	}
	state.Confirmed = 6
	if e.CanChangeNetwork() != nil {
		t.Fatal("settled obligation still blocks network")
	}
}
