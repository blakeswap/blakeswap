package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

const activityReadBudget = 200 * time.Millisecond

func (e *Engine) refreshActivity(ctx context.Context) {
	if ctx.Err() != nil || !e.activityBusy.CompareAndSwap(false, true) {
		return
	}
	defer e.activityBusy.Store(false)
	e.mu.Lock()
	if e.fatal != nil || e.activityClosed {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	e.activityCancel = cancel
	e.activityReaders.Add(1)
	e.mu.Unlock()
	defer e.activityReaders.Done()
	defer cancel()
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		e.indexActivityChain(ctx, id)
		e.observeActivityChain(ctx, id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil || ctx.Err() != nil {
		return
	}
	e.reconcileActivityReceipts()
	_ = e.save() // Save failure sets the daemon's existing fatal durability gate.
}

func (e *Engine) indexActivityChain(ctx context.Context, id chain.ID) {
	e.mu.Lock()
	if e.fatal != nil || len(e.receiveBook[id]) == 0 {
		e.mu.Unlock()
		return
	}
	if e.s.ActivityIndexes == nil {
		e.s.ActivityIndexes = map[chain.ID]ActivityIndex{}
	}
	index := e.s.ActivityIndexes[id]
	previousIndex := index
	if len(e.s.Activities) >= maxActivityRecords {
		index.Error = "History capacity reached; existing records remain available."
		e.s.ActivityIndexes[id] = index
		e.mu.Unlock()
		return
	}
	entries := e.receiveBook[id]
	if int(index.Address) >= len(entries) {
		index.Address = 0
		index.After = ""
	}
	entry := entries[index.Address]
	wallet, network := e.Config.Name, e.Config.Network
	backend, ok := e.watch[id].(chain.AddressHistorian)
	if !ok {
		index.Error = "This backend does not provide historical spent receipts."
		e.s.ActivityIndexes[id] = index
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, activityReadBudget)
	page, err := backend.AddressHistory(readCtx, entry.address, index.After, 2)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil || ctx.Err() != nil || wallet != e.Config.Name || network != e.Config.Network {
		return
	}
	if err != nil {
		// Endpoint errors may contain private URLs. Expose only an actionable,
		// stable description; transaction absence is never deletion evidence.
		index.Error = "Historical receipts unavailable; indexing will retry."
		e.s.ActivityIndexes[id] = index
		return
	}
	if page.Source == "" || len(page.Transactions) > 2 {
		return
	}
	if !e.activitySourceCurrent(id, page.Generation) {
		index.Error = "Chain source changed; history will be reverified."
		e.s.ActivityIndexes[id] = index
		return
	}
	if index.Source != "" && (index.Source != page.Source || index.Generation != page.Generation) {
		index.Address = 0
		index.After = "" // New source must cover every address.
	} else if page.Complete {
		index.Address++
		index.After = ""
		if int(index.Address) >= len(e.receiveBook[id]) {
			index.Address = 0
			index.CompletedPass = time.Now().Unix()
		}
	} else {
		index.After = page.Next
	}
	index.Source, index.Generation, index.Error = page.Source, page.Generation, ""
	for _, tx := range page.Transactions {
		if err := e.recordActivityReceipt(id, tx, page.Source, page.Generation); err != nil {
			index = previousIndex
			index.Error = "Historical transaction could not be indexed within validation/capacity limits; this pass remains incomplete."
			break
		}
	}
	e.s.ActivityIndexes[id] = index
}

func observationFor(tx chain.Transaction, source string, generation uint64, sequence uint64) ActivityObservation {
	state := "unknown"
	if tx.Confirmations > 0 {
		state = "confirmed"
	} else if tx.Confirmations == 0 {
		state = "mempool"
	} else {
		state = "conflicted"
	}
	return ActivityObservation{Sequence: sequence, TxID: tx.TxID, Status: state, Confirmations: tx.Confirmations, Height: tx.Height, BlockHash: tx.BlockHash, BlockTime: tx.BlockTime, ObservedAt: time.Now().Unix(), Source: source, Generation: generation}
}

func (e *Engine) recordActivityReceipt(id chain.ID, transaction chain.Transaction, source string, generation uint64) error {
	tx, err := contract.Parse(transaction.Hex)
	if err != nil {
		return err
	}
	if tx.TxHash().String() != transaction.TxID {
		return errors.New("history transaction ID mismatch")
	}
	if len(tx.TxIn) > 2048 || len(tx.TxOut) > 2048 {
		return errors.New("history transaction exceeds bounded input/output indexing capacity")
	}
	evidence := ReceiptEvidence{Coinbase: len(tx.TxIn) == 1 && tx.TxIn[0].PreviousOutPoint.Index == ^uint32(0)}
	if !evidence.Coinbase {
		for _, in := range tx.TxIn {
			evidence.Inputs = append(evidence.Inputs, CoinOutpoint{TxID: in.PreviousOutPoint.Hash.String(), Vout: in.PreviousOutPoint.Index})
		}
	}
	addresses := map[string]string{}
	for _, entry := range e.receiveBook[id] {
		addresses[hex.EncodeToString(entry.script)] = entry.address
	}
	newRecords := 0
	for vout, out := range tx.TxOut {
		if out.Value < 0 || out.Value > contract.MaxMoney-evidence.Total {
			return errors.New("invalid history amount")
		}
		evidence.Total += out.Value
		if addresses[hex.EncodeToString(out.PkScript)] != "" {
			evidence.OwnedTotal += out.Value
			key := activityID("receive", string(id)+"/"+pointKey(CoinOutpoint{TxID: transaction.TxID, Vout: uint32(vout)}))
			if _, exists := e.s.Activities[key]; !exists {
				newRecords++
			}
		}
	}
	if newRecords > maxActivityRecords-len(e.s.Activities) {
		return errors.New("history transaction exceeds remaining activity capacity")
	}
	if e.s.ActivityReceipts == nil {
		e.s.ActivityReceipts = map[string]ReceiptEvidence{}
	}
	e.s.ActivityReceipts[string(id)+"/"+transaction.TxID] = evidence
	e.s.ActivityObservationSequence++
	observation := observationFor(transaction, source, generation, e.s.ActivityObservationSequence)
	for vout, out := range tx.TxOut {
		address := addresses[hex.EncodeToString(out.PkScript)]
		if address == "" {
			continue
		}
		point := CoinOutpoint{TxID: transaction.TxID, Vout: uint32(vout)}
		key := activityID("receive", string(id)+"/"+pointKey(point))
		a, exists := e.s.Activities[key]
		if !exists {
			a = Activity{ID: key, GroupID: key, Kind: "receive", Chain: id, Direction: "incoming", Classification: "unclassified_receipt", Movement: true, Amount: out.Value, Principal: out.Value, Address: address, Outpoints: []CoinOutpoint{point}, TxID: transaction.TxID, Variants: []string{transaction.TxID}, Status: observation.Status, Label: "Received payment"}
		}
		a.Observations = []ActivityObservation{observation}
		e.putActivity(a, true)
	}
	return nil
}

func (e *Engine) observeActivityChain(ctx context.Context, id chain.ID) {
	readCtx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	seen := map[string]bool{}
	for attempt := 0; attempt < 8 && readCtx.Err() == nil; attempt++ {
		e.mu.Lock()
		backend, ok := e.nodes[id].(chain.HistoryObserver)
		if !ok || e.fatal != nil {
			e.mu.Unlock()
			return
		}
		ids := []string{}
		for key, a := range e.s.Activities {
			if a.Chain == id && len(a.Variants) > 0 {
				ids = append(ids, key)
			}
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			e.mu.Unlock()
			return
		}
		// Per-chain cursors avoid the other asset's lexicographic position causing
		// either chain to skip the beginning of its ledger indefinitely.
		if e.activityCursors == nil {
			e.activityCursors = map[chain.ID]string{}
			e.activityVariants = map[chain.ID]int{}
		}
		cursor := e.activityCursors[id]
		at := sort.SearchStrings(ids, cursor)
		if at >= len(ids) {
			at = 0
		}
		key := ids[at]
		a := e.s.Activities[key]
		wallet, network := e.Config.Name, e.Config.Network
		variant := e.activityVariants[id]
		if key != cursor || variant >= len(a.Variants) {
			variant = 0
		}
		txid := a.Variants[variant]
		if seen[key+"/"+txid] {
			e.mu.Unlock()
			return
		}
		seen[key+"/"+txid] = true
		var prior ActivityObservation
		for _, o := range a.Observations {
			if o.TxID == txid {
				prior = o
			}
		}
		if variant+1 < len(a.Variants) {
			e.activityCursors[id] = key
			e.activityVariants[id] = variant + 1
		} else {
			e.activityCursors[id] = ids[(at+1)%len(ids)]
			e.activityVariants[id] = 0
		}
		e.mu.Unlock()
		result, err := backend.HistoryTransaction(readCtx, txid, prior.Height, prior.BlockHash)
		e.mu.Lock()
		if e.fatal != nil || ctx.Err() != nil || wallet != e.Config.Name || network != e.Config.Network {
			e.mu.Unlock()
			return
		}
		a, exists := e.s.Activities[key]
		if !exists {
			e.mu.Unlock()
			continue
		}
		if !e.activitySourceCurrent(id, result.Generation) {
			e.mu.Unlock()
			continue // Never attach a late response to the new endpoint generation.
		}
		e.s.ActivityObservationSequence++
		observation := observationFor(result.Transaction, result.Source, result.Generation, e.s.ActivityObservationSequence)
		observation.TxID = txid
		if err != nil || result.Transaction.TxID != txid || result.Source == "" {
			observation.Status = "unknown"
			observation.Confirmations = 0
			observation.Error = "Transaction observation unavailable; previous outcomes are retained."
			if result.PreviousBlockChanged {
				observation.Status = "orphaned"
			}
		} else if result.Transaction.Hex != "" {
			parsed, parseErr := contract.Parse(result.Transaction.Hex)
			if parseErr != nil || parsed.TxHash().String() != txid {
				observation.Status = "unknown"
				observation.Error = "Transaction observation failed validation."
			}
		}
		a.Observations = append([]ActivityObservation{}, a.Observations...)
		updated := false
		for i := range a.Observations {
			if a.Observations[i].TxID == txid {
				a.Observations[i] = observation
				updated = true
			}
			// An observation from an earlier endpoint generation is no longer a
			// current confirmation proof for a competing signed variant.
			if result.Generation > 0 && a.Observations[i].Generation != result.Generation {
				a.Observations[i].Status = "unknown"
			}
		}
		if !updated {
			a.Observations = append(a.Observations, observation)
		}
		e.putActivity(a, true)
		e.mu.Unlock()
	}
}

func (e *Engine) activitySourceCurrent(id chain.ID, generation uint64) bool {
	if source, ok := e.nodes[id].(interface{ Generation() uint64 }); ok {
		return generation > 0 && source.Generation() == generation
	}
	return true
}

// Facts from retained local intents take precedence over inference. Every owned
// output remains discoverable, but change/payout rows link to its one payment.
func (e *Engine) reconcileActivityReceipts() {
	transactions := map[string]Activity{}
	owned := map[string]int64{}
	for _, a := range e.s.Activities {
		if a.Kind == "receive" {
			for _, point := range a.Outpoints {
				owned[string(a.Chain)+"/"+pointKey(point)] = a.Principal
			}
		} else {
			for _, txid := range a.Variants {
				transactions[string(a.Chain)+"/"+txid] = a
			}
		}
	}
	for _, original := range e.s.Activities {
		if original.Kind != "receive" {
			continue
		}
		a := original
		key := string(a.Chain) + "/" + a.TxID
		a.GroupID = a.ID
		a.RelatedIDs = nil
		a.Direction = "incoming"
		a.Movement = true
		a.Classification = "unclassified_receipt"
		a.Label = "Received payment"
		if parent, ok := transactions[key]; ok {
			a.GroupID = parent.GroupID
			a.RelatedIDs = []string{parent.ID}
			a.OrderID, a.SwapID, a.SendID = parent.OrderID, parent.SwapID, parent.SendID
			a.Movement = false
			a.Direction = "internal"
			switch parent.Kind {
			case "send":
				if a.Address == parent.Address {
					a.Classification = "self_transfer"
					a.Label = "Transfer to own address"
				} else {
					a.Classification = "change"
					a.Label = "Send change"
				}
			case "swap_funding":
				a.Classification = "change"
				a.Label = "Funding change"
			case "tower_earning":
				a.Classification = "tower_payout"
				a.Label = "Tower bounty receipt"
			default:
				a.Classification = "swap_payout"
				a.Label = "Swap settlement receipt"
			}
		} else if evidence, ok := e.s.ActivityReceipts[key]; ok && !evidence.Coinbase {
			known := 0
			for _, point := range evidence.Inputs {
				if _, ok := owned[string(a.Chain)+"/"+pointKey(point)]; ok {
					known++
				}
			}
			if known > 0 {
				a.Movement = false
				a.Direction = "internal"
				a.Classification = "unclassified_self_related"
				a.Label = "Receipt from a transaction spending wallet inputs"
				if known == len(evidence.Inputs) && evidence.OwnedTotal == evidence.Total {
					a.Classification = "self_transfer"
					a.Label = "Transfer between known wallet addresses"
				}
			}
		}
		e.putActivity(a, true)
	}
}

// Seed currently known receipts promptly without claiming UTXOs reconstruct old
// history. Historical reads subsequently attach block/source facts and classify.
func (e *Engine) seedActivityCoins() {
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		for _, coin := range e.knownCoins(id) {
			point := CoinOutpoint{TxID: coin.TxID, Vout: coin.Vout}
			key := activityID("receive", string(id)+"/"+pointKey(point))
			if _, exists := e.s.Activities[key]; exists {
				continue
			}
			address := ""
			for _, entry := range e.receiveBook[id] {
				if bytes.Equal(entry.script, mustActivityScript(coin.Script)) {
					address = entry.address
					break
				}
			}
			e.putActivity(Activity{ID: key, GroupID: key, Kind: "receive", Chain: id, Direction: "incoming", Classification: "unclassified_receipt", Movement: true, Amount: int64(coin.Amount), Principal: int64(coin.Amount), Address: address, Outpoints: []CoinOutpoint{point}, TxID: coin.TxID, Variants: []string{coin.TxID}, Status: "unverified", Label: "Received payment"}, true)
		}
	}
}
func mustActivityScript(value string) []byte { b, _ := hex.DecodeString(value); return b }
