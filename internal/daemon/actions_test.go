package daemon

import (
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/storage"
	"os"
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
			reveal = a.FirstReveal && len(a.Deadlines) == 4
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

func TestActionArmedTowerDoesNotSuppressFirstRevealAndImportedCannotReveal(t *testing.T) {
	now := time.Now().Unix()
	e := actionEngine(now)
	job := protocol.Job{Version: protocol.Version, ID: protocol.Digest("action receipt job")}
	s := &Swap{ID: "s", Role: "taker", Stage: "awaiting chain confirmations", Protection: &protocol.Tower{BPS: 50}, Jobs: []protocol.Job{job}, Receipts: map[string]protocol.Receipt{job.ID: {Version: protocol.Version, JobID: job.ID, Digest: protocol.Digest(job)}}, Terms: &protocol.Terms{RevealBefore: 106, Long: contract.HTLC{Chain: chain.BTC, RefundHeight: 200}, Short: contract.HTLC{Chain: chain.Blake, RefundHeight: 316}}}
	e.s.Swaps = map[string]*Swap{"s": s}
	a := e.walletActions(now).Actions[0]
	if !a.TowerReady || !a.FirstReveal {
		t.Fatal("external tower must not suppress first reveal", a)
	}
	var peerMargin bool
	for _, d := range a.Deadlines {
		if d.Kind == "reveal_safety" {
			peerMargin = d.Chain == chain.Blake && d.Remaining == 1 && d.Band == "approaching"
		}
	}
	if !peerMargin {
		t.Fatal("asymmetric peer-chain safety margin hidden", a)
	}
	e.s.Recovery = &RecoveryRecord{Status: RecoveryStatus{State: "recovering"}, Swaps: map[string]bool{"s": true}}
	for _, a := range e.walletActions(now).Actions {
		if a.Kind == "swap" && (a.FirstReveal || a.State != "restored_monitoring") {
			t.Fatal("imported private first revelation was advertised", a)
		}
	}
}
func TestActionEnabledAutomationRemainsPotentialAuthority(t *testing.T) {
	now := time.Now().Unix()
	e := actionEngine(now)
	e.s.Automations = map[string]*AutomationPolicy{"p": {Enabled: true}}
	s := SummarizeActions(chain.Regtest, 1, []WalletActions{e.walletActions(now)}, now)
	if !s.RequiresMonitoring || s.Wallets[0].Actions[0].Kind != "automation" {
		t.Fatal(s)
	}
	e.s.Automations["p"].Enabled = false
	e.s.Automations["p"].RestoreHold = true
	if s = SummarizeActions(chain.Regtest, 1, []WalletActions{e.walletActions(now)}, now); s.RequiresMonitoring {
		t.Fatal("disabled held policy alone is not a funded obligation", s)
	}
	e.s.Automations["p"].Pending = &ConfirmTradeRequest{}
	if s = SummarizeActions(chain.Regtest, 1, []WalletActions{e.walletActions(now)}, now); !s.RequiresMonitoring {
		t.Fatal("pending authorization hidden", s)
	}
}

func TestActionEncryptedStoredObligationSurvivesEndpointFailure(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("test-password-at-least-sixteen-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := storage.Open(filepath.Join(dir, "state.db"), []byte("test-password-at-least-sixteen-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, swap, _, _ := isolatedFixture(t, "taker")
	swap.Stage = "claiming"
	state := fixture.s
	if err := v.Save(state); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "offline", Network: chain.Regtest, DataDir: dir, PasswordFile: password}
	if _, err := LoadStoredActions(cfg); err == nil {
		t.Fatal("locked live vault must remain unknown")
	}
	v.Close()
	w, err := LoadStoredActions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w.Source != "stored" || !w.Known || len(w.Actions) != 1 || !w.Actions[0].RequiresMonitoring || !w.Actions[0].Uncertain {
		t.Fatal(w)
	}
	cfg.Network = chain.Mainnet
	if _, err := LoadStoredActions(cfg); err == nil {
		t.Fatal("wrong-network file was trusted")
	}
}

func TestActionSummaryIncludesColdArchiveHoldsBeforeReactivation(t *testing.T) {
	now := time.Now().Unix()
	e := actionEngine(now)
	fixture, swap, _, _ := isolatedFixture(t, "taker")
	swap.Stage = "completed"
	e.s = fixture.s
	e.s.Capacity = &CapacityRecord{Reactivating: true, Invalidated: map[string]bool{"send/cold": true, "swap/" + swap.ID: true}}
	got := e.walletActions(now)
	seen := map[string]int{}
	for _, a := range got.Actions {
		seen[a.ID]++
		if !a.RequiresMonitoring || !a.Uncertain {
			t.Fatal("cold/reopened obligation treated as settled", a)
		}
		if a.ID == "send/cold" && (a.ObjectID != "cold" || a.Kind != "send") {
			t.Fatal("cold detail identity lost", a)
		}
	}
	if seen["archive"] != 1 || seen["send/cold"] != 1 || seen["swap/"+swap.ID] != 1 || len(got.Actions) != 3 {
		t.Fatal("missing/duplicate archived obligation", got)
	}
	summary := SummarizeActions(chain.Regtest, 1, []WalletActions{got}, now)
	if !summary.Complete || !summary.RequiresMonitoring {
		t.Fatal("all-wallet quit projection lost archive hold", summary)
	}
	root := t.TempDir()
	password := filepath.Join(root, "password")
	if err := os.WriteFile(password, []byte("private-action-archive-test"), 0600); err != nil {
		t.Fatal(err)
	}
	vault, err := storage.Open(filepath.Join(root, "state.db"), []byte("private-action-archive-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(e.s); err != nil {
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadStoredActions(Config{Name: e.Config.Name, Network: chain.Regtest, DataDir: root, PasswordFile: password})
	if err != nil || stored.Source != "stored" || len(stored.Actions) != 3 {
		t.Fatal("reopen omitted archived holds", stored, err)
	}
	e.s.Capacity.Reactivating = false
	e.s.Capacity.Invalidated = nil
	e.s.Swaps = nil
	e.chainFresh[chain.BTC] = false
	if w := e.walletActions(now); len(w.Actions) != 0 {
		t.Fatal("ordinary archive checkpoint outage manufactured an obligation", w)
	}
}
