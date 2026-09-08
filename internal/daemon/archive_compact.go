package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

// Recent settlement remains in the frequently scanned tier. This depth is a
// storage policy, never an assertion of irreversibility: every archived chain
// prefix has a canonical anchor and any contradiction reactivates its evidence.
const archiveSettlementDepth = 144

func sortedArchiveIDs[T any](values map[string]T) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (e *Engine) archiveHold() error {
	if e.s.Capacity != nil && (e.s.Capacity.Reactivating || len(e.s.Capacity.Invalidated) != 0) {
		return errors.New("archived chain evidence is being reactivated; keep monitoring before admitting new work or changing networks")
	}
	for id, anchor := range e.archiveAnchors() {
		if anchor.Hash != "" && (!e.fresh(id) || e.archiveCurrent[id].Hash == "") {
			return errors.New("archived settlement checkpoints require current canonical chain observations")
		}
	}
	return nil
}
func (e *Engine) archiveAnchors() map[chain.ID]ArchiveAnchor {
	if e.s.Capacity == nil {
		return nil
	}
	return e.s.Capacity.Anchors
}

func (e *Engine) invalidateArchive(reason string) error {
	e.archiveCurrent = nil
	e.archiveSends = nil
	if e.s.Capacity == nil || e.s.Capacity.Archived.Count == 0 {
		return nil
	}
	e.s.Capacity.Reactivating = true
	e.s.Capacity.Reason = reason
	e.archiveCurrent = nil
	e.archiveSends = nil
	// Persist the monitoring hold before any bounded reactivation can stop.
	return e.save()
}

func (e *Engine) refreshArchiveCheckpoint(ctx context.Context, id chain.ID) error {
	delete(e.archiveCurrent, id)
	source, ok := e.nodes[id].(recoveryBlockHasher)
	if !ok {
		if len(e.archiveAnchors()) != 0 {
			return errors.New("archive monitoring requires canonical block hashes")
		}
		return nil // Unsupported backends cannot compact funded obligations.
	}
	if e.archiveCurrent == nil {
		e.archiveCurrent = map[chain.ID]recoveryCheckpoint{}
	}
	delete(e.archiveCurrent, id)
	height := e.heights[id]
	hash, err := source.BlockHash(ctx, height)
	if err != nil || hash == "" {
		return errors.Join(err, errors.New("archive checkpoint unavailable"))
	}
	anchor := e.archiveAnchors()[id]
	if anchor.Hash != "" {
		if height < anchor.Height {
			return e.invalidateArchive("The chain rewound across an archived settlement checkpoint.")
		}
		ancestor, err := source.BlockHash(ctx, anchor.Height)
		if err != nil || ancestor == "" {
			return errors.Join(err, errors.New("archived ancestor unavailable"))
		}
		if ancestor != anchor.Hash {
			return e.invalidateArchive("A canonical block hash contradicted archived settlement evidence.")
		}
	}
	current, err := source.BlockHash(ctx, height)
	if err != nil || current != hash {
		return errors.Join(err, errors.New("chain changed during archive checkpoint validation"))
	}
	generation := e.chainGeneration[id]
	if pool, ok := e.nodes[id].(*chain.Failover); ok {
		generation = pool.Generation()
	}
	e.archiveCurrent[id] = recoveryCheckpoint{Height: height, Hash: hash, Generation: generation}
	return nil
}

func (e *Engine) noteArchivePayment(send *WalletSend, expected string, tx chain.Transaction) {
	checkpoint := e.archiveCurrent[send.Chain]
	if checkpoint.Hash == "" || tx.Height == 0 || tx.Height > checkpoint.Height || tx.Confirmations < e.Config.Network.Confirmations() || !e.fresh(send.Chain) {
		return
	}
	raw, err := contract.Parse(tx.Hex)
	if err != nil || raw.TxHash().String() != expected || (tx.TxID != "" && tx.TxID != expected) {
		return
	}
	if e.archiveSends == nil {
		e.archiveSends = map[string]bool{}
	}
	e.archiveSends[send.ID] = true
}

func (e *Engine) compactArchive(ctx context.Context, swaps, towers map[chain.ID]map[string]chain.Observation) error {
	if e.s.Capacity != nil && e.s.Capacity.Reactivating {
		return e.reactivateArchive()
	}
	// Recheck the exact pre-scan anchors. A mixed fork must never qualify an
	// obligation for removal from the frequently scanned set.
	valid := map[chain.ID]bool{}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		point := e.archiveCurrent[id]
		source, ok := e.nodes[id].(recoveryBlockHasher)
		if !ok || point.Hash == "" || !e.fresh(id) {
			continue
		}
		hash, err := source.BlockHash(ctx, point.Height)
		if err != nil || hash == "" {
			delete(e.archiveCurrent, id)
			continue
		}
		if hash != point.Hash {
			return e.invalidateArchive("The canonical chain changed while archiving settled obligations.")
		}
		if pool, ok := e.nodes[id].(*chain.Failover); ok && pool.Generation() != point.Generation {
			continue
		}
		valid[id] = true
	}
	remaining := archiveBatchSize
	move := func(kind, id string, chains ...chain.ID) error {
		if remaining <= 0 {
			return nil
		}
		for _, chainID := range chains {
			if !valid[chainID] {
				return nil
			}
		}
		if err := e.stageArchive(kind, id); err != nil {
			return err
		}
		if e.s.Capacity != nil {
			if e.s.Capacity.Anchors == nil {
				e.s.Capacity.Anchors = map[chain.ID]ArchiveAnchor{}
			}
			for _, chainID := range chains {
				point := e.archiveCurrent[chainID]
				e.s.Capacity.Anchors[chainID] = ArchiveAnchor{Height: point.Height, Hash: point.Hash}
			}
		}
		remaining--
		return nil
	}
	for _, id := range sortedArchiveIDs(e.s.Sends) {
		send := e.s.Sends[id]
		if remaining == 0 {
			break
		}
		if send != nil && send.Confirmations >= archiveSettlementDepth && e.archiveSends[id] && valid[send.Chain] {
			if err := move("sends", id, send.Chain); err != nil {
				return err
			}
			if err := e.stageArchive("recovery_sends", id); err != nil {
				return err
			}
		}
	}
	settled := func(all map[chain.ID]map[string]chain.Observation, c contract.HTLC) bool {
		obs, ok := observation(all, c)
		return c.TxID != "" && valid[c.Chain] && ok && obs.Tx != nil && obs.Height > 0 && obs.Height <= e.archiveCurrent[c.Chain].Height && obs.Confirmations >= archiveSettlementDepth
	}
	for _, id := range sortedArchiveIDs(e.s.Swaps) {
		swap := e.s.Swaps[id]
		if remaining == 0 {
			break
		}
		if swap == nil || !terminalSwap(swap) {
			continue
		}
		var chains []chain.ID
		if !recoverySwapInactive(swap) {
			if !settled(swaps, swap.Long) || !settled(swaps, swap.Short) {
				continue
			}
			chains = []chain.ID{chain.BTC, chain.Blake}
		}
		if err := move("swaps", id, chains...); err != nil {
			return err
		}
		for _, kind := range []string{"recovery_swaps", "funding_fees"} {
			key := id
			if kind == "funding_fees" {
				key = "swap/" + id
			}
			if err := e.stageArchive(kind, key); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedArchiveIDs(e.s.TowerJobs) {
		job := e.s.TowerJobs[id]
		if remaining == 0 {
			break
		}
		// Expired but never-funded registrations remain hot: old valid funding
		// may appear later and their signed rescue authorization still applies.
		if job != nil && settled(towers, job.Job.Target) {
			if err := move("tower_jobs", id, job.Job.Target.Chain); err != nil {
				return err
			}
			if err := e.stageArchive("recovery_tower_jobs", id); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedArchiveIDs(e.s.Offers) {
		if remaining == 0 {
			break
		}
		event := e.s.Offers[id]
		if e.automationNeedsOffer(id) {
			continue
		}
		offer, err := historicalOffer(event)
		if err != nil || (offer.Status == "open" && offer.Expires > time.Now().Unix()) || offer.Status == "reserved" || e.s.Outbox[event.ID.Hex()] != nil {
			continue
		}
		if err := move("offers", id); err != nil {
			return err
		}
		if err := e.archiveOwnOfferView(id, event); err != nil {
			return err
		}
		for _, kind := range []string{"order_records", "offer_towers", "funding_fees"} {
			key := id
			if kind == "funding_fees" {
				key = "offer/" + id
			}
			if err := e.stageArchive(kind, key); err != nil {
				return err
			}
		}
	}
	if err := e.compactOwnPublicVersions(&remaining); err != nil {
		return err
	}
	for _, id := range sortedArchiveIDs(e.s.TradeReceipts) {
		if remaining == 0 {
			break
		}
		receipt := e.s.TradeReceipts[id]
		if receipt == nil || receipt.Result.State == "pending" || e.tradeConfirming[id] || e.automationNeedsReceipt(id) {
			continue
		}
		if err := move("trade_receipts", id); err != nil {
			return err
		}
		if receipt.Snapshot.Quote.Token != "" {
			if e.s.TradeTokens == nil {
				e.s.TradeTokens = map[string]string{}
			}
			e.s.TradeTokens[receipt.Snapshot.Quote.Token] = id
			if err := e.stageArchive("trade_tokens", receipt.Snapshot.Quote.Token); err != nil {
				return err
			}
		}
	}
	if err := e.compactActivity(ctx, &remaining, valid); err != nil {
		return err
	}
	for _, id := range sortedArchiveIDs(e.s.Seen) {
		if remaining == 0 {
			break
		}
		if err := move("seen", id); err != nil {
			return err
		}
	}
	for _, id := range sortedArchiveIDs(e.s.SeenSemantics) {
		if remaining == 0 {
			break
		}
		if err := move("seen_semantics", id); err != nil {
			return err
		}
	}
	for _, id := range sortedArchiveIDs(e.s.Outbox) {
		if remaining == 0 {
			break
		}
		if delivery := e.s.Outbox[id]; delivery != nil && delivery.IsAck && delivery.Published {
			if err := move("outbox", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// A known contradictory fork reactivates bounded batches of all potentially
// chain-dependent records. Admission/network exit stays held until every such
// record has an active owner; ordinary settlement can run between batches.
func (e *Engine) reactivateArchive() error {
	if e.vault == nil {
		return errors.New("archive vault unavailable")
	}
	remaining := archiveBatchSize
	for _, kind := range []string{"swaps", "sends", "tower_jobs", "recovery_swaps", "recovery_sends", "recovery_tower_jobs", "activities", "activity_receipts", "funding_fees"} {
		if remaining <= 0 {
			break
		}
		page, _, err := e.vault.ArchivePage(kind, "", remaining)
		if err != nil {
			return err
		}
		for _, record := range page {
			if _, err := e.activateArchived(record.Kind, record.ID); err != nil {
				return err
			}
			var obligation string
			switch record.Kind {
			case "swaps":
				if !recoverySwapInactive(e.s.Swaps[record.ID]) {
					obligation = "swap/" + record.ID
				}
			case "sends":
				obligation = "send/" + record.ID
			case "tower_jobs":
				obligation = "tower/" + record.ID
			}
			if obligation != "" {
				if e.s.Capacity.Invalidated == nil {
					e.s.Capacity.Invalidated = map[string]bool{}
				}
				e.s.Capacity.Invalidated[obligation] = true
			}
			remaining--
		}
	}
	left := uint64(0)
	for _, kind := range []string{"swaps", "sends", "tower_jobs", "recovery_swaps", "recovery_sends", "recovery_tower_jobs", "activities", "activity_receipts", "funding_fees"} {
		left += e.s.Capacity.Archived.Kinds[kind]
	}
	if left == 0 {
		e.s.Capacity.Reactivating = false
		e.s.Capacity.Reason = ""
		e.s.Capacity.Anchors = nil
	}
	return nil
}

func (e *Engine) reconcileArchiveHolds(swaps, towers map[chain.ID]map[string]chain.Observation) {
	if e.s.Capacity == nil {
		return
	}
	for id := range e.s.Capacity.Invalidated {
		kind, key, ok := strings.Cut(id, "/")
		if !ok {
			continue
		}
		resolved := false
		switch kind {
		case "swap":
			resolved = e.archiveCurrent[chain.BTC].Hash != "" && e.archiveCurrent[chain.Blake].Hash != "" && e.recoverySwapResolved(e.s.Swaps[key], swaps)
		case "send":
			send := e.s.Sends[key]
			resolved = send != nil && e.archiveCurrent[send.Chain].Hash != "" && e.fresh(send.Chain) && e.archiveSends[key]
		case "tower":
			if job := e.s.TowerJobs[key]; job != nil && e.archiveCurrent[job.Job.Target.Chain].Hash != "" && e.fresh(job.Job.Target.Chain) {
				obs, ok := observation(towers, job.Job.Target)
				resolved = ok && obs.Tx != nil && obs.Confirmations >= e.Config.Network.Confirmations()
			}
		}
		if resolved {
			delete(e.s.Capacity.Invalidated, id)
		}
	}
}
