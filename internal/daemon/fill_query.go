package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func (e *Engine) parseFillQuery(raw json.RawMessage) (FillQuery, error) {
	var q FillQuery
	if err := json.Unmarshal(raw, &q); err != nil {
		return q, err
	}
	if q.ExpectedWallet != e.Config.Name || q.ExpectedNetwork != string(e.Config.Network.Normalized()) {
		return q, errors.New("fill history wallet or network changed; refresh the selected wallet")
	}
	if !protocol.Hex32(q.ParentMaker) || !protocol.Hex32(q.ParentID) || q.Offset < 0 || q.Limit < 0 || q.Limit > 500 || (q.Revision != "" && !protocol.Hex32(q.Revision)) {
		return q, errors.New("invalid fill parent identity or page")
	}
	if q.Revision == "" && q.Offset != 0 {
		return q, errors.New("a fill offset requires its original revision")
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	return q, nil
}

func fillFilter(q FillQuery) string {
	q.Offset, q.Limit, q.Revision = 0, 0, ""
	return "fills/" + protocol.Digest(q)
}

// The signed request supplies identity and exact child economics. Only the
// local maker's retained allocation supplies quantity bins; a taker cannot
// infer remote allocation from a local terminal stage or missing evidence.
func (e *Engine) fillSummary(s *Swap, archived bool) (FillSummary, error) {
	if s == nil || s.ID != s.Request.ID {
		return FillSummary{}, errors.New("fill core identity mismatch")
	}
	offer, err := s.Request.Validate(int64(s.Request.OfferEvent.CreatedAt))
	if err != nil {
		return FillSummary{}, err
	}
	local := e.identity.Public().Hex()
	if offer.Network.Normalized() != e.Config.Network.Normalized() || (s.Role != "maker" && s.Role != "taker") || (s.Role == "maker" && offer.Maker != local) || (s.Role == "taker" && s.Request.Taker != local) {
		return FillSummary{}, errors.New("fill belongs to another wallet or network")
	}
	buy, err := protocol.RoundedBuy(offer.SellAmount, offer.BuyAmount, s.Request.Quantity)
	if err != nil {
		return FillSummary{}, err
	}
	row := FillSummary{ID: s.ID, ParentMaker: offer.Maker, ParentID: offer.ID, ParentRevision: s.Request.Revision, Quantity: s.Request.Quantity, BuyAmount: buy, Stage: s.Stage, Archived: archived, MonitoringRequired: e.archivedObligationHeld("swap/" + s.ID)}
	if s.Role == "maker" {
		child, err := e.retainedFillRecord(s.ID)
		if err != nil {
			return row, err
		}
		if child.ParentMaker != offer.Maker || child.ParentID != offer.ID || child.ParentRevision != s.Request.Revision || child.RequestDigest != protocol.Digest(s.Request) || child.Allocation.Quantity != row.Quantity || child.BuyAmount != row.BuyAmount {
			return row, errors.New("fill allocation does not match the retained child")
		}
		row.Disposition, row.AllocationKnown = child.Allocation.Disposition, true
		if child.Allocation.Disposition != FillRetired {
			row.AllocatedQuantity = child.Allocation.Quantity
		}
		row.MonitoringRequired = row.MonitoringRequired || child.ImportedUncertain
	}
	return row, nil
}

func (e *Engine) fillPage(ctx context.Context, source *storage.PageSnapshot, q FillQuery) (FillPage, error) {
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
	if q.Revision == "" {
		page, rows, err := e.freezeFillRows(ctx, source, q)
		if err != nil {
			return FillPage{}, err
		}
		if e.historyContext == nil && len(e.activitySnapshots) >= 4 {
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
		q.Revision = page.Revision
		e.activitySnapshotSequence++
		e.activitySnapshots[q.Revision] = activitySnapshot{Rows: rows, Page: ActivityPage{Expires: now + 600}, Fills: &page, Filter: fillFilter(q), Sequence: e.activitySnapshotSequence}
	}
	snapshot, ok := e.activitySnapshots[q.Revision]
	if !ok || snapshot.Fills == nil || snapshot.Filter != fillFilter(q) {
		return FillPage{}, errors.New("fill revision expired or parent changed; refresh the first page")
	}
	page := *snapshot.Fills
	if q.Offset > page.Total {
		return FillPage{}, errors.New("fill offset is outside its revision")
	}
	end := q.Offset + min(q.Limit, page.Total-q.Offset)
	if snapshot.Rows == nil {
		page.Records = append([]FillSummary{}, page.Records[q.Offset:end]...)
	} else {
		rows, err := snapshot.Rows.Page(ctx, uint64(q.Offset), uint64(end-q.Offset))
		if err != nil {
			return FillPage{}, err
		}
		page.Records = make([]FillSummary, 0, len(rows))
		for _, raw := range rows {
			var row FillSummary
			err := json.Unmarshal(raw, &row)
			clear(raw)
			if err != nil {
				return FillPage{}, err
			}
			page.Records = append(page.Records, row)
		}
	}
	page.More = end < page.Total
	if page.More {
		page.NextOffset = end
	}
	return page, nil
}

// Read only child cores and their point-addressed allocation companions. The
// source closes each live read transaction before decode, verification or sort;
// a final generation fence rejects a mixed archive view, including an ABA move.
func (e *Engine) freezeFillRows(ctx context.Context, source *storage.PageSnapshot, q FillQuery) (FillPage, *storage.SortedRows, error) {
	page := FillPage{Wallet: e.Config.Name, Network: e.Config.Network, ParentMaker: q.ParentMaker, ParentID: q.ParentID, Revision: transport.RandomID(), Records: []FillSummary{}}
	var sorter *storage.RowSorter
	var err error
	if e.vault != nil {
		sorter, err = storage.NewRowSorter(ctx, e.vault.PrivateDirectory(), func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
		if err != nil {
			return page, nil, err
		}
		defer sorter.Close()
	}
	emit := func(id string, s *Swap, archived bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s == nil || s.ID != id {
			return errors.New("fill core ownership mismatch")
		}
		var offered protocol.Offer
		if err := json.Unmarshal([]byte(s.Request.OfferEvent.Content), &offered); err != nil {
			return err
		}
		if offered.Maker != q.ParentMaker || offered.ID != q.ParentID {
			return nil
		}
		row, err := e.fillSummary(s, archived)
		if err != nil {
			return err
		}
		if sorter == nil {
			page.Records = append(page.Records, row)
			return nil
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		defer clear(raw)
		return sorter.Add([]byte(id), raw)
	}
	for id, s := range e.s.Swaps {
		if err := emit(id, s, false); err != nil {
			return page, nil, err
		}
	}
	if source != nil {
		if err := source.VisitArchiveKind(ctx, "swaps", func(record storage.ArchiveRecord) error {
			if _, exists := e.s.Swaps[record.ID]; exists {
				return errors.New("fill has duplicate active/archive ownership")
			}
			var s Swap
			if err := json.Unmarshal(record.Data, &s); err != nil {
				return err
			}
			return emit(record.ID, &s, true)
		}); err != nil {
			return page, nil, err
		}
		if err := source.Check(ctx); err != nil {
			return page, nil, err
		}
	}
	if sorter == nil {
		sort.Slice(page.Records, func(i, j int) bool { return page.Records[i].ID < page.Records[j].ID })
		page.Total = len(page.Records)
		return page, nil, nil
	}
	rows, err := sorter.Finish()
	if err != nil {
		return page, nil, err
	}
	if rows.Count > math.MaxUint32 {
		_ = rows.Close()
		return page, nil, errors.New("fill count exceeds the API offset representation")
	}
	page.Total = int(rows.Count)
	return page, rows, nil
}
