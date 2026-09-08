package daemon

import (
	"errors"
	"math"
	"math/big"

	"github.com/blakeswap/blakeswap/internal/protocol"
)

type FillDisposition string

const (
	FillReserved  FillDisposition = "reserved"
	FillCommitted FillDisposition = "committed"
	FillFilled    FillDisposition = "filled"
	FillReleased  FillDisposition = "released"
	FillRetired   FillDisposition = "retired"
)

// QuantityLedger is the compact parent aggregate. Withdrawn is the part of
// Released owned directly by parent cancellation, not another conserved bin.
// Exact child identities and allocations may be active or authenticated cold
// records; moving them between those locations cannot alter this aggregate.
type QuantityLedger struct {
	Total     int64  `json:"total"`
	Available int64  `json:"available"`
	Reserved  int64  `json:"reserved"`
	Committed int64  `json:"committed"`
	Filled    int64  `json:"filled"`
	Released  int64  `json:"released"`
	Withdrawn int64  `json:"withdrawn"`
	Revision  uint64 `json:"revision"`
	Closed    bool   `json:"closed"`
}

// FillAllocation retains immutable original quantity after a return. Retired
// means current allocation zero, not an amount also present in Released.
// EverCommitted is monotonic and prevents an old funded refund being treated
// as a never-funded return after restart or a chain contradiction.
type FillAllocation struct {
	Quantity      int64           `json:"quantity"`
	Disposition   FillDisposition `json:"disposition"`
	EverCommitted bool            `json:"ever_committed"`
}

func newQuantityLedger(total int64) (QuantityLedger, error) {
	l := QuantityLedger{Total: total, Available: total, Revision: 1}
	return l, l.validate()
}

func (l QuantityLedger) validate() error {
	if l.Total < protocol.MinPrincipal || l.Total > protocol.MaxPrincipal || l.Revision == 0 || l.Withdrawn < 0 || l.Withdrawn > l.Released || (l.Closed && l.Available != 0) {
		return errors.New("invalid parent quantity ledger")
	}
	sum := new(big.Int)
	for _, n := range []int64{l.Available, l.Reserved, l.Committed, l.Filled, l.Released} {
		if n < 0 || n > l.Total {
			return errors.New("invalid parent quantity bin")
		}
		sum.Add(sum, big.NewInt(n))
	}
	if sum.Cmp(big.NewInt(l.Total)) != 0 {
		return errors.New("parent quantity conservation failed")
	}
	return nil
}

func (a FillAllocation) validate() error {
	if a.Quantity < protocol.MinPrincipal || a.Quantity > protocol.MaxPrincipal {
		return errors.New("invalid child allocation quantity")
	}
	switch a.Disposition {
	case FillReserved, FillRetired:
		if a.EverCommitted {
			return errors.New("funded child cannot become reserved or retired")
		}
	case FillCommitted, FillFilled:
		if !a.EverCommitted {
			return errors.New("funded allocation lacks durable commitment")
		}
	case FillReleased:
	default:
		return errors.New("invalid child allocation disposition")
	}
	return nil
}

func (a FillAllocation) currentQuantity() int64 {
	if a.Disposition == FillRetired {
		return 0
	}
	return a.Quantity
}

func (l *QuantityLedger) advanceRevision() error {
	if l.Revision == math.MaxUint64 {
		return errors.New("parent revision exhausted")
	}
	l.Revision++
	return l.validate()
}

// reserveAllocation is quantity arithmetic only. The engine must first bind a
// new child identity, signed current revision, legal tail and exact resources,
// then persist the returned values together with its acceptance outbox.
func (l QuantityLedger) reserveAllocation(quantity int64) (QuantityLedger, FillAllocation, error) {
	a := FillAllocation{Quantity: quantity, Disposition: FillReserved}
	if err := l.validate(); err != nil {
		return l, FillAllocation{}, err
	}
	if err := a.validate(); err != nil {
		return l, FillAllocation{}, err
	}
	if l.Closed || quantity > l.Available {
		return l, FillAllocation{}, errors.New("parent quantity unavailable")
	}
	next := l
	next.Available -= quantity
	next.Reserved += quantity
	if err := next.advanceRevision(); err != nil {
		return l, FillAllocation{}, err
	}
	return next, a, nil
}

// transitionAllocation checks accounting and monotonic dispositions, not chain
// authority. Its caller must establish the corresponding positive proof or
// irreversible never-funding decision before requesting the transition.
// Returning values rather than editing pointers leaves all originals intact
// on errors; persistence must still commit parent and child in one transaction.
func (l QuantityLedger) transitionAllocation(a FillAllocation, to FillDisposition) (QuantityLedger, FillAllocation, error) {
	if err := l.validate(); err != nil {
		return l, a, err
	}
	if err := a.validate(); err != nil {
		return l, a, err
	}
	if a.Disposition == to {
		return l, a, nil
	}
	allowed := false
	switch a.Disposition {
	case FillReserved:
		allowed = to == FillCommitted || (to == FillRetired && !l.Closed) || (to == FillReleased && l.Closed)
	case FillCommitted:
		allowed = to == FillFilled || to == FillReleased
	case FillFilled:
		allowed = to == FillCommitted
	case FillReleased:
		allowed = a.EverCommitted && to == FillCommitted
	}
	if !allowed {
		return l, a, errors.New("invalid child quantity transition")
	}
	next, child := l, a
	bin := func(d FillDisposition) *int64 {
		switch d {
		case FillReserved:
			return &next.Reserved
		case FillCommitted:
			return &next.Committed
		case FillFilled:
			return &next.Filled
		case FillReleased:
			return &next.Released
		}
		return nil
	}
	from := bin(a.Disposition)
	if from == nil || *from < a.Quantity || (a.Disposition == FillReleased && *from-next.Withdrawn < a.Quantity) {
		return l, a, errors.New("child allocation exceeds its parent bin")
	}
	*from -= a.Quantity
	if to == FillRetired {
		next.Available += a.Quantity
	} else {
		*bin(to) += a.Quantity
	}
	child.Disposition = to
	child.EverCommitted = child.EverCommitted || to == FillCommitted
	if err := child.validate(); err != nil {
		return l, a, err
	}
	if err := next.advanceRevision(); err != nil {
		return l, a, err
	}
	return next, child, nil
}

func (l QuantityLedger) withdrawAvailable() (QuantityLedger, error) {
	if err := l.validate(); err != nil {
		return l, err
	}
	if l.Closed {
		return l, nil
	}
	next := l
	next.Withdrawn += next.Available
	next.Released += next.Available
	next.Available = 0
	next.Closed = true
	if err := next.advanceRevision(); err != nil {
		return l, err
	}
	return next, nil
}

// FillBudget records one asset's hard authorization ceiling. Consumed is
// permanent: settlement/refund/reorg cannot credit it. A safely retired
// never-funded child can return only its still-reserved authorization.
type FillBudget struct {
	Limit       int64 `json:"limit"`
	Reserved    int64 `json:"reserved"`
	Consumed    int64 `json:"consumed"`
	Transferred int64 `json:"transferred"`
}

func (b FillBudget) validate() error {
	if b.Limit < 0 || b.Reserved < 0 || b.Consumed < 0 || b.Consumed > b.Limit || b.Reserved > b.Limit-b.Consumed || b.Transferred < 0 || b.Transferred > b.Limit-b.Consumed-b.Reserved {
		return errors.New("invalid per-asset fill budget")
	}
	return nil
}

func (b FillBudget) reserve(amount int64) (FillBudget, error) {
	if err := b.validate(); err != nil {
		return b, err
	}
	if amount < 0 || amount > b.Limit-b.Consumed-b.Reserved-b.Transferred {
		return b, errors.New("parent fee or bounty authorization exhausted")
	}
	b.Reserved += amount
	return b, nil
}

func (b FillBudget) consume(amount int64) (FillBudget, error) {
	if err := b.validate(); err != nil {
		return b, err
	}
	if amount < 0 || amount > b.Reserved {
		return b, errors.New("child has no matching fee reservation")
	}
	b.Reserved -= amount
	b.Consumed += amount
	return b, nil
}

func (b FillBudget) returnReserved(amount int64) (FillBudget, error) {
	if err := b.validate(); err != nil {
		return b, err
	}
	if amount < 0 || amount > b.Reserved {
		return b, errors.New("cannot return consumed fill authorization")
	}
	b.Reserved -= amount
	return b, nil
}

// transfer removes only unused authorization from this parent. The original
// reviewed limit and permanent consumption remain available for audit.
func (b FillBudget) transfer(amount int64) (FillBudget, error) {
	if err := b.validate(); err != nil {
		return b, err
	}
	if amount < 0 || amount > b.Limit-b.Consumed-b.Reserved-b.Transferred {
		return b, errors.New("replacement exceeds unassigned parent authorization")
	}
	b.Transferred += amount
	return b, nil
}
