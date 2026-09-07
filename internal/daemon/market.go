package daemon

import (
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"time"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// OrderRecord retains management evidence alongside T08's linked activity.
// Publication means acknowledged relay storage, never global availability.
type OrderRecord struct {
	Offer            protocol.Offer `json:"offer"`
	EventID          string         `json:"event_id"`
	CreatedAt        int64          `json:"created_at"`
	Publication      string         `json:"publication"`
	AcknowledgedAt   int64          `json:"acknowledged_at"`
	CancelledEventID string         `json:"cancelled_event_id,omitempty"`
	Replaces         string         `json:"replaces,omitempty"`
	ReplacedBy       string         `json:"replaced_by,omitempty"`
	RecreatedFrom    string         `json:"recreated_from,omitempty"`
}

func historicalOffer(event nostr.Event) (protocol.Offer, error) {
	var o protocol.Offer
	if err := json.Unmarshal([]byte(event.Content), &o); err != nil || o.Expires <= 0 {
		return o, errors.New("invalid historical offer")
	}
	// Verify signature, namespace and immutable bounds independently of current
	// availability. A cancellation may itself have been signed after expiry.
	return protocol.DecodeOffer(event, o.Expires-1)
}

func (e *Engine) syncOrderRecords() {
	if e.s.OrderRecords == nil {
		e.s.OrderRecords = map[string]OrderRecord{}
	}
	for id, event := range e.s.Offers {
		o, err := historicalOffer(event)
		if err != nil || o.ID != id || o.Maker != e.identity.Public().Hex() || o.Network.Normalized() != e.Config.Network {
			continue
		}
		r, exists := e.s.OrderRecords[id]
		if exists && r.EventID != event.ID.Hex() {
			r.Publication, r.AcknowledgedAt = "unknown", 0
		}
		if !exists {
			r.Publication = "unknown"
			r.CreatedAt = e.s.Activities[activityID("order", id)].CreatedAt
		}
		r.Offer, r.EventID = o, event.ID.Hex()
		e.s.OrderRecords[id] = r
	}
}

type MarketQuery struct {
	ExpectedWallet  string `json:"expected_wallet"`
	ExpectedNetwork string `json:"expected_network"`
	Owner           string `json:"owner"`
	Side            string `json:"side"` // This wallet buys/sells BTC, independent of maker ownership.
	Status          string `json:"status"`
	BTCMin          int64  `json:"btc_min"`
	BTCMax          int64  `json:"btc_max"`
	Sort            string `json:"sort"`
	Descending      bool   `json:"descending"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
	Revision        string `json:"revision"`
}
type MarketOrder struct {
	Offer          protocol.Offer `json:"offer"`
	EventID        string         `json:"event_id"`
	Own            bool           `json:"own"`
	Side           string         `json:"side"`
	BTCAmount      int64          `json:"btc_amount"`
	BlakeAmount    int64          `json:"blake_amount"`
	Rate           string         `json:"rate"`
	Status         string         `json:"status"`
	Availability   string         `json:"availability"`
	Publication    string         `json:"publication"`
	AcknowledgedAt int64          `json:"acknowledged_at"`
	CreatedAt      int64          `json:"created_at"`
	Replaces       string         `json:"replaces"`
	ReplacedBy     string         `json:"replaced_by"`
	RecreatedFrom  string         `json:"recreated_from"`
	SwapIDs        []string       `json:"swap_ids"`
	ActivityID     string         `json:"activity_id"`
	CanTake        bool           `json:"can_take"`
	CanCancel      bool           `json:"can_cancel"`
	CanReplace     bool           `json:"can_replace"`
	CanRecreate    bool           `json:"can_recreate"`
}
type MarketPage struct {
	Wallet     string        `json:"wallet"`
	Network    chain.Network `json:"network"`
	Records    []MarketOrder `json:"records"`
	Revision   string        `json:"revision"`
	Total      int           `json:"total"`
	NextOffset int           `json:"next_offset"`
	More       bool          `json:"more"`
	ObservedAt int64         `json:"observed_at"`
	AllRelays  bool          `json:"all_relays"`
}

func (e *Engine) marketOrder(o protocol.Offer, eventID string, record OrderRecord, now int64) MarketOrder {
	row := MarketOrder{Offer: o, EventID: eventID, Own: o.Maker == e.identity.Public().Hex(), Status: o.Status, SwapIDs: []string{}}
	row.BTCAmount, row.BlakeAmount = o.SellAmount, o.BuyAmount
	if o.Sell == chain.Blake {
		row.BTCAmount, row.BlakeAmount = o.BuyAmount, o.SellAmount
	}
	row.Rate = new(big.Rat).SetFrac64(row.BlakeAmount, row.BTCAmount).FloatString(8)
	row.Side = "buy_btc"
	if (o.Sell == chain.BTC) == row.Own {
		row.Side = "sell_btc"
	}
	if row.Status == "open" && o.Expires <= now {
		row.Status = "expired"
	}
	active := false
	for id, s := range e.s.Swaps {
		var requested protocol.Offer
		if json.Unmarshal([]byte(s.Request.OfferEvent.Content), &requested) != nil || requested.Maker != o.Maker || requested.ID != o.ID {
			continue
		}
		row.SwapIDs = append(row.SwapIDs, id)
		if !terminalSwap(s) {
			active = true
			if !row.Own && row.Status == "open" {
				row.Status = "pending"
			}
		}
	}
	sort.Strings(row.SwapIDs)
	if row.Own && row.Status == "reserved" && !active {
		if finished := e.finishedOrder(o.ID); finished != "" {
			row.Status = finished
		}
	}
	row.Availability = row.Status
	if row.Own {
		row.Offer = e.ownOffer(o)
		row.Publication, row.AcknowledgedAt, row.CreatedAt = record.Publication, record.AcknowledgedAt, record.CreatedAt
		row.Replaces, row.ReplacedBy, row.RecreatedFrom = record.Replaces, record.ReplacedBy, record.RecreatedFrom
		row.ActivityID = activityID("order", o.ID)
		row.CanCancel = o.Status == "open" && !active && e.s.Offers[o.ID].ID.Hex() == eventID
		row.CanReplace = row.CanCancel && row.Status == "open"
		row.CanRecreate = !active && (row.Status == "expired" || row.Status == "cancelled" || row.Status == "filled" || row.Status == "refunded")
		if row.Status == "open" && record.Publication != "relay_acknowledged" {
			row.Availability = "publication_pending"
		}
	} else {
		row.Offer.Tower, row.Offer.TowerBPS = nil, 0
		fresh := e.marketObservedAt > 0 && now-e.marketObservedAt <= 120
		row.CanTake = row.Status == "open" && fresh
		if row.Status == "open" && !fresh {
			row.Availability = "stale"
		}
	}
	return row
}

func marketLess(a, b MarketOrder, key string, descending bool) bool {
	comparison := 0
	switch key {
	case "rate":
		left := new(big.Int).Mul(big.NewInt(a.BlakeAmount), big.NewInt(b.BTCAmount))
		right := new(big.Int).Mul(big.NewInt(b.BlakeAmount), big.NewInt(a.BTCAmount))
		comparison = left.Cmp(right)
	case "size":
		comparison = big.NewInt(a.BTCAmount).Cmp(big.NewInt(b.BTCAmount))
	case "expiry":
		comparison = big.NewInt(a.Offer.Expires).Cmp(big.NewInt(b.Offer.Expires))
	}
	if comparison == 0 {
		return a.Offer.Maker+":"+a.Offer.ID < b.Offer.Maker+":"+b.Offer.ID
	}
	return (comparison < 0) != descending
}

func (e *Engine) marketPage(raw json.RawMessage) (MarketPage, error) {
	var q MarketQuery
	if err := json.Unmarshal(raw, &q); err != nil {
		return MarketPage{}, err
	}
	if err := e.tradeBinding(q.ExpectedWallet, q.ExpectedNetwork); err != nil {
		return MarketPage{}, err
	}
	if q.Owner == "" {
		q.Owner = "all"
	}
	if q.Side == "" {
		q.Side = "all"
	}
	if q.Status == "" {
		q.Status = "open"
	}
	if q.Sort == "" {
		q.Sort = "rate"
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if (q.Owner != "all" && q.Owner != "mine" && q.Owner != "others") || (q.Side != "all" && q.Side != "buy_btc" && q.Side != "sell_btc") || (q.Sort != "rate" && q.Sort != "size" && q.Sort != "expiry") || q.BTCMin < 0 || q.BTCMax < 0 || q.BTCMin > 10000000000 || q.BTCMax > 10000000000 || (q.BTCMax > 0 && q.BTCMax < q.BTCMin) || q.Offset < 0 || q.Limit < 1 || q.Limit > 500 {
		return MarketPage{}, errors.New("invalid market filters or page")
	}
	switch q.Status {
	case "all", "open", "pending", "reserved", "filled", "cancelled", "expired", "refunded":
	default:
		return MarketPage{}, errors.New("invalid order status filter")
	}
	page := MarketPage{Wallet: e.Config.Name, Network: e.Config.Network, Records: []MarketOrder{}, ObservedAt: e.marketObservedAt, AllRelays: e.marketAllRelays}
	now := time.Now().Unix()
	rows := map[string]MarketOrder{}
	for id, r := range e.s.OrderRecords {
		if r.Offer.Expires > 0 && r.Offer.Validate(r.Offer.Expires-1) == nil && r.Offer.ID == id && r.Offer.Maker == e.identity.Public().Hex() && r.Offer.Network.Normalized() == e.Config.Network {
			rows[r.Offer.Maker+":"+id] = e.marketOrder(r.Offer, r.EventID, r, now)
		}
	}
	for _, event := range e.s.Book {
		o, err := protocol.DecodeOffer(event, now)
		if err != nil || o.Maker == e.identity.Public().Hex() || o.Network.Normalized() != e.Config.Network {
			continue
		}
		rows[o.Maker+":"+o.ID] = e.marketOrder(o, event.ID.Hex(), OrderRecord{}, now)
	}
	for _, row := range rows {
		if (q.Owner == "mine" && !row.Own) || (q.Owner == "others" && row.Own) || (q.Side != "all" && row.Side != q.Side) || (q.Status != "all" && row.Status != q.Status) || row.BTCAmount < q.BTCMin || (q.BTCMax > 0 && row.BTCAmount > q.BTCMax) {
			continue
		}
		page.Records = append(page.Records, row)
	}
	sort.Slice(page.Records, func(i, j int) bool { return marketLess(page.Records[i], page.Records[j], q.Sort, q.Descending) })
	page.Revision = protocol.Digest(page.Records)
	if q.Revision != "" && q.Revision != page.Revision {
		return MarketPage{}, errors.New("market changed while paging; refresh the filtered results")
	}
	page.Total = len(page.Records)
	if q.Offset > page.Total {
		return MarketPage{}, errors.New("market page offset is unavailable")
	}
	end := min(q.Offset+q.Limit, page.Total)
	page.Records = page.Records[q.Offset:end]
	page.NextOffset, page.More = end, end < page.Total
	return page, nil
}
