package transport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
)

const RelayPageSize = 128
const RelayTieLimit = 1024
const RelayPageBytes = 2 << 20

// RelayCursor belongs to one relay and one exact filter. Until is inclusive;
// the oldest timestamp is reread before moving below it. NIP-01 has no ID-range
// pagination and permits relays to return fewer events than requested, so EOSE
// is not a proof that the relay exposed all historical events. Coverage remains
// explicitly unverified, even after a sweep reaches an empty result.
type RelayCursor struct {
	Until      nostr.Timestamp `json:"until"`
	Limit      int             `json:"limit"`
	Sweeps     uint64          `json:"sweeps"`
	Blocked    bool            `json:"blocked"`
	Incomplete string          `json:"incomplete"`
}

type RelayPage struct {
	Events []nostr.Event
	Next   RelayCursor
}

func ReadPageAs(ctx context.Context, url string, identity nostr.SecretKey, filter nostr.Filter, cursor RelayCursor) (RelayPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if cursor.Limit == 0 {
		cursor.Limit = RelayPageSize
	}
	if cursor.Limit < RelayPageSize || cursor.Limit > RelayTieLimit || cursor.Until < 0 {
		return RelayPage{}, errors.New("invalid relay history cursor")
	}
	if cursor.Blocked {
		return RelayPage{Next: cursor}, errors.New(cursor.Incomplete)
	}
	filter.Until = cursor.Until
	filter.Limit, filter.LimitZero = cursor.Limit, false
	page := RelayPage{Next: cursor}
	pageBytes := 0
	// Signature verification and websocket reads happen outside wallet locks.
	// Reject excess server output immediately rather than buffering 10,000 items.
	err := subscription(ctx, url, &identity, []nostr.Filter{filter}, func(event nostr.Event) error {
		pageBytes += len(event.String())
		if pageBytes > RelayPageBytes {
			return errors.New("relay page exceeds bounded history byte budget; coverage is incomplete at this boundary")
		}
		if len(page.Events) >= cursor.Limit {
			return errors.New("relay ignored requested history page limit")
		}
		if len(page.Events) > 0 {
			previous := page.Events[len(page.Events)-1]
			if event.CreatedAt > previous.CreatedAt || (event.CreatedAt == previous.CreatedAt && event.ID.Hex() <= previous.ID.Hex()) {
				return errors.New("relay history order or duplicate IDs violate NIP-01")
			}
		}
		page.Events = append(page.Events, event)
		return nil
	}, nil, func() { page.Events = nil; pageBytes = 0 }, false)
	if err != nil {
		// Partial data can still contain established-obligation messages. Deliver
		// them but keep the previous durable position so retry never skips them.
		page.Next.Incomplete = err.Error()
		return page, err
	}
	page.Next.Incomplete = "Historical coverage is unverified: relays may omit matching events. Live delivery and repeated sweeps remain enabled."
	if len(page.Events) == 0 {
		page.Next.Until, page.Next.Limit = 0, RelayPageSize
		page.Next.Sweeps++
		return page, nil
	}
	oldest := page.Events[len(page.Events)-1].CreatedAt
	if oldest == cursor.Until {
		if len(page.Events) == cursor.Limit {
			if cursor.Limit == RelayTieLimit {
				page.Next.Blocked = true
				page.Next.Incomplete = fmt.Sprintf("Historical synchronization is incomplete at timestamp %d: at least %d tied events; this relay cannot expose the next IDs with standard NIP-01 filters.", oldest, RelayTieLimit)
			} else {
				page.Next.Limit = min(cursor.Limit*2, RelayTieLimit)
			}
			return page, nil
		}
		// The exposed boundary fits in a page. This is observed progress, not an
		// exhaustiveness claim: a server may still be withholding a tied event.
		if oldest <= 1 || (filter.Since > 0 && oldest <= filter.Since) {
			page.Next.Until = 0
			page.Next.Sweeps++
		} else {
			page.Next.Until = oldest - 1
		}
	} else {
		page.Next.Until = oldest
	}
	page.Next.Limit = RelayPageSize
	return page, nil
}
