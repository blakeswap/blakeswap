package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"strings"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

// FillStateReader belongs to one authenticated, immutable/exclusively owned
// checkpoint. Rows may occur in any order and companions need not be adjacent.
// Returned point-read data is owned by the caller; visitor data is borrowed.
type FillStateReader interface {
	ReadArchive(string, string) (storage.ArchiveRecord, bool, error)
	VisitArchive(context.Context, func(storage.ArchiveRecord) error) error
}

type fillTotals struct {
	Bins     [4]int64    // reserved, committed, filled, child-owned released
	Fees     [2][2]int64 // asset, reserved/consumed
	Bounties [2][2]int64
}

func parentFillTotals(p *ParentOrder) fillTotals {
	t := fillTotals{Bins: [4]int64{p.Quantities.Reserved, p.Quantities.Committed, p.Quantities.Filled, p.Quantities.Released - p.Quantities.Withdrawn}}
	for i, id := range []chain.ID{chain.BTC, chain.Blake} {
		t.Fees[i] = [2]int64{p.Fees[id].Reserved, p.Fees[id].Consumed}
		t.Bounties[i] = [2]int64{p.Bounties[id].Reserved, p.Bounties[id].Consumed}
	}
	return t
}
func childFillTotals(f *FillRecord) fillTotals {
	var t fillTotals
	switch f.Allocation.Disposition {
	case FillReserved:
		t.Bins[0] = f.Allocation.Quantity
	case FillCommitted:
		t.Bins[1] = f.Allocation.Quantity
	case FillFilled:
		t.Bins[2] = f.Allocation.Quantity
	case FillReleased:
		t.Bins[3] = f.Allocation.Quantity
	}
	charge := -1
	if f.Allocation.EverCommitted {
		charge = 1
	} else if f.Allocation.Disposition == FillReserved {
		charge = 0
	}
	if charge >= 0 {
		for i, id := range []chain.ID{chain.BTC, chain.Blake} {
			t.Fees[i][charge] = f.Fees[id]
			t.Bounties[i][charge] = f.Bounties[id]
		}
	}
	return t
}
func (t *fillTotals) add(other fillTotals, sign int64) error {
	add := func(a *int64, b int64) error {
		if b < 0 || (sign > 0 && *a > math.MaxInt64-b) || (sign < 0 && *a < b) {
			return errors.New("child conservation overflow or underflow")
		}
		*a += sign * b
		return nil
	}
	for i := range t.Bins {
		if err := add(&t.Bins[i], other.Bins[i]); err != nil {
			return err
		}
	}
	for i := range t.Fees {
		for j := range t.Fees[i] {
			if err := add(&t.Fees[i][j], other.Fees[i][j]); err != nil {
				return err
			}
			if err := add(&t.Bounties[i][j], other.Bounties[i][j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func fillValue[T any](s *State, reader FillStateReader, kind, id string) (*T, error) {
	group, err := archiveMap(s, kind, false)
	if err != nil {
		return nil, err
	}
	if group.IsValid() {
		v := group.MapIndex(reflect.ValueOf(id))
		if v.IsValid() {
			if p, ok := v.Interface().(*T); ok {
				if p == nil {
					return nil, errors.New("null fill companion")
				}
				return p, nil
			}
			if value, ok := v.Interface().(T); ok {
				return &value, nil
			}
			return nil, errors.New("invalid fill companion type")
		}
	}
	if reader == nil {
		return nil, nil
	}
	record, found, err := reader.ReadArchive(kind, id)
	defer clear(record.Data)
	if err != nil || !found {
		return nil, err
	}
	if record.Kind != kind || record.ID != id {
		return nil, errors.New("fill companion identity changed")
	}
	var value *T
	if err = json.Unmarshal(record.Data, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("null cold fill companion")
	}
	return value, nil
}

// This validates immutable custody, not chain truth. In particular a restored
// commitment may have no locally saved funding bytes, and held/unknown outcomes
// never become settled simply because their records are structurally coherent.
func validateFillBinding(s *State, reader FillStateReader, id string) (*ParentOrder, *FillRecord, error) {
	f, err := fillValue[FillRecord](s, reader, "fill_records", id)
	if err != nil {
		return nil, nil, err
	}
	if f == nil {
		return nil, nil, errors.New("maker child allocation is missing")
	}
	if err = validateFillRecord(id, f); err != nil {
		return nil, nil, err
	}
	p, err := fillValue[ParentOrder](s, reader, "parent_orders", f.ParentID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, errors.New("child parent ledger is missing")
	}
	if err = validateParentOrder(f.ParentID, p); err != nil {
		return nil, nil, err
	}
	core, err := fillValue[Swap](s, reader, "swaps", id)
	if err != nil {
		return nil, nil, err
	}
	if core == nil || core.ID != id || core.Role != "maker" || core.Terms == nil {
		return nil, nil, errors.New("allocation lacks its accepted maker core")
	}
	offered, err := core.Request.Validate(int64(core.Request.OfferEvent.CreatedAt))
	if err != nil {
		return nil, nil, err
	}
	if err = core.Terms.Validate(); err != nil {
		return nil, nil, err
	}
	amounts, err := core.Request.Amounts()
	if err != nil {
		return nil, nil, err
	}
	if protocol.Digest(core.Request) != protocol.Digest(core.Terms.Request) || f.RequestDigest != protocol.Digest(core.Request) || core.Request.ID != id || f.ParentID != offered.ID || f.ParentMaker != offered.Maker || f.ParentRevision != core.Request.Revision || f.Allocation.Quantity != amounts.Sell || f.BuyAmount != amounts.Buy || p.Offer.Maker != offered.Maker || p.Economics != offered.EconomicsDigest() || p.Offer.Network.Normalized() != s.Network.Normalized() || f.FundingPolicy != p.FundingPolicy {
		return nil, nil, errors.New("child core, economics and parent authorization disagree")
	}
	long, short := core.Long, core.Short
	long.TxID, long.Vout = "", 0
	short.TxID, short.Vout = "", 0
	if long != core.Terms.Long || short != core.Terms.Short {
		return nil, nil, errors.New("saved child contracts differ from immutable terms")
	}
	fee, err := fillValue[FeeSelection](s, reader, "funding_fees", "swap/"+id)
	if err != nil {
		return nil, nil, err
	}
	if fee == nil || *fee != f.FundingPolicy || core.OwnerFeeCap != f.FundingPolicy.OwnerFeeCap {
		return nil, nil, errors.New("child funding fee differs from its exact authorization")
	}
	owner := int64(2000)
	if p.FundingPolicy.OwnerFeeCap > 0 {
		owner = p.FundingPolicy.OwnerFeeCap
	}
	if p.Offer.TowerBPS > 0 {
		owner = max(owner, protocol.RescueFees[len(protocol.RescueFees)-1])
	}
	refund := max(owner, protocol.RescueFees[len(protocol.RescueFees)-1])
	wantFees := map[chain.ID]int64{p.Offer.Sell: p.FundingPolicy.FundingFee + refund, p.Offer.Sell.Other(): owner}
	wantBounties := map[chain.ID]int64{p.Offer.Sell: protocol.Bounty(amounts.Sell, p.Offer.TowerBPS), p.Offer.Sell.Other(): protocol.Bounty(amounts.Buy, p.Offer.TowerBPS)}
	if !maps.Equal(f.Fees, wantFees) || !maps.Equal(f.Bounties, wantBounties) {
		return nil, nil, errors.New("child monetary charge differs from its reviewed policy")
	}
	if f.FundingDisabled && !f.Allocation.EverCommitted && (core.ShortFunding != "" || core.Short.TxID != "" || core.ShortSent || len(core.SelfRefunds) > 0 || len(core.Jobs) > 0) {
		return nil, nil, errors.New("never-funded refusal contradicts own funding evidence")
	}
	if !f.Allocation.EverCommitted && (core.ShortFunding != "" || core.ShortSent || len(core.SelfRefunds) > 0 || len(core.Jobs) > 0) {
		return nil, nil, errors.New("signed child funding authority lacks permanent commitment")
	}
	if core.ShortFunding != "" {
		bound, err := bindFunding(core.Terms.Short, core.ShortFunding)
		if err != nil {
			return nil, nil, err
		}
		if bound != core.Short {
			return nil, nil, errors.New("saved maker funding identity differs from child contract")
		}
		tx, err := contract.Parse(core.ShortFunding)
		if err != nil {
			return nil, nil, err
		}
		if len(tx.TxIn) != len(f.Inputs) {
			return nil, nil, errors.New("saved maker funding changed assigned inputs")
		}
		for i, input := range tx.TxIn {
			if input == nil || input.PreviousOutPoint.Hash.String() != f.Inputs[i].TxID || input.PreviousOutPoint.Index != f.Inputs[i].Vout {
				return nil, nil, errors.New("saved maker funding changed assigned inputs")
			}
		}
	}
	if r, ok := s.CoinReservations["swap/"+id]; ok {
		if r.Chain != p.Offer.Sell || !reflect.DeepEqual(r.Inputs, f.Inputs) {
			return nil, nil, errors.New("maker reservation differs from assigned child inputs")
		}
	}
	return p, f, nil
}

type fillTotalRow struct {
	Parent       string
	ParentRecord bool
	Totals       fillTotals
}
type fillInputRow struct{ Point, Owner string }

// ValidateFillConservation is a COMPLETED checkpoint check, never a validator
// for one archive row. It uses exact point reads and encrypted external sorting:
// one parent aggregate and at most 32 output rows are decoded at a time, without
// a lifetime parent/child map. directory is private disposable scratch storage.
func ValidateFillConservation(ctx context.Context, s *State, reader FillStateReader, directory string) error {
	_, err := validateFillConservation(ctx, s, reader, directory)
	return err
}
func validateFillConservation(ctx context.Context, s *State, reader FillStateReader, directory string) (map[string]string, error) {
	if err := ValidateStateVersion(s); err != nil {
		return nil, err
	}
	if len(s.Archive) != 0 {
		return nil, errors.New("completed conservation requires separated archive custody")
	}
	totals, err := storage.NewRowSorter(ctx, directory, func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
	if err != nil {
		return nil, err
	}
	defer totals.Close()
	inputs, err := storage.NewRowSorter(ctx, directory, func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
	if err != nil {
		return nil, err
	}
	defer inputs.Close()
	addTotal := func(row fillTotalRow) error {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		defer clear(raw)
		return totals.Add([]byte(row.Parent), raw)
	}
	addInput := func(point, owner string) error {
		raw, _ := json.Marshal(fillInputRow{point, owner})
		defer clear(raw)
		return inputs.Add([]byte(point), raw)
	}
	coldInputs := map[string]string{}
	visit := func(kind, id string, cold bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch kind {
		case "parent_orders":
			p, err := fillValue[ParentOrder](s, reader, kind, id)
			if err != nil {
				return err
			}
			if p == nil {
				return errors.New("parent vanished during validation")
			}
			if err = validateParentOrder(id, p); err != nil {
				return err
			}
			if p.Offer.Network.Normalized() != s.Network.Normalized() {
				return errors.New("parent belongs to another network")
			}
			return addTotal(fillTotalRow{Parent: id, ParentRecord: true})
		case "fill_records":
			p, f, err := validateFillBinding(s, reader, id)
			if err != nil {
				return fmt.Errorf("fill %s: %w", id, err)
			}
			if err = addTotal(fillTotalRow{Parent: f.ParentID, Totals: childFillTotals(f)}); err != nil {
				return err
			}
			if f.Allocation.Disposition == FillReserved || f.Allocation.Disposition == FillCommitted {
				for _, point := range f.Inputs {
					key := string(p.Offer.Sell) + "/" + pointKey(point)
					owner := "swap/" + id
					if err = addInput(key, owner); err != nil {
						return err
					}
					if cold {
						if other := coldInputs[key]; other != "" && other != owner {
							return errors.New("cold live children share an input")
						}
						coldInputs[key] = owner
					}
				}
			}
		case "swaps":
			core, err := fillValue[Swap](s, reader, kind, id)
			if err != nil {
				return err
			}
			if core == nil {
				return errors.New("child core vanished")
			}
			if core.Role == "maker" {
				f, err := fillValue[FillRecord](s, reader, "fill_records", id)
				if err != nil {
					return err
				}
				if f == nil {
					return errors.New("maker core has no retained allocation")
				}
			}
		}
		return nil
	}
	for _, kind := range []string{"parent_orders", "fill_records", "swaps"} {
		group, err := archiveMap(s, kind, false)
		if err != nil {
			return nil, err
		}
		if group.IsValid() {
			for _, key := range group.MapKeys() {
				if err = visit(kind, key.String(), false); err != nil {
					return nil, err
				}
			}
		}
	}
	if reader != nil {
		if err = reader.VisitArchive(ctx, func(record storage.ArchiveRecord) error {
			if record.Kind != "parent_orders" && record.Kind != "fill_records" && record.Kind != "swaps" {
				return nil
			}
			if _, err := ValidateArchiveRecordAgainstState(*s, record); err != nil {
				return err
			}
			return visit(record.Kind, record.ID, true)
		}); err != nil {
			return nil, err
		}
	}
	for owner, r := range s.CoinReservations {
		if !r.Chain.Valid() {
			return nil, errors.New("input reservation has invalid chain")
		}
		seen := map[string]bool{}
		for _, p := range r.Inputs {
			point := string(r.Chain) + "/" + pointKey(p)
			if !protocol.Hex32(p.TxID) || seen[point] {
				return nil, errors.New("invalid reserved input")
			}
			seen[point] = true
			if err = addInput(point, owner); err != nil {
				return nil, err
			}
		}
	}
	var current string
	var sum fillTotals
	var parents int
	finish := func() error {
		if current == "" {
			return nil
		}
		p, err := fillValue[ParentOrder](s, reader, "parent_orders", current)
		if err != nil {
			return err
		}
		if p == nil || parents != 1 || sum != parentFillTotals(p) {
			return errors.New("parent bins or monetary counters disagree with retained children")
		}
		return nil
	}
	if err = visitFillSorted(ctx, totals, func(raw []byte) error {
		var row fillTotalRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Parent != current {
			if err := finish(); err != nil {
				return err
			}
			current = row.Parent
			sum = fillTotals{}
			parents = 0
		}
		if row.ParentRecord {
			parents++
		}
		return sum.add(row.Totals, 1)
	}); err != nil {
		return nil, err
	}
	if err = finish(); err != nil {
		return nil, err
	}
	var previous fillInputRow
	if err = visitFillSorted(ctx, inputs, func(raw []byte) error {
		var row fillInputRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if previous.Point == row.Point && previous.Owner != row.Owner {
			return errors.New("live owners share an assigned funding input")
		}
		previous = row
		return nil
	}); err != nil {
		return nil, err
	}
	return coldInputs, ctx.Err()
}
func visitFillSorted(ctx context.Context, sorter *storage.RowSorter, visit func([]byte) error) error {
	rows, err := sorter.Finish()
	if err != nil {
		return err
	}
	defer rows.Close()
	for offset := uint64(0); offset < rows.Count; offset += 32 {
		batch, err := rows.Page(ctx, offset, 32)
		if err != nil {
			return err
		}
		for i, raw := range batch {
			err = visit(raw)
			clear(raw)
			if err != nil {
				for _, rest := range batch[i+1:] {
					clear(rest)
				}
				return err
			}
		}
	}
	return ctx.Err()
}

// A complete in-memory export already owns its lifetime records. This adapter
// is not used by streamed rows or by the ordinary save/tick path.
func ValidateCompleteFillState(s *State) error {
	return ValidateFillConservation(context.Background(), s, nil, "")
}

// This adapter visits only core kinds in bounded pages. The caller holds the
// vault exclusively, or owns an immutable private copy for the entire check.
type vaultFillReader struct{ v *storage.Vault }

func (r vaultFillReader) ReadArchive(kind, id string) (storage.ArchiveRecord, bool, error) {
	return r.v.ReadArchive(kind, id)
}
func (r vaultFillReader) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	for _, kind := range []string{"parent_orders", "fill_records", "swaps"} {
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, next, err := r.v.ArchivePage(kind, cursor, 32)
			if err != nil {
				return err
			}
			for i, record := range batch {
				err = visit(record)
				clear(record.Data)
				if err != nil {
					for _, rest := range batch[i+1:] {
						clear(rest.Data)
					}
					return err
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return ctx.Err()
}

func fillOwnerID(owner string) string {
	if strings.HasPrefix(owner, "swap/") {
		return strings.TrimPrefix(owner, "swap/")
	}
	return ""
}
