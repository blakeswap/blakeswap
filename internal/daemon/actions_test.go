package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func actionEngine(now int64) *Engine {
	return &Engine{Config: Config{Name: "other-wallet", Network: chain.Regtest}, chainFresh: map[chain.ID]bool{chain.BTC: true, chain.Blake: true}, chainObserved: map[chain.ID]int64{chain.BTC: now, chain.Blake: now}, heights: map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 300}, clocks: map[chain.ID]uint32{chain.BTC: 100, chain.Blake: 300}}
}
func TestActionDeadlinesExactClockUnitsAndStaleness(t *testing.T) {
	now := int64(1800000000)
	e := actionEngine(now)
	d := e.actionDeadline("reveal", chain.BTC, 106, now)
	if !d.Certain || d.Remaining != 6 || d.Band != "approaching" || d.Unit != "blocks" {
		t.Fatal(d)
	}
	if d = e.actionDeadline("reveal", chain.BTC, 100, now); d.Band != "reached" {
		t.Fatal(d)
	}
	e.chainFresh[chain.Blake] = false
	if d = e.actionDeadline("reveal", chain.BTC, 106, now); d.Certain {
		t.Fatal("peer outage hidden", d)
	}
	e.chainFresh[chain.Blake] = true
	e.Config.Network = chain.Mainnet
	e.clocks = map[chain.ID]uint32{chain.BTC: uint32(now), chain.Blake: uint32(now)}
	if d = e.actionDeadline("refund", chain.BTC, uint32(now), now); !d.Certain || d.Remaining != 1 || d.Band == "reached" || d.Unit != "median_time" {
		t.Fatal("timestamp finality is strictly after lock", d)
	}
	if d = e.actionDeadline("reveal", chain.BTC, uint32(now), now); d.Remaining != 0 || d.Band != "reached" {
		t.Fatal(d)
	}
	e.clocks[chain.Blake] -= protocol.MaxClockSkew + 1
	if d = e.actionDeadline("reveal", chain.BTC, uint32(now+100), now); d.Certain {
		t.Fatal("clock disagreement hidden", d)
	}
	if d = e.actionDeadline("refund", chain.BTC, uint32(now+100), now+91); d.Certain || d.Band != "unknown" {
		t.Fatal("stale observation trusted", d)
	}
}
func TestActionSummaryPreservesLocalObligationsAndRecoveryHolds(t *testing.T) {
	now := time.Now().Unix()
	e := actionEngine(now)
	e.s.Swaps = map[string]*Swap{"swap-secret-id": {ID: "swap-secret-id", Role: "taker", Stage: "funding broadcast", LongFunding: "private-signed-raw", Secret: "private-preimage", Terms: &protocol.Terms{RevealBefore: 104, Long: contract.HTLC{Chain: chain.BTC, RefundHeight: 200}, Short: contract.HTLC{Chain: chain.Blake, RefundHeight: 400}}}}
	e.s.Sends = map[string]*WalletSend{"send": {PublicSend: PublicSend{ID: "send", Chain: chain.BTC, Confirmations: 0}, Raw: "private-send-raw"}}
	e.s.TowerJobs = map[string]*TowerJob{"job": {Job: protocol.Job{ID: "job", Kind: "refund", Lock: 120, Target: contract.HTLC{Chain: chain.BTC}}, Secret: "private-job-secret"}}
	w := e.walletActions(now)
	s := SummarizeActions(chain.Regtest, 4, []WalletActions{w}, now)
	if !s.Complete || !s.RequiresMonitoring || len(w.Actions) != 3 {
		t.Fatal(s)
	}
	var reveal bool
	for _, a := range w.Actions {
		if a.Kind == "swap" {
			reveal = a.FirstReveal && len(a.Deadlines) == 3
		}
	}
	if !reveal {
		t.Fatal("taker first revelation absent")
	}
	raw, _ := json.Marshal(s)
	for _, secret := range []string{"private-signed-raw", "private-preimage", "private-send-raw", "private-job-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret leaked")
		}
	}
	e.s.Swaps["swap-secret-id"].Stage = "completed"
	e.s.Sends["send"].Confirmations = 6
	e.s.TowerJobs["job"].Confirmed = 6
	e.chainFresh = map[chain.ID]bool{}
	if s = SummarizeActions(chain.Regtest, 4, []WalletActions{e.walletActions(now)}, now); s.RequiresMonitoring {
		t.Fatal("ordinary outage reopened positive terminal history", s)
	}
	e.s.Recovery = &RecoveryRecord{Status: RecoveryStatus{State: "ready"}, InvalidatedSettlements: map[string]bool{"swap/swap-secret-id": true, "send/send": true, "tower/job": true}}
	s = SummarizeActions(chain.Regtest, 4, []WalletActions{e.walletActions(now)}, now)
	if !s.RequiresMonitoring {
		t.Fatal("durable reorg hold hidden")
	}
	for _, a := range s.Wallets[0].Actions {
		if a.Kind != "recovery" && (!a.RequiresMonitoring || a.State != "reopened") {
			t.Fatal(a)
		}
	}
}
func TestActionStoredEmptyAndUnavailableAreDistinct(t *testing.T) {
	now := time.Now().Unix()
	w, err := LoadStoredActions(Config{Name: "fresh", Network: chain.Regtest, DataDir: filepath.Join(t.TempDir(), "new")})
	if err != nil {
		t.Fatal(err)
	}
	if s := SummarizeActions(chain.Regtest, 1, []WalletActions{w}, now+3600); !s.Complete || s.RequiresMonitoring {
		t.Fatal("known empty local state requires no node", s)
	}
	w = WalletActions{WalletID: "opening", Network: chain.Regtest}
	if s := SummarizeActions(chain.Regtest, 1, []WalletActions{w}, now); s.Complete || !s.RequiresMonitoring {
		t.Fatal("unreadable state became empty", s)
	}
	w = actionEngine(now).walletActions(now)
	if s := SummarizeActions(chain.Regtest, 1, []WalletActions{w}, now+91); s.Complete {
		t.Fatal("stale live state accepted")
	}
}
