package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
)

// Full engine acceptance workload. All files and envelope keys are disposable;
// checkpoint observations are deterministic private fixtures, not real chains.
func TestArchiveLifetimeWorkloadAndMailboxSettlementBudget(t *testing.T) {
	if os.Getenv("BLAKESWAP_ARCHIVE_SCALE") != "1" {
		t.Skip("explicit combined engine scale acceptance")
	}
	archiveScaleWorkload(t, 1001, 10017)
}

func TestArchiveScaleCurrentFixture(t *testing.T) {
	t.Run("original_mini", func(t *testing.T) { archiveScaleWorkload(t, 3, 3) })
	t.Run("batch_boundary", func(t *testing.T) { archiveScaleWorkload(t, 17, 65) })
}

func archiveScaleWorkload(t *testing.T, completed, messages int) {
	ctx := context.Background()
	e, s, _, _ := isolatedTowerFixture(t)
	t.Cleanup(func() {
		e.nodes = nil // In-memory backends have no transport Close implementation.
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	e.Config.Mode = "trader"
	e.Config.Name = "archive-scale"
	// This separately initialized installation has no participant custody.
	// Allocate its optional maps without clearing the participant's saved state.
	e.s.Swaps = map[string]*Swap{}
	e.s.Sends = map[string]*WalletSend{}
	e.s.TowerJobs = map[string]*TowerJob{}
	e.s.Offers = map[string]nostr.Event{}
	e.s.Book = map[string]nostr.Event{}
	e.s.Outbox = map[string]*Delivery{}
	e.s.Seen = map[string]string{}
	e.s.OrderRecords = map[string]OrderRecord{}
	e.chainFresh = map[chain.ID]bool{chain.BTC: true, chain.Blake: true}
	backends := map[chain.ID]*receiveBackend{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		b := &receiveBackend{used: map[string]bool{}}
		backends[id] = b
		e.nodes[id] = b
		e.watch[id] = b
		e.heights[id] = 200
		if err := e.refreshArchiveCheckpoint(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	target := s.Short
	s.Protection = &protocol.Tower{PubKey: e.identity.Public().Hex(), BPS: 100, Scripts: map[chain.ID]string{chain.BTC: hex.EncodeToString(e.scripts[chain.BTC]), chain.Blake: hex.EncodeToString(e.scripts[chain.Blake])}}
	jobTemplate, err := e.makeJob(s, target, "refund", nil, target.RefundHeight+protocol.RefundDelay(e.Config.Network))
	if err != nil {
		t.Fatal(err)
	}
	key, err := e.swapKey(target.Chain, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	spend, err := contract.Spend(target, key, e.scripts[target.Chain], 2000, true, target.RefundHeight, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	observations := map[chain.ID]map[string]chain.Observation{chain.BTC: {}, chain.Blake: {}}
	observations[target.Chain][chain.OutpointKey(target.TxID, target.Vout)] = chain.Observation{Tx: spend, Height: 1, Confirmations: 200}
	for i := 0; i < completed; i++ {
		id := fmt.Sprintf("%064x", i+1)
		chainID := chain.BTC
		if i%2 != 0 {
			chainID = chain.Blake
		}
		entry := e.receiveBook[chainID][0]
		coins := []chain.UTXO{{TxID: id, Amount: 1000000, Script: hex.EncodeToString(entry.script), Confirmations: 200}}
		payment, err := contract.PayWithKeys(chainID, 500000, entry.script, coins, map[string]*btcec.PrivateKey{coins[0].Script: entry.key}, entry.script, 2000)
		if err != nil {
			t.Fatal(err)
		}
		raw := contract.Hex(payment)
		send := &WalletSend{PublicSend: PublicSend{ID: id, Chain: chainID, TxID: payment.TxHash().String(), Confirmations: 200, State: "confirmed"}, Raw: raw, History: []SignedVariant{{PublicVariant: PublicVariant{TxID: payment.TxHash().String(), Confirmations: 200}, Raw: raw}}}
		e.s.Sends[id] = send
		e.noteArchivePayment(send, send.TxID, chain.Transaction{TxID: send.TxID, Hex: raw, Height: 1, Confirmations: 200})
		// Separate signed registrations may authorize the same target. Their IDs
		// and original receipts remain distinct durable obligations.
		job := jobTemplate
		job.ID = id
		if err := job.Validate(s.Protection.Scripts, s.Protection.BPS); err != nil {
			t.Fatal(err)
		}
		e.s.TowerJobs[id] = &TowerJob{Job: job, Confirmed: 200}
		scaleCompletedOrder(t, e, chainID, 1000000, 2000000)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cycles := 0
	for len(e.s.Sends)+len(e.s.TowerJobs)+len(e.s.Offers)+len(e.s.ParentOrders) != 0 {
		if err := e.compactArchive(ctx, nil, observations); err != nil {
			t.Fatal(err)
		}
		if err := e.save(); err != nil {
			t.Fatal(err)
		}
		cycles++
		if cycles > 100 {
			t.Fatal("compaction stopped making bounded progress")
		}
	}
	t.Logf("physically_archived_sends=%d offers=%d tower_jobs=%d cycles=%d elapsed=%s active_bytes=%d retained_bytes=%d", completed, completed, completed, cycles, time.Since(start), e.stateBytes, e.s.Capacity.Archived.Bytes)
	if len(e.s.Book) > 100 || len(e.Status().Orders) > 100 {
		t.Fatalf("completed own offers remain in the active public/status view: book=%d status=%d", len(e.s.Book), len(e.Status().Orders))
	}
	if err := ValidateVaultProtocolState(e.vault, &e.s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"sends", "offers", "tower_jobs", "parent_orders", "fill_records", "swaps"} {
		if e.s.Capacity.Archived.Kinds[kind] != uint64(completed) {
			t.Fatal("physical compaction lost retained population", kind)
		}
	}
	page := scaleFilledMarket(t, e)
	if page.Total != completed || len(page.Records) != 1 {
		t.Fatal("physically archived filled orders lost exact custody", page.Total)
	}
	if err := e.admitWork("send"); err != nil {
		t.Fatal("finished lifetime history blocked new work", err)
	}

	peer := nostr.Generate()
	events := make([]nostr.Event, messages)
	for i := range events {
		id := fmt.Sprintf("%064x", i+100000)
		body, _ := json.Marshal(map[string]string{"id": id, "digest": id})
		message := transport.Message{Version: transport.MessageVersion, ID: id, Type: "ack", SwapID: s.ID, Body: body}
		event, err := transport.WrapFor(e.Config.Network.Namespace(), peer, e.identity.Public(), message)
		if err != nil {
			t.Fatal(err)
		}
		events[i] = event
		e.s.Seen[peer.Public().Hex()+":"+id] = protocol.Digest(message)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	for len(e.s.Seen) != 0 {
		if err := e.compactArchive(ctx, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.save(); err != nil {
			t.Fatal(err)
		}
	}
	if e.s.Capacity.Archived.Kinds["seen"] != uint64(messages) {
		t.Fatal("processed mailbox identity lost")
	}
	e.relayCancel = func() {}
	w := &relayWorker{key: "private-fixture\nmailbox", name: "mailbox", pages: make(chan *relayHistoryBatch, 1), live: make(chan nostr.Event, 32), states: make(chan relayLiveState, 4)}
	e.relayWorkers = []*relayWorker{w}
	e.s.RelaySync = map[string]RelaySyncRecord{w.key: {}}
	e.scanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	e.towerScanners = map[chain.ID]chain.SpendScanner{chain.BTC: &recordingScanner{}, chain.Blake: &recordingScanner{}}
	var cold WalletSend
	if found, err := e.archivedValue("sends", fmt.Sprintf("%064x", 1), &cold); err != nil || !found {
		t.Fatal(err)
	}
	cold.ID = "new-active-payment"
	cold.Confirmations = 0
	cold.History[0].Confirmations = 0
	cold.History[0].Submitted = false
	e.s.Sends[cold.ID] = &cold
	broadcasts := 0
	e.nodes[chain.BTC] = &sendBackend{receiveBackend: backends[chain.BTC], broadcast: func(string) (string, error) { broadcasts++; return cold.TxID, nil }}
	var maxTick, maxStatus time.Duration
	ticks := 0
	start = time.Now()
	for offset := 0; offset < len(events); {
		end := min(offset+transport.RelayPageSize, len(events))
		batch := &relayHistoryBatch{page: transport.RelayPage{Events: events[offset:end], Next: transport.RelayCursor{Until: nostr.Timestamp(end)}}, ack: make(chan struct{})}
		w.pages <- batch
		for {
			cold.LastAttempt = 0
			before := batch.offset
			began := time.Now()
			if err := e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			maxTick = max(maxTick, time.Since(began))
			ticks++
			if batch.offset-before > relayTickEvents {
				t.Fatal("local tick exceeded mailbox event budget")
			}
			if broadcasts != ticks {
				t.Fatalf("settlement starved behind mailbox replay: tick=%d broadcasts=%d", ticks, broadcasts)
			}
			began = time.Now()
			status := e.Status()
			maxStatus = max(maxStatus, time.Since(began))
			if len(status.Sends) > 101 || len(status.TowerJobs) > 100 || len(status.Orders) > 100 {
				t.Fatal("status grew with lifetime history")
			}
			select {
			case <-batch.ack:
				goto nextPage
			default:
			}
		}
	nextPage:
		if got := e.s.RelaySync[w.key]; got.Error != "" || got.Cursor.Until != nostr.Timestamp(end) {
			t.Fatal("mailbox replay incomplete", got)
		}
		offset = end
	}
	t.Logf("mailbox_events=%d ticks=%d elapsed=%s max_tick=%s max_status=%s broadcasts=%d active_bytes=%d retained_bytes=%d", messages, ticks, time.Since(start), maxTick, maxStatus, broadcasts, e.stateBytes, e.s.Capacity.Archived.Bytes)
	if maxTick > 2*time.Second || maxStatus > 250*time.Millisecond {
		t.Fatal("local scale execution budget exceeded", maxTick, maxStatus)
	}
}

// Construct already-settled query custody with exact signed parent/child
// identities and the reviewed pure allocation transitions. Explicitly placing
// the child cold is setup, not evidence of chain finality or hot-swap compaction.
// The caller still exercises ordinary compaction of the retained parent offer.
func scaleCompletedOrder(t *testing.T, e *Engine, sell chain.ID, sellAmount, buyAmount int64) protocol.Offer {
	t.Helper()
	btc, blake := sellAmount, buyAmount
	if sell == chain.Blake {
		btc, blake = buyAmount, sellAmount
	}
	offer := marketOffer(t, e, true, sell, btc, blake, "filled", time.Now().Unix()+3600)
	event := e.s.Offers[offer.ID]
	// Model only this completed offer's drained publication; never discard an
	// unrelated pending message or an established child's durable authority.
	delete(e.s.Outbox, event.ID.Hex())
	record := e.s.OrderRecords[offer.ID]
	if len(record.Settlements) != 1 {
		t.Fatal("completed whole order must retain exactly one allocated child")
	}
	for id := range record.Settlements {
		if err := e.stageArchive("swaps", id); err != nil {
			t.Fatal(err)
		}
	}
	return offer
}

func scaleFilledMarket(t *testing.T, e *Engine) MarketPage {
	t.Helper()
	raw, _ := json.Marshal(MarketQuery{ExpectedWallet: e.Config.Name, ExpectedNetwork: string(e.Config.Network), Owner: "mine", Status: "filled", Limit: 1})
	result, err := e.historyCommand(context.Background(), Request{Method: "market.list", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return result.(MarketPage)
}
