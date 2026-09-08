package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// The production manifest's JSON representation. Desktop tests exercise its
// installer/profile/API path; these real-chain tests exercise the same encrypted
// file and daemon gate against funded contracts after the snapshot grows stale.
type recoveryArchive struct {
	FormatVersion int                     `json:"format_version"`
	CreatedAt     int64                   `json:"created_at"`
	Wallets       []recoveryArchiveWallet `json:"wallets"`
}
type recoveryArchiveWallet struct {
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Identity string                   `json:"identity"`
	Mnemonic string                   `json:"mnemonic"`
	Networks map[chain.Network]*State `json:"networks"`
}

const testPortablePassword = "isolated actual-chain portable password"

func snapshotRecoveryArchive(t *testing.T, h *harness, name string) string {
	t.Helper()
	e := h.engines[name]
	state, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	key, err := e.keys.Derive(2, "nostr-identity")
	if err != nil {
		t.Fatal(err)
	}
	identity := sha256.Sum256(key.PubKey().SerializeCompressed())
	entry := recoveryArchiveWallet{ID: name, Name: name, Identity: hex.EncodeToString(identity[:]), Mnemonic: state.Mnemonic, Networks: map[chain.Network]*State{}}
	for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
		entry.Networks[network] = &State{Version: StateVersion, Network: network, Mnemonic: state.Mnemonic}
	}
	entry.Networks[chain.Regtest] = &state
	archive := recoveryArchive{FormatVersion: 1, CreatedAt: time.Now().Unix(), Wallets: []recoveryArchiveWallet{entry}}
	path := filepath.Join(t.TempDir(), "portable.blakeswap")
	if err = storage.WritePortable(h.ctx, path, []byte(testPortablePassword), archive); err != nil {
		t.Fatal(err)
	}
	return path
}
func restoreRecoveryArchive(t *testing.T, h *harness, name, path string) {
	t.Helper()
	h.offline(name)
	var archive recoveryArchive
	if err := storage.ReadPortable(h.ctx, path, []byte(testPortablePassword), &archive); err != nil {
		t.Fatal(err)
	}
	if archive.FormatVersion != 1 || len(archive.Wallets) != 1 {
		t.Fatal("invalid test archive")
	}
	state := archive.Wallets[0].Networks[chain.Regtest]
	if err := PrepareRecovery(state, archive.CreatedAt, false); err != nil {
		t.Fatal(err)
	}
	cfg := h.configs[name]
	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	// A portable import installs a separate vault. Replacing only the active
	// state in the source vault would retain that installation's cold records.
	cfg.DataDir = t.TempDir()
	cfg.PasswordFile = filepath.Join(cfg.DataDir, "pass")
	cfg.Socket = filepath.Join(cfg.DataDir, "daemon.sock")
	if err := os.WriteFile(cfg.PasswordFile, password, 0600); err != nil {
		t.Fatal(err)
	}
	vault, err := storage.Open(filepath.Join(cfg.DataDir, "state.db"), bytes.TrimSpace(password))
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(state); err != nil {
		_ = vault.Close()
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	h.configs[name] = cfg
	h.online(name)
	if h.engines[name].Status().Recovery.State != "recovering" {
		t.Fatal("restored engine opened ready before live reconciliation")
	}
}

func TestRealPortableRestoreWitnessAndReorg(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 0)
			faults := installEndpointFaults(t, h, false)
			id := h.fundBothFees(sell, 0, 6500, 20000)
			maker := h.swap("maker", id)
			incoming, own := maker.Long, maker.Short
			archive := snapshotRecoveryArchive(t, h, "maker")
			h.offline("maker")
			h.online("taker")
			partialWaitMailbox(h, "peer's confirmed first claim", func() bool {
				taker := h.swap("taker", id)
				return taker.SelfClaim != "" && taker.IncomingClaimSeen && taker.ShortConfirmations >= protocol.Confirmations
			}, func() { h.tick("taker"); h.minePending() })
			faults[incoming.Chain].setDown(true)
			restoreRecoveryArchive(t, h, "maker", archive)
			tickDegraded(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if !maker.SecretObserved || maker.SelfClaim != "" {
				t.Fatal("old snapshot failed observe-only recovery", maker.Error)
			}
			h.offline("maker")
			faults[incoming.Chain].setDown(false)
			faults[own.Chain].setDown(true)
			h.online("maker")
			tickDegraded(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if maker.SelfClaim == "" || !maker.SecretObserved {
				t.Fatal("restored witness failed target-only claim", maker.Error)
			}
			tx, err := ancestrySelectedClaim(maker)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.nodes[incoming.Chain].Transaction(h.ctx, tx.TxHash().String()); err != nil {
				t.Fatal("claim not published", err)
			}
			h.mine(incoming.Chain, 2)
			faults[own.Chain].setDown(false)
			tickUntilConnected(t, h.engines["maker"])
			tickUntilConnected(t, h.engines["taker"])
			status := h.engines["maker"].Status()
			if status.Recovery.State != "ready" || h.swap("maker", id).Stage != "completed" || !h.swap("maker", id).IncomingClaimSeen {
				t.Fatal("positive settlement did not complete recovery", status.Recovery)
			}
			var block struct {
				BlockHash string `json:"blockhash"`
			}
			if err = h.nodes[incoming.Chain].Call(h.ctx, "getrawtransaction", &block, tx.TxHash().String(), true); err != nil {
				t.Fatal(err)
			}
			if block.BlockHash == "" {
				t.Fatal("claim block unavailable")
			}
			if err = h.nodes[incoming.Chain].Call(h.ctx, "invalidateblock", nil, block.BlockHash); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[incoming.Chain].Call(h.ctx, "reconsiderblock", nil, block.BlockHash); err != nil {
					t.Error(err)
				}
			}()
			h.offline("maker")
			h.online("maker") // Reopen the persisted completed outcome after the reorg.
			maker = h.swap("maker", id)
			maker.ClaimLastAttempt = 0 // Make the existing signed retry interval eligible.
			tickUntilConnected(t, h.engines["maker"])
			maker = h.swap("maker", id)
			if terminalSwapStage(maker.Stage) || maker.ClaimLastAttempt == 0 {
				t.Fatal("reorged restored claim did not reopen and retry", maker.Stage, maker.Error)
			}
			if h.engines["maker"].Status().Recovery.State == "ready" || !maker.IncomingClaimSeen || !maker.SecretObserved {
				t.Fatal("reorg forgot witness or retained ready state")
			}
			if err = h.engines["maker"].checkRefundAcceleration(h.ctx, maker, maker.Short); err == nil {
				t.Fatal("reorg enabled restored refund after incoming claim")
			}
			h.minePending()
			tickUntilConnected(t, h.engines["maker"])
			if h.engines["maker"].Status().Recovery.State != "ready" || h.swap("maker", id).Stage != "completed" {
				t.Fatal("authorized restored claim did not reconfirm after reorg", h.engines["maker"].Status().Recovery)
			}
			t.Logf("portable restored %s claim %s; positive ready -> reorg signed retry -> reconfirmed ready", sell, tx.TxHash())
		})
	}
}

func TestRealPortableRestorePreservesRefunds(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 0)
			id := h.fundBothFees(sell, 0, 6500, 20000)
			h.online("taker") // Opening alone must not publish its private secret.
			taker := h.swap("taker", id)
			crashed := false
			e := h.engines["taker"]
			e.nodes[taker.Short.Chain] = &fundingCrashBackend{Backend: e.nodes[taker.Short.Chain], before: func(string) { crashed = true }}
			// Keep the interceptor installed while the restarted mailbox catches
			// up. No preliminary tick may publish this deliberately private claim.
			partialWaitMailbox(h, "private claim crash boundary", func() bool { return crashed }, func() {
				defer func() {
					if r := recover(); r != nil && r != "simulated funding crash" {
						t.Fatalf("unexpected private claim crash: %v", r)
					}
				}()
				_ = e.Tick(h.ctx)
			})
			taker = h.swap("taker", id)
			if !crashed || taker.SelfClaim == "" || taker.SecretObserved {
				t.Fatal("private claim crash boundary was not reached")
			}
			private, err := contract.Parse(taker.SelfClaim)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.nodes[taker.Short.Chain].Transaction(h.ctx, private.TxHash().String()); !chain.TransactionNotFound(err) {
				t.Fatal("private claim was published before restore", err)
			}
			archive := snapshotRecoveryArchive(t, h, "taker")
			refunds := append([]string(nil), taker.SelfRefunds...)
			own, incoming := taker.Long, taker.Short
			restoreRecoveryArchive(t, h, "taker", archive)
			tickUntilConnected(t, h.engines["taker"])
			taker = h.swap("taker", id)
			if taker.SecretObserved || taker.SelfClaim != contract.Hex(private) || h.engines["taker"].Status().Recovery.State == "ready" {
				t.Fatal("private restored snapshot became ready or revealed")
			}
			if _, err = h.nodes[incoming.Chain].Transaction(h.ctx, private.TxHash().String()); !chain.TransactionNotFound(err) {
				t.Fatal("restored private claim reached chain", err)
			}
			if height := h.height(incoming.Chain); height < incoming.RefundHeight {
				h.mine(incoming.Chain, incoming.RefundHeight-height)
			}
			h.tick("maker")
			h.minePending()
			h.tick("maker")
			if height := h.height(own.Chain); height < own.RefundHeight {
				h.mine(own.Chain, own.RefundHeight-height)
			}
			tickUntilConnected(t, h.engines["taker"])
			h.minePending()
			tickUntilConnected(t, h.engines["taker"])
			h.tick("maker")
			taker = h.swap("taker", id)
			if taker.Stage != "refunded" || h.engines["taker"].Status().Recovery.State != "ready" || taker.IncomingClaimSeen || taker.SecretObserved {
				t.Fatal("positive refund recovery failed", taker.Stage, taker.Error, h.engines["taker"].Status().Recovery)
			}
			if len(refunds) != len(taker.SelfRefunds) || taker.OwnerFeeCap != 20000 {
				t.Fatal("restore changed authorized refund ladder")
			}
			for i := range refunds {
				if refunds[i] != taker.SelfRefunds[i] {
					t.Fatal("restore rewrote signed refund")
				}
			}
			record, err := h.nodes[own.Chain].Transaction(h.ctx, taker.LongSpend)
			if err != nil || record.Confirmations < 2 {
				t.Fatal("restored refund not confirmed", err)
			}
			t.Logf("portable restored %s refund %s with persisted fee cap", sell, taker.LongSpend)
		})
	}
}

func TestRealPortableRestoreBeforeFundingPublication(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
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
			e.nodes[fundingChain] = &fundingCrashBackend{Backend: e.nodes[fundingChain], before: func(string) { crashed = true }}
			func() {
				defer func() {
					if r := recover(); r != "simulated funding crash" {
						t.Fatalf("unexpected funding crash: %v", r)
					}
				}()
				_ = e.Tick(h.ctx)
			}()
			if !crashed {
				t.Fatal("private funding boundary not reached")
			}
			snapshot := snapshotRecoveryArchive(t, h, role)
			before := h.swap(role, id)
			raw := before.LongFunding
			if role == "maker" {
				raw = before.ShortFunding
			}
			tx, err := contract.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			other := "maker"
			if role == "maker" {
				other = "taker"
			}
			h.offline(other)
			restoreRecoveryArchive(t, h, role, snapshot)
			tickUntilConnected(t, h.engines[role])
			tickUntilConnected(t, h.engines[role])
			if _, err = h.nodes[fundingChain].Transaction(h.ctx, tx.TxHash().String()); !chain.TransactionNotFound(err) {
				t.Fatal("restore published private funding", err)
			}
			if h.engines[role].Status().Recovery.State == "ready" || h.swap(role, id).SecretObserved || len(h.engines[role].s.Offers) > 0 {
				t.Fatal("prepublication restore bypassed quarantine or reconciliation")
			}
			t.Logf("portable %s restore retained signed %s privately and held uncertain funding", role, tx.TxHash())
		})
	}
}

func TestRealPortableTowerRecovery(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 50)
			faults := installEndpointFaults(t, h, false)
			id := h.fundBothFees(sell, 50, 6500, 20000)
			maker := h.swap("maker", id)
			target, observe := maker.Long, maker.Short
			archive := snapshotRecoveryArchive(t, h, "tower")
			h.offline("tower")
			h.offline("maker")
			h.online("taker")
			partialWaitMailbox(h, "peer's confirmed tower-observed claim", func() bool {
				taker := h.swap("taker", id)
				return taker.SelfClaim != "" && taker.IncomingClaimSeen && taker.ShortConfirmations >= protocol.Confirmations
			}, func() { h.tick("taker"); h.minePending() })
			claimID, _ := settlementVariant(h.swap("taker", id).SelfClaims, h.swap("taker", id).ClaimVariant, maker.Long, maker.Short, false)
			claimRecord, err := h.nodes[observe.Chain].Transaction(h.ctx, claimID)
			if err != nil || claimRecord.BlockHash == "" {
				t.Fatal("missing real revealing claim", err)
			}
			faults[target.Chain].setDown(true)
			restoreRecoveryArchive(t, h, "tower", archive)
			jobID := ""
			for key, state := range h.engines["tower"].s.TowerJobs {
				if state.Job.SwapID == id && state.Job.Kind == "claim" {
					if jobID != "" || state.Job.Target != target || state.Job.Observe == nil || *state.Job.Observe != observe {
						t.Fatal("fixture claim job does not uniquely match the restored contracts")
					}
					jobID = key
				}
			}
			if jobID == "" {
				t.Fatal("fixture has no authorized tower claim")
			}
			learned := h.engines["tower"].s.TowerJobs[jobID]
			if learned.Secret != "" {
				t.Fatal("archive unexpectedly contains the later public witness")
			}
			// Restoring resets historical cursors. An incomplete first scan is not
			// evidence that the already-mined claim has been examined and forgotten.
			observed := &captureTowerScan{SpendScanner: h.engines["tower"].towerScanners[observe.Chain]}
			if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "" {
				observed.firstBudget = 200 * time.Millisecond
			}
			h.engines["tower"].towerScanners[observe.Chain] = observed
			tickDegraded(t, h.engines["tower"])
			if observed.firstBudget > 0 {
				if observed.result != nil || observed.err == nil || !strings.Contains(observed.err.Error(), "block scan progress retained") || !h.engines["tower"].fresh(observe.Chain) {
					t.Fatal("first observation slice did not retain healthy bounded progress", observed.err)
				}
				t.Logf("observation %s scan retained progress while target was unavailable: %v", observe.Chain, observed.err)
			}
			witnessCtx, cancelWitness := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelWitness()
			for cycle := 0; learned.Secret == "" && cycle < 12 && witnessCtx.Err() == nil; cycle++ {
				if learned.Broadcast != "" {
					t.Fatal("tower published while target was unavailable")
				}
				time.Sleep(100 * time.Millisecond)
				tickDegradedContext(t, h.engines["tower"], witnessCtx)
			}
			if learned.Secret == "" || learned.Broadcast != "" {
				t.Fatal("tower failed bounded witness recovery while target unavailable", learned.Error)
			}
			archive = snapshotRecoveryArchive(t, h, "tower")
			h.offline("tower")
			if err := h.nodes[observe.Chain].Call(h.ctx, "invalidateblock", nil, claimRecord.BlockHash); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[observe.Chain].Call(h.ctx, "reconsiderblock", nil, claimRecord.BlockHash); err != nil {
					t.Error(err)
				}
			}()
			faults[observe.Chain].setDown(true)
			faults[target.Chain].setDown(false)
			h.mine(target.Chain, maker.Terms.Takeover+1-h.height(target.Chain))
			restoreRecoveryArchive(t, h, "tower", archive)
			towerEngine := h.engines["tower"]
			captured := &captureTowerScan{SpendScanner: towerEngine.towerScanners[target.Chain]}
			if os.Getenv("BLAKESWAP_TEST_ELECTRUM") == "" {
				captured.firstBudget = 200 * time.Millisecond
			}
			towerEngine.towerScanners[target.Chain] = captured
			expectedSecret := towerEngine.s.TowerJobs[jobID].Secret
			if expectedSecret == "" {
				t.Fatal("durable witness missing before target catch-up")
			}
			tickDegraded(t, towerEngine)
			state := towerEngine.s.TowerJobs[jobID]
			if captured.firstBudget > 0 {
				if captured.result != nil || captured.err == nil || !strings.Contains(captured.err.Error(), "block scan progress retained") {
					t.Fatal("first target slice did not exercise bounded historical progress", captured.err)
				}
				if state.Broadcast != "" || state.Secret != expectedSecret || !towerEngine.fresh(target.Chain) {
					t.Fatal("incomplete target scan published, lost witness, or poisoned wallet readiness", state.Error)
				}
				t.Logf("target %s scan retained progress and held publication: %v", target.Chain, captured.err)
			}
			// Catch-up intentionally spans worker cycles. Keep each existing tick
			// bound and require eventual publication within both a wall deadline
			// and a finite number of paced cycles; the witness must never vanish.
			catchupCtx, cancelCatchup := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelCatchup()
			for cycle := 0; state.Broadcast == "" && cycle < 12 && catchupCtx.Err() == nil; cycle++ {
				if state.Secret != expectedSecret {
					t.Fatal("tower lost witness during target catch-up")
				}
				time.Sleep(100 * time.Millisecond)
				tickDegradedContext(t, towerEngine, catchupCtx)
			}
			if state.Secret != expectedSecret || state.Broadcast == "" {
				t.Fatal("tower failed bounded recovery after restart/outage", state.Error)
			}
			record, err := h.nodes[target.Chain].Transaction(h.ctx, state.Broadcast)
			if err != nil {
				t.Fatal("tower claim not published to healthy chain", err)
			}
			tx, err := contract.Parse(record.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := contract.ExtractSecret(target, tx); !ok || tx.TxOut[1].Value != protocol.Bounty(target.Amount, 50) {
				t.Fatal("tower claim changed preimage or bounty authorization")
			}
			h.mine(target.Chain, 2)
			tickDegraded(t, h.engines["tower"])
			if state.Confirmed < 2 {
				t.Fatal("target-only tower claim did not confirm")
			}
			for _, restoredJob := range towerEngine.s.TowerJobs {
				if restoredJob.Job.Kind == "refund" && restoredJob.Attempt != 0 {
					t.Fatal("restored standalone refund published")
				}
			}
			t.Logf("tower retained reorged witness and claimed %s on %s", state.Broadcast, target.Chain)
		})
	}
}

func TestRealPortablePaymentVariantsAndReceiveIndexes(t *testing.T) {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(id), func(t *testing.T) {
			h := newHarness(t, 0)
			e := h.engines["maker"]
			indexes := map[chain.ID]uint32{}
			for c, index := range e.s.ReceiveIndexes {
				indexes[c] = index
			}
			var inputs []CoinOutpoint
			for _, coin := range e.knownCoins(id) {
				inputs = append(inputs, CoinOutpoint{coin.TxID, coin.Vout})
			}
			request := SendRequest{ID: transport.RandomID(), Chain: id, Destination: h.engines["taker"].addresses[id], Amount: 1000000, Fee: 1, MaxFee: 20000, Inputs: inputs, ExpectedNetwork: "regtest"}
			raw, _ := json.Marshal(request)
			sent, err := e.sendCoins(h.ctx, raw)
			if err != nil || sent.Submitted {
				t.Fatal("expected preserved below-relay transaction", err, sent)
			}
			// Rejection places the selected endpoint in backoff. Establish the
			// existing bounded positive-readiness precondition before acceleration;
			// an immediate replacement must not assume the failed source is ready.
			tickUntilConnected(t, e)
			bumped, err := e.bumpSend(h.ctx, BumpRequest{ID: request.ID, Kind: "send", Fee: 6500, ExpectedTxID: sent.TxID})
			if err != nil || bumped.Error != "" {
				t.Fatal(err, bumped)
			}
			original := e.s.Sends[request.ID]
			history, _ := json.Marshal(original.History)
			archive := snapshotRecoveryArchive(t, h, "maker")
			h.offline("maker")
			h.mine(id, 2) // Snapshot still says unconfirmed; actual payment now settled.
			restoreRecoveryArchive(t, h, "maker", archive)
			e = h.engines["maker"]
			if e.recoveryTradingReady() == nil {
				t.Fatal("payment restored ready before observation")
			}
			tickUntilConnected(t, e)
			restored := e.s.Sends[request.ID]
			if restored.TxID != bumped.TxID || restored.Confirmations < 2 || e.Status().Recovery.State != "ready" {
				t.Fatal("recorded replacement did not reconcile", restored.public(), e.Status().Recovery)
			}
			if len(restored.History) != 2 || restored.MaxFee != 20000 {
				t.Fatal("lost signed payment lineage/limit")
			}
			// Only observation fields change; exact authorizations survive the file.
			var before []SignedVariant
			if err := json.Unmarshal(history, &before); err != nil {
				t.Fatal(err)
			}
			for i, v := range before {
				if restored.History[i].Raw != v.Raw || restored.History[i].TxID != v.TxID || restored.History[i].Fee != v.Fee {
					t.Fatal("restored variant changed")
				}
			}
			for c, index := range indexes {
				if e.s.ReceiveIndexes[c] < index {
					t.Fatal("receive derivation moved backwards")
				}
			}
			if result, err := e.sendCoins(h.ctx, raw); err != nil || result.TxID != restored.TxID || len(e.s.Sends) != 1 {
				t.Fatal("exact original request duplicated payment", err)
			}
			record, err := h.nodes[id].Transaction(h.ctx, restored.TxID)
			if err != nil || record.BlockHash == "" {
				t.Fatal(err)
			}
			if err := h.nodes[id].Call(h.ctx, "invalidateblock", nil, record.BlockHash); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[id].Call(h.ctx, "reconsiderblock", nil, record.BlockHash); err != nil {
					t.Error(err)
				}
			}()
			tickUntilConnected(t, e)
			if e.Status().Recovery.State != "recovering" || restored.Confirmations != 0 {
				t.Fatal("reorg retained payment readiness", e.Status().Recovery, restored.public())
			}
			if !e.reservedCoins(id, "")[pointKey(inputs[0])] {
				t.Fatal("reorg released exact payment inputs")
			}
		})
	}
}

func TestRealPortableTowerRefundObservesConfirmedOutcome(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			h := newHarness(t, 50)
			id := h.fundBothFees(sell, 50, 6500, 20000)
			target := h.swap("maker", id).Short
			h.offline("maker")
			h.offline("taker")
			tower := h.engines["tower"]
			var jobID string
			for key, state := range tower.s.TowerJobs {
				if state.Job.SwapID == id && state.Job.Kind == "refund" && state.Job.Target == target {
					if jobID != "" {
						t.Fatal("duplicate target refund job")
					}
					jobID = key
				}
			}
			if jobID == "" {
				t.Fatal("missing actual signed tower refund")
			}
			// This portable profile retained one authorized recovery job. Its
			// publication and later confirmation occur after the exported snapshot.
			job := tower.s.TowerJobs[jobID].Job
			tower.s.TowerJobs = map[string]*TowerJob{jobID: tower.s.TowerJobs[jobID]}
			archive := snapshotRecoveryArchive(t, h, "tower")
			h.offline("tower")
			if height := h.height(target.Chain); height <= job.Lock {
				h.mine(target.Chain, job.Lock+1-height)
			}
			raw := job.Templates[len(job.Templates)-1]
			tx, err := contract.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.nodes[target.Chain].Broadcast(h.ctx, raw); err != nil {
				t.Fatal("previously authorized tower refund rejected", err)
			}
			h.mine(target.Chain, 6)
			restoreRecoveryArchive(t, h, "tower", archive)
			tower = h.engines["tower"]
			state := tower.s.TowerJobs[jobID]
			// Both endpoints are healthy. A normal tick can complete immediately,
			// or retain bounded tower catch-up in LastError/Recovery without a
			// protocol error. Require the positive outcome instead of an outage.
			tick := func(parent context.Context) error {
				started := time.Now()
				ctx, cancel := context.WithTimeout(parent, 12*time.Second)
				defer cancel()
				err := tower.Tick(ctx)
				if time.Since(started) > 11*time.Second {
					t.Fatal("tower reconciliation monopolized recovery cycle")
				}
				if err != nil {
					t.Logf("bounded tower reconciliation: %v", err)
				}
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var tickErr error
			for cycle := 0; cycle < 12 && ctx.Err() == nil; cycle++ {
				tickErr = tick(ctx)
				if state.LastAttempt != 0 || state.Attempt != 0 {
					t.Fatal("restored refund was published during reconciliation")
				}
				if tickErr == nil && state.Confirmed >= 6 && tower.Status().Recovery.State == "ready" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if tickErr != nil || state.Confirmed < 6 || state.Broadcast != tx.TxHash().String() || tower.Status().Recovery.State != "ready" || tower.CanChangeNetwork() != nil {
				t.Fatal("confirmed restored tower job remained active", tickErr, state.Error, tower.Status().Recovery, tower.CanChangeNetwork())
			}
			activity, found := tower.s.Activities[activityID("tower", jobID)]
			if !found || activity.TxID != state.Broadcast || activity.LocalStatus != "confirmed" || activity.Amount != protocol.Bounty(target.Amount, job.BPS) {
				t.Fatal("restored bounty history lost actual settled outcome", activity)
			}
			record, err := h.nodes[target.Chain].Transaction(h.ctx, state.Broadcast)
			if err != nil || record.BlockHash == "" {
				t.Fatal("confirmed refund block unavailable", err)
			}
			if err = h.nodes[target.Chain].Call(h.ctx, "invalidateblock", nil, record.BlockHash); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.nodes[target.Chain].Call(h.ctx, "reconsiderblock", nil, record.BlockHash); err != nil {
					t.Error(err)
				}
			}()
			_ = tick(context.Background())
			// A bounded scan may still display the prior confirmed outcome. The
			// known checkpoint contradiction must nevertheless hold monitoring
			// immediately, before target catch-up can reconcile that display.
			if state.LastAttempt != 0 || state.Attempt != 0 || tower.CanChangeNetwork() == nil || tower.Status().Recovery.State == "ready" {
				t.Fatal("tower refund reorg failed to immediately hold monitoring", state.Error, tower.Status().Recovery)
			}
			reorgCtx, reorgCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer reorgCancel()
			for cycle := 0; state.Confirmed != 0 && cycle < 12 && reorgCtx.Err() == nil; cycle++ {
				_ = tick(reorgCtx)
				if state.LastAttempt != 0 || state.Attempt != 0 || tower.CanChangeNetwork() == nil || tower.Status().Recovery.State == "ready" {
					t.Fatal("tower refund lost its hold during reorg catch-up", state.Error, tower.Status().Recovery)
				}
			}
			if state.Confirmed != 0 {
				t.Fatal("tower refund display did not reconcile the reorg within bounded work", state.Error, tower.Status().Recovery)
			}
			t.Logf("restored %s tower refund %s observed without publication; bounty retained and reorg reopened recovery", target.Chain, state.Broadcast)
		})
	}
}
