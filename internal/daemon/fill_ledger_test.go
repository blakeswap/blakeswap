package daemon

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/blakeswap/blakeswap/internal/protocol"
)

func checkFillOwnership(t *testing.T, l QuantityLedger, children []FillAllocation) {
	t.Helper()
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	bins := map[FillDisposition]int64{}
	for _, child := range children {
		if err := child.validate(); err != nil {
			t.Fatal(err)
		}
		bins[child.Disposition] += child.currentQuantity()
	}
	if bins[FillReserved] != l.Reserved || bins[FillCommitted] != l.Committed || bins[FillFilled] != l.Filled || bins[FillReleased]+l.Withdrawn != l.Released || bins[FillRetired] != 0 {
		t.Fatalf("parent/child ownership mismatch: %+v children=%+v", l, bins)
	}
}

func TestFillLedgerReturnRetiresCurrentAllocationAndPreservesChildren(t *testing.T) {
	l, _ := newQuantityLedger(1000000)
	l, first, err := l.reserveAllocation(300000)
	if err != nil {
		t.Fatal(err)
	}
	l, second, err := l.reserveAllocation(300000)
	if err != nil {
		t.Fatal(err)
	}
	l, first, err = l.transitionAllocation(first, FillRetired)
	if err != nil || first.Quantity != 300000 || first.currentQuantity() != 0 || l.Available != 700000 || l.Released != 0 {
		t.Fatalf("returned child must own zero: %+v %+v %v", l, first, err)
	}
	l, third, err := l.reserveAllocation(500000)
	if err != nil {
		t.Fatal(err)
	}
	l, second, err = l.transitionAllocation(second, FillCommitted)
	if err != nil {
		t.Fatal(err)
	}
	l, second, err = l.transitionAllocation(second, FillFilled)
	if err != nil {
		t.Fatal(err)
	}
	l, err = l.withdrawAvailable()
	if err != nil || l.Withdrawn != 200000 || l.Filled != 300000 || l.Reserved != 500000 {
		t.Fatalf("cancel changed accepted children: %+v %v", l, err)
	}
	l, third, err = l.transitionAllocation(third, FillReleased)
	if err != nil || third.EverCommitted || l.Released != 700000 {
		t.Fatalf("unfunded closed-parent release: %+v %+v %v", l, third, err)
	}
	before := l
	for _, child := range []FillAllocation{first, third} {
		for _, target := range []FillDisposition{FillReserved, FillCommitted, FillFilled} {
			after, _, err := l.transitionAllocation(child, target)
			if err == nil || after != before {
				t.Fatal("retired or never-funded closed child revived")
			}
		}
	}
	checkFillOwnership(t, l, []FillAllocation{first, second, third})
	// A contradiction of the genuinely funded child demotes only its amount.
	l, second, err = l.transitionAllocation(second, FillCommitted)
	if err != nil || l.Committed != 300000 || l.Filled != 0 || l.Released != 700000 || first.currentQuantity() != 0 || third.EverCommitted {
		t.Fatalf("child reorg changes unrelated allocations: %+v %v", l, err)
	}
	checkFillOwnership(t, l, []FillAllocation{first, second, third})
}

func TestFillLedgerRandomTransitionsSurviveSerializedRestart(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 911))
	for trial := 0; trial < 100; trial++ {
		l, _ := newQuantityLedger(protocol.MaxPrincipal)
		var children []FillAllocation
		for step := 0; step < 1000; step++ {
			if !l.Closed && l.Available >= protocol.MinPrincipal && (len(children) == 0 || rng.IntN(3) == 0) {
				q := protocol.MinPrincipal + rng.Int64N(l.Available-protocol.MinPrincipal+1)
				var c FillAllocation
				var err error
				l, c, err = l.reserveAllocation(q)
				if err != nil {
					t.Fatal(err)
				}
				children = append(children, c)
			} else if len(children) > 0 {
				i := rng.IntN(len(children))
				c := children[i]
				target := c.Disposition
				switch c.Disposition {
				case FillReserved:
					target = FillCommitted
					if rng.IntN(2) == 0 {
						target = FillRetired
						if l.Closed {
							target = FillReleased
						}
					}
				case FillCommitted:
					target = FillFilled
					if rng.IntN(2) == 0 {
						target = FillReleased
					}
				case FillFilled, FillReleased:
					if c.EverCommitted {
						target = FillCommitted
					}
				}
				var err error
				l, children[i], err = l.transitionAllocation(c, target)
				if err != nil {
					t.Fatal(err)
				}
			}
			if step == 750 {
				var err error
				l, err = l.withdrawAvailable()
				if err != nil {
					t.Fatal(err)
				}
			}
			checkFillOwnership(t, l, children)
			if step%100 == 0 {
				// Checks durable encoding semantics. Actual vault/failed-save and
				// wire retry coverage belongs to the engine integration tests.
				before, _ := json.Marshal(struct {
					Ledger   QuantityLedger
					Children []FillAllocation
				}{l, children})
				var restored struct {
					Ledger   QuantityLedger
					Children []FillAllocation
				}
				if err := json.Unmarshal(before, &restored); err != nil || !reflect.DeepEqual(l, restored.Ledger) || !reflect.DeepEqual(children, restored.Children) {
					t.Fatalf("allocation changed after reload: %v", err)
				}
				l, children = restored.Ledger, restored.Children
			}
		}
	}
}

func TestFillLedgerRefusesOverdrawCorruptionAndRevisionOverflow(t *testing.T) {
	l, _ := newQuantityLedger(1000000)
	if next, _, err := l.reserveAllocation(1000001); err == nil || next != l {
		t.Fatal("overdraw mutated quantity")
	}
	l.Revision = math.MaxUint64
	if next, _, err := l.reserveAllocation(100000); err == nil || next != l {
		t.Fatal("overflow wrapped revision or mutated quantity")
	}
	if next, err := l.withdrawAvailable(); err == nil || next != l {
		t.Fatal("overflow closed parent")
	}
	for _, bad := range []QuantityLedger{
		{Total: 1000000, Available: math.MaxInt64, Reserved: math.MaxInt64, Revision: 1},
		{Total: 1000000, Released: 1000000, Withdrawn: 1000001, Revision: 1},
		{Total: 1000000, Available: 1000000, Closed: true, Revision: 1},
	} {
		if err := bad.validate(); err == nil {
			t.Fatalf("corrupt quantity ledger accepted: %+v", bad)
		}
	}
}

func TestFillBudgetNeverReturnsConsumedAuthorization(t *testing.T) {
	b := FillBudget{Limit: math.MaxInt64}
	b, err := b.reserve(math.MaxInt64 - 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err = b.consume(math.MaxInt64 - 10)
	if err != nil {
		t.Fatal(err)
	}
	before := b
	if next, err := b.reserve(11); err == nil || next != before {
		t.Fatal("aggregate authorization overflow")
	}
	if next, err := b.returnReserved(1); err == nil || next != before {
		t.Fatal("consumed fee credited")
	}
	b, err = b.reserve(10)
	if err != nil {
		t.Fatal(err)
	}
	b, err = b.returnReserved(10)
	if err != nil || b != before {
		t.Fatal("unfunded reservation return changed permanent charge")
	}
}
