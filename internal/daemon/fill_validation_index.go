package daemon

import (
	"context"
	"errors"
	"reflect"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type fillInputChange struct{ old, next *FillRecord }

func (c *fillValidationCheckpoint) abort() {
	if c == nil {
		return
	}
	if c.pending != nil {
		c.pending.Rollback()
		c.pending = nil
	}
	if c.freshInputs {
		_ = c.inputs.Close()
		c.freshInputs = false
	}
}
func (c *fillValidationCheckpoint) commit() error {
	if c.pending != nil {
		if err := c.pending.Commit(); err != nil {
			return err
		}
		c.pending = nil
	}
	c.freshInputs = false
	return nil
}

// The first completed pass writes only a bounded batch to the disposable B-tree.
// Later passes stage exact affected-key changes in one rollbackable transaction;
// the caller commits it only after the authoritative vault commit succeeds.
func (e *Engine) prepareFillInputIndex(reader FillStateReader, changes map[string]fillInputChange) (*fillValidationCheckpoint, error) {
	ctx := context.Background()
	previous := e.fillValidation
	if previous == nil || previous.inputs == nil {
		directory := ""
		if e.vault != nil {
			directory = e.vault.PrivateDirectory()
		}
		index, err := storage.NewPrivateIndex(ctx, directory)
		if err != nil {
			return nil, err
		}
		var tx *storage.PrivateIndexTx
		success := false
		defer func() {
			if tx != nil {
				tx.Rollback()
			}
			if !success {
				_ = index.Close()
			}
		}()
		batch := 0
		err = validateFillConservation(ctx, &e.s, reader, directory, func(row fillInputRow) error {
			if tx == nil {
				tx, err = index.Begin(ctx)
				if err != nil {
					return err
				}
			}
			if err := tx.Assign(row.Point, row.Owner); err != nil {
				return err
			}
			batch++
			if batch == 64 {
				err := tx.Commit()
				tx = nil
				batch = 0
				return err
			}
			return nil
		})
		if err == nil && tx != nil {
			err = tx.Commit()
			tx = nil
		}
		if err != nil {
			return nil, err
		}
		if e.vault != nil {
			if err := e.vault.OwnPrivateIndex(index); err != nil {
				return nil, err
			}
		}
		checkpoint := captureFillValidation(&e.s, index)
		checkpoint.freshInputs = true
		success = true
		return checkpoint, nil
	}

	// These two maps contain only active/touched component inputs, never the
	// untouched cold population. The latter remains behind exact encrypted keys.
	oldOwners, newOwners := map[string]string{}, map[string]string{}
	add := func(owners map[string]string, point, owner string) error {
		if previous := owners[point]; previous != "" && previous != owner {
			return errors.New("live owners share assigned inputs")
		}
		owners[point] = owner
		return nil
	}
	oldState := State{Version: e.s.Version, Network: e.s.Network, ParentOrders: previous.parents}
	var oldReader FillStateReader
	if e.vault != nil {
		oldReader = vaultFillReader{e.vault}
	}
	child := func(owners map[string]string, state *State, reader FillStateReader, f *FillRecord) error {
		if f == nil || (f.Allocation.Disposition != FillReserved && f.Allocation.Disposition != FillCommitted) {
			return nil
		}
		parent, err := fillValue[ParentOrder](state, reader, "parent_orders", f.ParentID)
		if err != nil {
			return err
		}
		if parent == nil {
			return errors.New("live child parent missing")
		}
		if r, ok := state.CoinReservations["swap/"+f.ID]; ok && (r.Chain != parent.Offer.Sell || !reflect.DeepEqual(r.Inputs, f.Inputs)) {
			return errors.New("live maker assignment changed")
		}
		for _, point := range f.Inputs {
			if err := add(owners, string(parent.Offer.Sell)+"/"+pointKey(point), "swap/"+f.ID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, change := range changes {
		if err := child(oldOwners, &oldState, oldReader, change.old); err != nil {
			return nil, err
		}
		if err := child(newOwners, &e.s, reader, change.next); err != nil {
			return nil, err
		}
	}
	reservations := func(owners map[string]string, reservations map[string]CoinReservation) error {
		for owner, reservation := range reservations {
			if !reservation.Chain.Valid() {
				return errors.New("invalid reservation chain")
			}
			seen := map[string]bool{}
			for _, point := range reservation.Inputs {
				key := string(reservation.Chain) + "/" + pointKey(point)
				if !protocol.Hex32(point.TxID) || seen[key] {
					return errors.New("invalid duplicate reservation input")
				}
				seen[key] = true
				if err := add(owners, key, owner); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := reservations(oldOwners, previous.reservations); err != nil {
		return nil, err
	}
	if err := reservations(newOwners, e.s.CoinReservations); err != nil {
		return nil, err
	}
	tx, err := previous.inputs.Begin(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			tx.Rollback()
		}
	}()
	for point, owner := range oldOwners {
		if err := tx.Release(point, owner); err != nil {
			return nil, err
		}
	}
	for point, owner := range newOwners {
		if err := tx.Assign(point, owner); err != nil {
			return nil, err
		}
	}
	checkpoint := captureFillValidation(&e.s, previous.inputs)
	checkpoint.pending = tx
	success = true
	return checkpoint, nil
}
