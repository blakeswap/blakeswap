package protocol

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
)

func TestFillPartitionMatchesExhaustiveReachability(t *testing.T) {
	for minimum := int64(1); minimum <= 25; minimum++ {
		for maximum := minimum; maximum <= 40; maximum++ {
			reachable := make([]bool, 301)
			reachable[0] = true
			for n := 1; n < len(reachable); n++ {
				for q := minimum; q <= maximum && q <= int64(n); q++ {
					reachable[n] = reachable[n] || reachable[n-int(q)]
				}
				if got := Partitionable(int64(n), minimum, maximum); got != reachable[n] {
					t.Fatalf("amount=%d range=%d..%d got=%v reachable=%v", n, minimum, maximum, got, reachable[n])
				}
			}
		}
	}
	if !Partitionable(math.MaxInt64, 1, math.MaxInt64) || Partitionable(-1, 1, 2) || Partitionable(1, 0, 2) || Partitionable(1, 2, 1) {
		t.Fatal("partition arithmetic boundary")
	}
}

func TestFillRoundingPreservesAggregateRateAndOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewPCG(71, 13))
	for trial := 0; trial < 10000; trial++ {
		a := MinPrincipal + rng.Int64N(MaxPrincipal-MinPrincipal+1)
		b := MinPrincipal + rng.Int64N(MaxPrincipal-MinPrincipal+1)
		count := 1 + rng.IntN(20)
		left := a
		sum := new(big.Int)
		quantities := make([]int64, 0, count)
		for i := 0; i < count && left > 0; i++ {
			q := left
			if i < count-1 {
				q = 1 + rng.Int64N(left)
			}
			quantities = append(quantities, q)
			got, err := RoundedBuy(a, b, q)
			if err != nil {
				t.Fatal(err)
			}
			paid := new(big.Int).Mul(big.NewInt(got), big.NewInt(a))
			price := new(big.Int).Mul(big.NewInt(q), big.NewInt(b))
			if paid.Cmp(price) < 0 || new(big.Int).Sub(paid, price).Cmp(big.NewInt(a)) >= 0 {
				t.Fatalf("not minimal safe ceiling: %d/%d q=%d got=%d", b, a, q, got)
			}
			sum.Add(sum, big.NewInt(got))
			left -= q
		}
		if left != 0 || sum.Cmp(big.NewInt(b)) < 0 || new(big.Int).Sub(new(big.Int).Set(sum), big.NewInt(b)).Cmp(big.NewInt(int64(len(quantities)))) >= 0 {
			t.Fatalf("aggregate inequality failed for %d/%d", b, a)
		}
		rng.Shuffle(len(quantities), func(i, j int) { quantities[i], quantities[j] = quantities[j], quantities[i] })
		reordered := new(big.Int)
		for _, q := range quantities {
			got, _ := RoundedBuy(a, b, q)
			reordered.Add(reordered, big.NewInt(got))
		}
		if reordered.Cmp(sum) != 0 {
			t.Fatal("child price changed with fill ordering")
		}
	}
	if got, err := RoundedBuy(MaxPrincipal, MaxPrincipal, MaxPrincipal); err != nil || got != MaxPrincipal {
		t.Fatalf("10^20 intermediate: %d %v", got, err)
	}
}

func TestFillIntervalRoundingAndDustTails(t *testing.T) {
	p := FillPolicy{Mode: FillPartial, Min: MinPrincipal, Max: 1000000}
	m, max, err := p.Interval(1000000, 333333)
	if err != nil || m != 299998 || max != 1000000 {
		t.Fatalf("inverse ceil boundary: %d..%d %v", m, max, err)
	}
	if _, err := p.Quote(1000000, 333333, 1000000, m-1); err == nil {
		t.Fatal("below rounded minimum accepted")
	}
	if q, err := p.Quote(1000000, 333333, 1000000, m); err != nil || q.Buy != MinPrincipal {
		t.Fatalf("first valid quantity rejected: %+v %v", q, err)
	}
	if _, err := p.Quote(1000000, 333333, 1000000, 800000); err == nil {
		t.Fatal("uneconomic remainder accepted")
	}
	if q, err := p.Quote(1000000, 333333, 600000, 600000); err != nil || q.Remaining != 0 || q.Buy != 200000 {
		t.Fatalf("legal last child: %+v %v", q, err)
	}
	for _, bad := range []FillPolicy{{}, {Mode: FillWhole, Min: MinPrincipal, Max: 1000000}, {Mode: FillPartial, Min: 600000, Max: 700000}, {Mode: FillPartial, Min: 1, Max: 1000000}} {
		if _, _, err := bad.Interval(1000000, 1000000); err == nil {
			t.Fatalf("invalid policy accepted: %+v", bad)
		}
	}
	whole := FillPolicy{Mode: FillWhole, Min: 1000000, Max: 1000000}
	if _, err := whole.Quote(1000000, 333333, 1000000, 500000); err == nil {
		t.Fatal("whole offer accepted a partial child")
	}
}

func TestFillBudgetCheckedWideProducts(t *testing.T) {
	if got, err := CheckedBudget(100000, 100000); err != nil || got != 10000000000 {
		t.Fatalf("bounded budget: %d %v", got, err)
	}
	for _, p := range [][2]int64{{math.MaxInt64, 2}, {-1, 1}, {1, -1}} {
		if _, err := CheckedBudget(p[0], p[1]); err == nil {
			t.Fatalf("unsafe budget accepted: %v", p)
		}
	}
}
