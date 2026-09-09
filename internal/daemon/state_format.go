package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// StateVersion stays at 1 throughout first-release development. Validate the
// current schema as well as its marker; never relabel or reset incompatible data.
const StateVersion = 1

func ValidateStateVersion(s *State) error {
	if s == nil || s.Version != StateVersion {
		return errors.New("incompatible development wallet state: v1 requires the current wallet state schema; keep this source and create a separate profile")
	}
	if s.Network == "" || !s.Network.Valid() {
		return errors.New("state format 1 requires an explicit supported network")
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
	for id, delivery := range s.Outbox {
		if err := validateDeliveryFormat(s.Network, id, delivery); err != nil {
			return err
		}
	}
	if s.Recovery != nil {
		for id, delivery := range s.Recovery.Outbox {
			if err := validateDeliveryFormat(s.Network, id, delivery); err != nil {
				return err
			}
		}
	}
	for key, owner := range s.FillKeys {
		if err := validateFillIdentityRecord(key, owner); err != nil {
			return err
		}
	}
	for id, parent := range s.ParentOrders {
		if err := validateParentOrder(id, parent); err != nil {
			return err
		}
	}
	for id, child := range s.FillRecords {
		if err := validateFillRecord(id, child); err != nil {
			return err
		}
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
		if _, err := swapIdentityKeys(swap); err != nil {
			return err
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
	return ValidateVaultProtocolState(v, &s)
}

// ValidateVaultProtocolState checks retained authority without reconstructing
// unrelated lifetime history. The caller owns a read-only or exclusive vault,
// so all bounded pages belong to the same committed file. Activation repeats
// this check under writer ownership after a read-only preflight releases it.
func ValidateVaultProtocolState(v *storage.Vault, s *State) error {
	return ValidateVaultProtocolStateContext(context.Background(), v, s)
}

// ValidateVaultProtocolStateContext keeps cancellation attached to completed
// private import/snapshot validation, including the final external-sort pass.
func ValidateVaultProtocolStateContext(ctx context.Context, v *storage.Vault, s *State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateProtocolState(s); err != nil {
		return err
	}
	stats, err := v.ArchiveStats()
	if err != nil {
		return err
	}
	if err := s.ValidateArchiveCheckpoint(stats); err != nil {
		return err
	}
	for _, swap := range s.Swaps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateVaultSwapIdentity(v, s, swap); err != nil {
			return err
		}
	}
	for kind, fields := range archiveFields {
		// Derive the category from the registry's actual State field path;
		// Recovery.Offers is stored as quarantined_offers, not recovery_offers.
		owned := len(fields) == 1 && (fields[0] == "FillKeys" || fields[0] == "Outbox" || fields[0] == "ParentOrders" || fields[0] == "FillRecords" || fields[0] == "Offers" || fields[0] == "OrderRecords" || fields[0] == "Swaps" || fields[0] == "TowerJobs")
		quarantined := len(fields) == 2 && fields[0] == "Recovery" && (fields[1] == "Offers" || fields[1] == "Outbox")
		if !owned && !quarantined {
			continue
		}
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			records, next, err := v.ArchivePage(kind, cursor, 32)
			if err != nil {
				return err
			}
			for _, record := range records {
				if record.Kind == "swaps" {
					var swap Swap
					if err := json.Unmarshal(record.Data, &swap); err != nil {
						return err
					}
					if err := validateVaultSwapIdentity(v, s, &swap); err != nil {
						return err
					}
				}
				if _, err := ValidateArchiveRecordAgainstState(*s, record); err != nil {
					return err
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return ValidateFillConservation(ctx, s, vaultFillReader{v}, v.PrivateDirectory())
}

// openCurrentStateVault returns an exclusively owned current-format checkpoint.
// Neither acquiring/authenticating the writer nor a rejected validation writes.
func openCurrentStateVault(path string, password []byte) (*storage.Vault, State, error) {
	v, err := storage.OpenExisting(path, password)
	if err != nil {
		return nil, State{}, err
	}
	var s State
	if _, err = v.Load(&s); err == nil {
		err = ValidateVaultProtocolState(v, &s)
	}
	if err != nil {
		_ = v.Close()
		return nil, State{}, err
	}
	return v, s, nil
}
