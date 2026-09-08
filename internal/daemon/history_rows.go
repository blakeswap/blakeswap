package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func (e *Engine) historyRow(a Activity) Activity {
	a.Wallet = e.Config.Name
	a.ArchiveCoverage = ""
	a.ArchiveVerified = false
	if a.Generation > 0 && !e.activitySourceCurrent(a.Chain, a.Generation) {
		a.History = append(append([]ActivityOutcome{}, a.History...), activityOutcome(a))
		a.Status = "unknown"
		a.Confirmations = 0
	}
	return a
}
func (e *Engine) freezeActivityRows(ctx context.Context, source *storage.PageSnapshot, q ActivityQuery) (ActivityQuery, error) {
	if q.Cursor != 0 {
		return q, errors.New("an activity cursor requires its original snapshot")
	}
	sorter, err := storage.NewRowSorter(ctx, e.vault.PrivateDirectory(), func(a, b []byte) bool { return bytes.Compare(a, b) > 0 })
	if err != nil {
		return q, err
	}
	defer sorter.Close()
	emit := func(a Activity) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a = e.historyRow(a)
		if !activityMatches(a, q) {
			return nil
		}
		key := binary.BigEndian.AppendUint64(nil, uint64(activityTime(a))^(uint64(1)<<63))
		key = append(key, []byte(a.ID)...)
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		defer clear(raw)
		return sorter.Add(key, raw)
	}
	for _, a := range e.s.Activities {
		if err := emit(a); err != nil {
			return q, err
		}
	}
	if source != nil {
		err = source.VisitArchiveKind(ctx, "activities", func(record storage.ArchiveRecord) error {
			if _, overlap := e.s.Activities[record.ID]; overlap {
				return errors.New("activity has duplicate active/archive ownership")
			}
			var row Activity
			if err := json.Unmarshal(record.Data, &row); err != nil {
				return err
			}
			if row.ID != record.ID || row.Network.Normalized() != e.Config.Network.Normalized() {
				return errors.New("archived activity identity or network mismatch")
			}
			return emit(row)
		})
		if err != nil {
			return q, err
		}
		if err = source.Check(ctx); err != nil {
			return q, err
		}
	}
	rows, err := sorter.Finish()
	if err != nil {
		return q, err
	}
	if rows.Count > math.MaxUint32 {
		_ = rows.Close()
		return q, errors.New("activity count exceeds the API cursor representation")
	}
	now := time.Now().Unix()
	if e.activitySnapshots == nil {
		e.activitySnapshots = map[string]activitySnapshot{}
	}
	for id, snapshot := range e.activitySnapshots {
		if snapshot.Page.Expires <= now {
			if snapshot.Rows != nil {
				_ = snapshot.Rows.Close()
			}
			delete(e.activitySnapshots, id)
		}
	}
	if len(e.activitySnapshots) >= 4 {
		oldest := ""
		for id, snapshot := range e.activitySnapshots {
			if oldest == "" || snapshot.Sequence < e.activitySnapshots[oldest].Sequence {
				oldest = id
			}
		}
		if old := e.activitySnapshots[oldest].Rows; old != nil {
			_ = old.Close()
		}
		delete(e.activitySnapshots, oldest)
	}
	page := ActivityPage{Snapshot: transport.RandomID(), Expires: now + 600, Revision: e.s.ActivityRevision, Total: uint32(rows.Count), Records: []Activity{}, Index: e.s.ActivityIndexes, Error: e.s.ActivityError}
	e.activitySnapshotSequence++
	e.activitySnapshots[page.Snapshot] = activitySnapshot{Page: page, Rows: rows, Filter: activityFilter(q), Sequence: e.activitySnapshotSequence}
	q.Snapshot = page.Snapshot
	return q, nil
}

func marketMatches(row MarketOrder, q MarketQuery) bool {
	return !((q.Owner == "mine" && !row.Own) || (q.Owner == "others" && row.Own) || (q.Side != "all" && row.Side != q.Side) || (q.Status != "all" && row.Status != q.Status) || row.BTCAmount < q.BTCMin || (q.BTCMax > 0 && row.BTCAmount > q.BTCMax))
}
func (e *Engine) streamMarketPage(ctx context.Context, source *storage.PageSnapshot, q MarketQuery) (MarketPage, error) {
	sorter, err := storage.NewRowSorter(ctx, e.vault.PrivateDirectory(), func(a, b []byte) bool {
		var left, right MarketOrder
		// These small keys were encoded below and authenticated by the sorter.
		_ = json.Unmarshal(a, &left)
		_ = json.Unmarshal(b, &right)
		return marketLess(left, right, q.Sort, q.Descending)
	})
	if err != nil {
		return MarketPage{}, err
	}
	defer sorter.Close()
	now := time.Now().Unix()
	emit := func(row MarketOrder) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !marketMatches(row, q) {
			return nil
		}
		key, err := json.Marshal(MarketOrder{Offer: protocol.Offer{ID: row.Offer.ID, Maker: row.Offer.Maker, Expires: row.Offer.Expires}, BTCAmount: row.BTCAmount, BlakeAmount: row.BlakeAmount})
		if err != nil {
			return err
		}
		defer clear(key)
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		defer clear(raw)
		return sorter.Add(key, raw)
	}
	own := func(id string, r OrderRecord) error {
		if r.Offer.Expires <= 0 || r.Offer.Validate(r.Offer.Expires-1) != nil || r.Offer.ID != id || r.Offer.Maker != e.identity.Public().Hex() || r.Offer.Network.Normalized() != e.Config.Network {
			return nil
		}
		if err := validateOrderSettlement(id, r, e.Config.Network); err != nil {
			return err
		}
		// Protection remains an exact cold companion for orders without a funded
		// swap. Read it on demand; this never reactivates authority or a publisher.
		if _, hot := e.s.OfferTowers[id]; !hot {
			var tower protocol.Tower
			found, err := e.archivedValue("offer_towers", id, &tower)
			if err != nil {
				return err
			}
			if found {
				r.Protection = &tower
			}
		}
		// Refuse a metadata read failure, rather than presenting recreation as a
		// definite unavailable action because an authenticated source was unreadable.
		if _, err := e.finishedOrderChecked(id); err != nil {
			return err
		}
		return emit(e.marketOrder(r.Offer, r.EventID, r, now))
	}
	if q.Owner != "others" {
		for id, r := range e.s.OrderRecords {
			if err := own(id, r); err != nil {
				return MarketPage{}, err
			}
		}
		if source != nil {
			if err := source.VisitArchiveKind(ctx, "order_records", func(record storage.ArchiveRecord) error {
				if _, overlap := e.s.OrderRecords[record.ID]; overlap {
					return errors.New("order has duplicate active/archive ownership")
				}
				var row OrderRecord
				if err := json.Unmarshal(record.Data, &row); err != nil {
					return err
				}
				return own(record.ID, row)
			}); err != nil {
				return MarketPage{}, err
			}
		}
	}
	if q.Owner != "mine" {
		// The live public book is independently bounded and keyed by maker/order.
		rows := map[string]MarketOrder{}
		for _, event := range e.s.Book {
			offer, err := protocol.DecodeOffer(event, now)
			if err != nil || offer.Maker == e.identity.Public().Hex() || offer.Network.Normalized() != e.Config.Network {
				continue
			}
			rows[offer.Maker+":"+offer.ID] = e.marketOrder(offer, event.ID.Hex(), OrderRecord{}, now)
		}
		for _, row := range rows {
			if err := emit(row); err != nil {
				return MarketPage{}, err
			}
		}
	}
	if source != nil {
		if err := source.Check(ctx); err != nil {
			return MarketPage{}, err
		}
	}
	rows, err := sorter.Finish()
	if err != nil {
		return MarketPage{}, err
	}
	defer rows.Close()
	if rows.Count > math.MaxInt {
		return MarketPage{}, errors.New("market result exceeds the API count representation")
	}
	if q.Revision != "" && q.Revision != rows.Digest {
		return MarketPage{}, errors.New("market changed while paging; refresh the filtered results")
	}
	if uint64(q.Offset) > rows.Count {
		return MarketPage{}, errors.New("market page offset is unavailable")
	}
	raw, err := rows.Page(ctx, uint64(q.Offset), uint64(q.Limit))
	if err != nil {
		return MarketPage{}, err
	}
	page := MarketPage{Wallet: e.Config.Name, Network: e.Config.Network, Records: []MarketOrder{}, Revision: rows.Digest, Total: int(rows.Count), ObservedAt: e.marketObservedAt, AllRelays: e.marketAllRelays}
	for _, data := range raw {
		var row MarketOrder
		err := json.Unmarshal(data, &row)
		clear(data)
		if err != nil {
			return MarketPage{}, err
		}
		page.Records = append(page.Records, row)
	}
	page.NextOffset = q.Offset + len(page.Records)
	page.More = page.NextOffset < page.Total
	return page, nil
}
