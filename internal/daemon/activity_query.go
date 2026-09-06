package daemon

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func activityTime(a Activity) int64 {
	if a.CreatedAt > 0 {
		return a.CreatedAt
	}
	if a.BlockTime > 0 {
		return a.BlockTime
	}
	return a.RecordedAt
}
func activityMatches(a Activity, q ActivityQuery) bool {
	stamp := activityTime(a)
	return (q.Kind == "" || a.Kind == q.Kind) && (q.Status == "" || a.Status == q.Status) && (q.Chain == "" || a.Chain == q.Chain) && (q.From == 0 || stamp >= q.From) && (q.To == 0 || stamp <= q.To)
}
func activityFilter(q ActivityQuery) string {
	q.Snapshot = ""
	q.Cursor = 0
	q.Limit = 0
	return protocol.Digest(q)
}
func (e *Engine) activityPage(raw json.RawMessage) (ActivityPage, error) {
	var q ActivityQuery
	if err := json.Unmarshal(raw, &q); err != nil {
		return ActivityPage{}, err
	}
	if q.ExpectedWallet != e.Config.Name || q.ExpectedNetwork != string(e.Config.Network.Normalized()) {
		return ActivityPage{}, errors.New("activity wallet or network changed; refresh the selected wallet")
	}
	if (q.Chain != "" && !q.Chain.Valid()) || len(q.Kind) > 64 || len(q.Status) > 100 || q.From < 0 || q.To < 0 || (q.To > 0 && q.From > q.To) || q.Limit > 500 {
		return ActivityPage{}, errors.New("invalid activity filter or page size")
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	now := time.Now().Unix()
	if e.activitySnapshots == nil {
		e.activitySnapshots = map[string]activitySnapshot{}
	}
	for id, s := range e.activitySnapshots {
		if s.Page.Expires <= now {
			delete(e.activitySnapshots, id)
		}
	}
	if q.Snapshot == "" {
		if q.Cursor != 0 {
			return ActivityPage{}, errors.New("an activity cursor requires its original snapshot")
		}
		if len(e.activitySnapshots) >= 32 {
			oldest := ""
			for id, s := range e.activitySnapshots {
				if oldest == "" || s.Page.Expires < e.activitySnapshots[oldest].Page.Expires {
					oldest = id
				}
			}
			delete(e.activitySnapshots, oldest)
		}
		page := ActivityPage{Snapshot: transport.RandomID(), Expires: now + 600, Revision: e.s.ActivityRevision, Records: []Activity{}, Index: e.s.ActivityIndexes, Error: e.s.ActivityError}
		for _, a := range e.s.Activities {
			if activityMatches(a, q) {
				page.Records = append(page.Records, a)
			}
		}
		sort.Slice(page.Records, func(i, j int) bool {
			a, b := page.Records[i], page.Records[j]
			if activityTime(a) != activityTime(b) {
				return activityTime(a) > activityTime(b)
			}
			return a.ID > b.ID
		})
		page.Total = uint32(len(page.Records))
		// Freeze every nested slice/map; ongoing confirmations and new deposits may
		// update live records without changing pagination or an in-progress export.
		encoded, err := json.Marshal(page)
		if err != nil {
			return ActivityPage{}, err
		}
		if err = json.Unmarshal(encoded, &page); err != nil {
			return ActivityPage{}, err
		}
		q.Snapshot = page.Snapshot
		e.activitySnapshots[q.Snapshot] = activitySnapshot{Page: page, Filter: activityFilter(q)}
	}
	snapshot, ok := e.activitySnapshots[q.Snapshot]
	if !ok {
		return ActivityPage{}, errors.New("activity snapshot expired or was replaced; refresh the first page")
	}
	if snapshot.Filter != activityFilter(q) {
		return ActivityPage{}, errors.New("activity filters changed; start a fresh snapshot")
	}
	if q.Cursor > snapshot.Page.Total {
		return ActivityPage{}, errors.New("activity cursor is outside its snapshot")
	}
	end := min(q.Cursor+q.Limit, snapshot.Page.Total)
	page := snapshot.Page
	page.Records = page.Records[q.Cursor:end]
	page.NextCursor = 0
	if end < page.Total {
		page.NextCursor = end
	}
	return page, nil
}

func csvText(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if len(value) > 0 && (value[0] == '\t' || value[0] == '\r' || value[0] == '\n') {
		return "'" + value
	}
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
func csvTime(value int64) string {
	if value == 0 {
		return ""
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}
func activityCSV(records []Activity, header bool) (string, error) {
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	if header {
		if err := writer.Write([]string{"id", "group_id", "wallet", "network", "kind", "asset", "direction", "movement", "amount_sats", "principal_sats", "fee_sats", "fee_known", "fee_payer", "bounty_sats", "txid", "variants", "outpoints", "order_id", "swap_id", "send_id", "address", "status", "confirmations", "created_at_utc", "created_time_source", "first_recorded_at_utc", "block_time_utc", "observed_at_utc", "source", "generation", "label", "prior_outcomes"}); err != nil {
			return "", err
		}
	}
	for _, a := range records {
		points := make([]string, 0, len(a.Outpoints))
		for _, p := range a.Outpoints {
			points = append(points, pointKey(p))
		}
		history, _ := json.Marshal(a.History)
		fee := ""
		if a.FeeKnown {
			fee = strconv.FormatInt(a.Fee, 10)
		}
		row := []string{a.ID, a.GroupID, a.Wallet, string(a.Network), a.Kind, string(a.Chain), a.Direction, strconv.FormatBool(a.Movement), strconv.FormatInt(a.Amount, 10), strconv.FormatInt(a.Principal, 10), fee, strconv.FormatBool(a.FeeKnown), a.FeePayer, strconv.FormatInt(a.Bounty, 10), a.TxID, strings.Join(a.Variants, "|"), strings.Join(points, "|"), a.OrderID, a.SwapID, a.SendID, a.Address, a.Status, strconv.Itoa(a.Confirmations), csvTime(a.CreatedAt), a.CreatedSource, csvTime(a.RecordedAt), csvTime(a.BlockTime), csvTime(a.ObservedAt), a.Source, strconv.FormatUint(a.Generation, 10), a.Label, string(history)}
		for i := range row {
			// Numeric cells are formatted from typed integers, never user text.
			// Preserve negative values exactly if a future activity uses them.
			switch i {
			case 8, 9, 10, 13, 22, 29:
				continue
			}
			row[i] = csvText(row[i])
		}
		if err := writer.Write(row); err != nil {
			return "", err
		}
	}
	writer.Flush()
	return output.String(), writer.Error()
}
func (e *Engine) exportActivity(raw json.RawMessage) (ActivityExport, error) {
	var q ActivityQuery
	if err := json.Unmarshal(raw, &q); err != nil {
		return ActivityExport{}, err
	}
	page, err := e.activityPage(raw)
	if err != nil {
		return ActivityExport{}, err
	}
	csv, err := activityCSV(page.Records, q.Cursor == 0)
	return ActivityExport{Snapshot: page.Snapshot, Expires: page.Expires, NextCursor: page.NextCursor, Total: page.Total, CSV: csv}, err
}
