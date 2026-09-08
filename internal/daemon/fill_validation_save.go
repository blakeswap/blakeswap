package daemon

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// Only preceding active components are cached in memory. Exact input ownership
// uses an independently encrypted disposable index, never a lifetime Go map.
// Neither representation is serialized or exported as wallet authority.
type fillValidationCheckpoint struct {
	fundings     map[string]string // only active signed funding identities, both roles
	parents      map[string]*ParentOrder
	children     map[string]*FillRecord
	cores        map[string]string
	coreTerms    map[string]string
	reservations map[string]CoinReservation
	inputs       *storage.PrivateIndex
	pending      *storage.PrivateIndexTx
	freshInputs  bool
}

func fillCoreValidationHash(s *Swap, fee FeeSelection) string {
	if s == nil {
		return ""
	}
	return protocol.Digest(struct {
		Role        string
		Request     protocol.Request
		Terms       *protocol.Terms
		Long, Short any
		Funding     string
		Sent        bool
		OwnerFeeCap int64
		Refunds     []string
		Jobs        []protocol.Job
		Fee         FeeSelection
	}{s.Role, s.Request, s.Terms, s.Long, s.Short, s.ShortFunding, s.ShortSent, s.OwnerFeeCap, s.SelfRefunds, s.Jobs, fee})
}
func captureFillValidation(s *State, inputs *storage.PrivateIndex) *fillValidationCheckpoint {
	c := &fillValidationCheckpoint{fundings: map[string]string{}, parents: map[string]*ParentOrder{}, children: map[string]*FillRecord{}, cores: map[string]string{}, coreTerms: map[string]string{}, reservations: map[string]CoinReservation{}, inputs: inputs}
	for id, p := range s.ParentOrders {
		if p != nil {
			value := p.clone()
			if p.Offer.Tower != nil {
				tower := *p.Offer.Tower
				tower.Scripts = maps.Clone(tower.Scripts)
				value.Offer.Tower = &tower
			}
			c.parents[id] = &value
		}
	}
	for id, f := range s.FillRecords {
		if f != nil {
			value := *f
			value.Fees = maps.Clone(f.Fees)
			value.Bounties = maps.Clone(f.Bounties)
			value.Inputs = append([]CoinOutpoint(nil), f.Inputs...)
			c.children[id] = &value
		}
	}
	for owner, reservation := range s.CoinReservations {
		reservation.Inputs = append([]CoinOutpoint(nil), reservation.Inputs...)
		c.reservations[owner] = reservation
	}
	for id, core := range s.Swaps {
		if hash := fundingCustodyHash(core); hash != "" {
			c.fundings[id] = hash
		}
		if core != nil && core.Role == "maker" {
			c.cores[id] = fillCoreValidationHash(core, s.FundingFees["swap/"+id])
			c.coreTerms[id] = protocol.Digest([]any{core.Request, core.Terms})
		}
	}
	return c
}

type engineFillReader struct{ e *Engine }

func (r engineFillReader) ReadArchive(kind, id string) (storage.ArchiveRecord, bool, error) {
	record, found, err := r.e.archiveRecord(kind, id)
	if found {
		record.Data = append([]byte(nil), record.Data...)
	}
	return record, found, err
}
func (r engineFillReader) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	if r.e.vault != nil {
		if err := (vaultFillReader{r.e.vault}).VisitArchive(ctx, func(record storage.ArchiveRecord) error {
			key := archiveMoveKey(record.Kind, record.ID)
			if _, ok := r.e.archiveDeletes[key]; ok {
				return nil
			}
			if _, ok := r.e.archivePuts[key]; ok {
				return nil
			}
			return visit(record)
		}); err != nil {
			return err
		}
	}
	for _, record := range r.e.archivePuts {
		if record.Kind == "parent_orders" || record.Kind == "fill_records" || record.Kind == "swaps" || record.Kind == "fill_keys" {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(record); err != nil {
				return err
			}
		}
	}
	return nil
}

// Every write compares changed components with the preceding validated custody.
// Historical totals are the previously verified parent counters, adjusted by
// exact changed child contributions. No archive page or lifetime scan occurs
// after initialization; only touched cold companions are point-read.
func (e *Engine) prepareFillValidation() (*fillValidationCheckpoint, error) {
	reader := engineFillReader{e}
	if e.fillValidation == nil {
		return e.prepareFillInputIndex(reader, nil)
	}
	previous := e.fillValidation
	oldState := State{Version: e.s.Version, Network: e.s.Network, ParentOrders: previous.parents, FillRecords: previous.children}
	var oldReader FillStateReader
	if e.vault != nil {
		oldReader = vaultFillReader{e.vault}
	}
	ids := map[string]bool{}
	for owner, reservation := range previous.reservations {
		if id := fillOwnerID(owner); id != "" && !reflect.DeepEqual(reservation, e.s.CoinReservations[owner]) {
			ids[id] = true
		}
	}
	for owner, reservation := range e.s.CoinReservations {
		if id := fillOwnerID(owner); id != "" && !reflect.DeepEqual(reservation, previous.reservations[owner]) {
			ids[id] = true
		}
	}
	parents := map[string]bool{}
	cores := map[string]bool{}
	for id := range previous.children {
		ids[id] = true
	}
	for id := range e.s.FillRecords {
		ids[id] = true
	}
	for id := range previous.parents {
		parents[id] = true
	}
	for id := range e.s.ParentOrders {
		parents[id] = true
	}
	for id := range previous.cores {
		cores[id] = true
	}
	for id := range previous.fundings {
		cores[id] = true
	}
	for id, core := range e.s.Swaps {
		if core != nil {
			cores[id] = true
		}
	}
	include := func(kind, id string) {
		switch kind {
		case "fill_records":
			ids[id] = true
			cores[id] = true
		case "parent_orders":
			parents[id] = true
		case "swaps":
			cores[id] = true
		case "funding_fees":
			if id = fillOwnerID(id); id != "" {
				cores[id] = true
			}
		}
	}
	for _, key := range e.archiveDeletes {
		include(key.Kind, key.ID)
	}
	for _, record := range e.archivePuts {
		include(record.Kind, record.ID)
	}
	changes := map[string]fillInputChange{}
	inputChanges := map[string]fillInputChange{}

	for id := range ids {
		before, err := fillValue[FillRecord](&oldState, oldReader, "fill_records", id)
		if err != nil {
			return nil, err
		}
		after, err := fillValue[FillRecord](&e.s, reader, "fill_records", id)
		if err != nil {
			return nil, err
		}
		if before != nil && after == nil {
			return nil, errors.New("write would erase retained child allocation")
		}
		if after == nil {
			continue
		}
		if before != nil {
			// Inputs and monetary authorization survive retirement, refund and reorg.
			a, b := *before, *after
			a.Allocation = b.Allocation
			a.ImportedUncertain = b.ImportedUncertain
			a.FundingDisabled = b.FundingDisabled
			if !reflect.DeepEqual(a, b) || before.Allocation.EverCommitted && !after.Allocation.EverCommitted || before.FundingDisabled && !after.FundingDisabled {
				return nil, errors.New("write changed immutable child custody or permanent commitment")
			}
		}
		if !reflect.DeepEqual(before, after) {
			changes[id] = fillInputChange{before, after}
			parents[after.ParentID] = true
			cores[id] = true
		}
		inputChanges[id] = fillInputChange{before, after}
	}
	expected := map[string]fillTotals{}
	for id := range parents {
		before, err := fillValue[ParentOrder](&oldState, oldReader, "parent_orders", id)
		if err != nil {
			return nil, err
		}
		after, err := fillValue[ParentOrder](&e.s, reader, "parent_orders", id)
		if err != nil {
			return nil, err
		}
		if before != nil && after == nil {
			return nil, errors.New("write would erase retained parent custody")
		}
		if after == nil {
			continue
		}
		if err = validateParentOrder(id, after); err != nil {
			return nil, err
		}
		if after.Offer.Network.Normalized() != e.s.Network.Normalized() {
			return nil, errors.New("parent network changed")
		}
		if before != nil {
			if protocol.Digest(before.Offer) != protocol.Digest(after.Offer) || before.Economics != after.Economics || before.FundingPolicy != after.FundingPolicy {
				return nil, errors.New("write changed immutable parent economics or policy")
			}
			for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
				a, b := before.Fees[asset], after.Fees[asset]
				c, d := before.Bounties[asset], after.Bounties[asset]
				if a.Limit != b.Limit || c.Limit != d.Limit || a.Consumed > b.Consumed || c.Consumed > d.Consumed || a.Transferred > b.Transferred || c.Transferred > d.Transferred {
					return nil, errors.New("write replenished permanent parent authorization")
				}
			}
			expected[id] = parentFillTotals(before)
		}
	}
	for _, change := range changes {
		if change.old != nil {
			v := expected[change.old.ParentID]
			if err := v.add(childFillTotals(change.old), -1); err != nil {
				return nil, err
			}
			expected[change.old.ParentID] = v
		}
	}
	for _, change := range changes {
		v := expected[change.next.ParentID]
		if err := v.add(childFillTotals(change.next), 1); err != nil {
			return nil, err
		}
		expected[change.next.ParentID] = v
	}
	for id := range parents {
		p, err := fillValue[ParentOrder](&e.s, reader, "parent_orders", id)
		if err != nil {
			return nil, err
		}
		if p != nil && expected[id] != parentFillTotals(p) {
			return nil, errors.New("write breaks parent/child quantity or monetary conservation")
		}
	}
	for key := range e.s.FillKeys {
		if strings.HasPrefix(key, "funding/") {
			if err := validateFundingIdentity(&e.s, reader, key); err != nil {
				return nil, err
			}
		}
	}
	for _, record := range e.archivePuts {
		if record.Kind == "fill_keys" && strings.HasPrefix(record.ID, "funding/") {
			if err := validateFundingIdentity(&e.s, reader, record.ID); err != nil {
				return nil, err
			}
		}
	}
	for _, key := range e.archiveDeletes {
		if key.Kind == "fill_keys" && strings.HasPrefix(key.ID, "funding/") {
			if err := validateFundingIdentity(&e.s, reader, key.ID); err != nil {
				return nil, err
			}
		}
	}
	for id := range cores {
		core, err := fillValue[Swap](&e.s, reader, "swaps", id)
		if err != nil {
			return nil, err
		}
		if core == nil {
			oldCore, readErr := fillValue[Swap](&oldState, oldReader, "swaps", id)
			if readErr != nil {
				return nil, readErr
			}
			if previous.fundings[id] != "" || previous.cores[id] != "" || oldCore != nil && (oldCore.Role == "maker" || fundingCustodyHash(oldCore) != "") {
				return nil, errors.New("write erased retained child funding custody")
			}
			continue
		}
		if err := validateSwapFundingParents(&e.s, reader, core); err != nil {
			return nil, err
		}
		oldFunding := previous.fundings[id]
		if oldFunding == "" && oldReader != nil {
			oldCore, err := fillValue[Swap](&oldState, oldReader, "swaps", id)
			if err != nil {
				return nil, err
			}
			oldFunding = fundingCustodyHash(oldCore)
		}
		if oldFunding != "" && oldFunding != fundingCustodyHash(core) {
			return nil, errors.New("write changed immutable local funding ancestry")
		}
		if core.Role != "maker" {
			allocation, readErr := fillValue[FillRecord](&e.s, reader, "fill_records", id)
			if readErr != nil {
				return nil, readErr
			}
			if allocation != nil {
				return nil, errors.New("maker allocation changed core role")
			}
			continue
		}
		terms := protocol.Digest([]any{core.Request, core.Terms})
		oldTerms := previous.coreTerms[id]
		if oldTerms == "" && oldReader != nil {
			oldCore, err := fillValue[Swap](&oldState, oldReader, "swaps", id)
			if err != nil {
				return nil, err
			}
			if oldCore != nil {
				oldTerms = protocol.Digest([]any{oldCore.Request, oldCore.Terms})
			}
		}
		if oldTerms != "" && oldTerms != terms {
			return nil, errors.New("write changed accepted child terms")
		}
		fee, err := fillValue[FeeSelection](&e.s, reader, "funding_fees", "swap/"+id)
		if err != nil {
			return nil, err
		}
		hash := ""
		if fee != nil {
			hash = fillCoreValidationHash(core, *fee)
		}
		_, changed := changes[id]
		if changed || hash == "" || previous.cores[id] != hash {
			if _, _, err = validateFillBinding(&e.s, reader, id); err != nil {
				return nil, err
			}
		}
	}
	return e.prepareFillInputIndex(reader, inputChanges)
}
