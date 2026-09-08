package daemon

import (
	"errors"

	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/wire"
)

// commitMakerFill is called after signing locally and before retaining or
// publishing those bytes. prepare saves this allocation and the funding bundle
// together; even an uncertain later publication can never return its budget.
func (e *Engine) commitMakerFill(s *Swap, tx *wire.MsgTx) error {
	child := e.s.FillRecords[s.ID]
	if child == nil || child.RequestDigest != protocol.Digest(s.Request) || child.FundingDisabled || child.ImportedUncertain || child.Allocation.Disposition != FillReserved || child.Allocation.EverCommitted || s.Terms == nil || child.ParentID != s.Terms.Offer().ID || child.Allocation.Quantity != s.Short.Amount || child.BuyAmount != s.Long.Amount {
		return errors.New("maker funding lacks the exact current child authorization")
	}
	parent := e.s.ParentOrders[child.ParentID]
	if parent == nil || parent.RestoreHold || len(tx.TxIn) != len(child.Inputs) {
		return errors.New("maker funding does not spend its assigned child inputs")
	}
	for i, point := range child.Inputs {
		input := tx.TxIn[i].PreviousOutPoint
		if input.Hash.String() != point.TxID || input.Index != point.Vout {
			return errors.New("maker funding changed a child input")
		}
	}
	next, value, err := parent.transitionFill(*child, FillCommitted, false)
	if err != nil {
		return err
	}
	*parent, *child = next, value
	return nil
}
