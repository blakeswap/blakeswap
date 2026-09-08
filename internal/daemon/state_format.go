package daemon

import (
	"encoding/json"
	"errors"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// StateVersion is independent of portable encryption framing. Format 3 is the
// hard cutover to child quantities; earlier development vaults are preserved
// and rejected, never relabelled or silently reset.
const StateVersion = 3

func ValidateStateVersion(s *State) error {
	if s == nil || s.Version != StateVersion {
		return errors.New("incompatible development wallet state: protocol 2 requires state format 3; keep this source and create a separate profile")
	}
	if s.Network == "" || !s.Network.Valid() {
		return errors.New("state format 3 requires an explicit supported network")
	}
	return nil
}

// ValidateProtocolState rejects incompatible owned protocol records even when
// an outer state marker was changed independently. Detailed signing, quantity
// ownership and recovery validators remain separate from this format check.
// Foreign advisory caches do not confer authority and are filtered on ingestion.
func ValidateProtocolState(s *State) error {
	if err := ValidateStateVersion(s); err != nil {
		return err
	}
	checkOffer := func(o protocol.Offer) error {
		if o.Version != protocol.Version || o.Revision == 0 || o.Network.Normalized() != s.Network.Normalized() {
			return errors.New("incompatible owned offer format")
		}
		_, _, err := o.FillPolicy.Interval(o.SellAmount, o.BuyAmount)
		return err
	}
	checkEvents := func(events map[string]nostr.Event) error {
		for _, event := range events {
			var o protocol.Offer
			if err := json.Unmarshal([]byte(event.Content), &o); err != nil {
				return err
			}
			if err := checkOffer(o); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkEvents(s.Offers); err != nil {
		return err
	}
	if s.Recovery != nil {
		if err := checkEvents(s.Recovery.Offers); err != nil {
			return err
		}
	}
	for _, record := range s.OrderRecords {
		if err := checkOffer(record.Offer); err != nil {
			return err
		}
	}
	for _, swap := range s.Swaps {
		if swap == nil || swap.Request.Version != protocol.Version || swap.Request.Revision == 0 || swap.Request.Quantity < protocol.MinPrincipal {
			return errors.New("incompatible saved child request")
		}
		var offered protocol.Offer
		if err := json.Unmarshal([]byte(swap.Request.OfferEvent.Content), &offered); err != nil {
			return err
		}
		if err := checkOffer(offered); err != nil {
			return err
		}
		if swap.Terms != nil && (swap.Terms.Version != protocol.Version || swap.Terms.Request.Version != protocol.Version || swap.Terms.Request.Quantity != swap.Request.Quantity || swap.Terms.Request.Revision != swap.Request.Revision) {
			return errors.New("incompatible saved child terms")
		}
		for _, job := range swap.Jobs {
			if job.Version != protocol.Version {
				return errors.New("incompatible saved rescue job")
			}
		}
		for _, receipt := range swap.Receipts {
			if err := receipt.Validate(); err != nil {
				return err
			}
		}
	}
	for _, job := range s.TowerJobs {
		if job == nil || job.Job.Version != protocol.Version {
			return errors.New("incompatible saved watchtower job")
		}
	}
	return nil
}

// PreflightStateVersion does not initialize or modify an incompatible source.
// The activation path repeats validation after obtaining its exclusive writer.
func PreflightStateVersion(path string, password []byte) error {
	v, err := storage.OpenReadOnly(path, password)
	if err != nil {
		return err
	}
	defer v.Close()
	var s State
	if _, err := v.Load(&s); err != nil {
		return err
	}
	return ValidateProtocolState(&s)
}
