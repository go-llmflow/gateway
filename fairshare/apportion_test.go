package fairshare

import (
	"fmt"
	"math"
	"sort"
	"testing"
)

func TestApportionNeverExceedsCapacity(t *testing.T) {
	vectors := [][]float64{
		{0.8125, 0.1875},
		{0.5, 0.3, 0.2},
		{1.0 / 3, 1.0 / 3, 1.0 / 3},
		{0.6, 0.3},       // sums to 0.9
		{0.7, 0.2, 0.05}, // sums to 0.95
		{0.999, 0.001},
		{0.1},
		{0.45, 0.45, 0.1, 0}, // a zero share
		{0.25, 0.25, 0.25, 0.25},
	}
	for _, v := range vectors {
		shares := map[string]float64{}
		var names []string
		sum := 0.0
		for i, s := range v {
			n := fmt.Sprintf("t%d", i)
			shares[n] = s
			names = append(names, n)
			sum += s
		}
		sort.Strings(names)
		for c := 1; c <= 200; c++ {
			r := Apportion(c, names, shares)
			total := 0
			for _, n := range names {
				q := shares[n] * float64(c)
				// Hamilton's quota property: floor(q) <= r <= floor(q)+1.
				if f := int(math.Floor(q + 1e-9)); r[n] < f || r[n] > f+1 {
					t.Fatalf("shares %v C=%d: %s gets %d for quota %.4f", v, c, n, r[n], q)
				}
				total += r[n]
			}
			if total > c {
				t.Fatalf("shares %v C=%d: reservations sum to %d", v, c, total)
			}
			if math.Abs(sum-1) < 1e-9 && total != c {
				t.Fatalf("shares %v sum to 1 but C=%d is apportioned to %d", v, c, total)
			}
		}
	}
}

func TestApportionExamples(t *testing.T) {
	names := []string{"etabA", "etabB"}
	shares := map[string]float64{"etabA": 0.8125, "etabB": 0.1875}
	for c, want := range map[int][2]int{10: {8, 2}, 16: {13, 3}, 64: {52, 12}, 256: {208, 48}, 1: {1, 0}} {
		r := Apportion(c, names, shares)
		if r["etabA"] != want[0] || r["etabB"] != want[1] {
			t.Errorf("C=%d: %v, want %v", c, r, want)
		}
	}
	// The rounding-up rule this replaces: 9 + 2 = 11 slots for C = 10.
	if math.Ceil(8.125)+math.Ceil(1.875) != 11 {
		t.Fatal("arithmetic")
	}
	p := NewPool("p", 10, shares, 0, nil)
	if st := p.Snapshot(); st[0].Reserved != 8 || st[1].Reserved != 2 {
		t.Fatalf("snapshot reservations %+v", st)
	}
}

// At C = 10, A must never hold more than C - reserved_B = 8 requests at
// within-share priority, so B's within-share requests never queue behind
// more than that many equal-priority requests of A.
func TestSmallPoolKeepsBGuarantee(t *testing.T) {
	const c = 10
	maxA, bWaitingSeen, maxBehind := 0, 0, 0
	observe := func(e *engine) {
		aWithin := 0
		for _, r := range e.running {
			if r.team == "etabA" && !r.grant.Borrowing {
				aWithin++
			}
		}
		for _, r := range e.waiting {
			if r.team == "etabA" && !r.grant.Borrowing {
				aWithin++
			}
		}
		if aWithin > maxA {
			maxA = aWithin
		}
		for _, r := range e.waiting {
			if r.team != "etabB" || r.grant.Borrowing {
				continue
			}
			bWaitingSeen++
			// Priority-0 requests of A that run or wait ahead of it.
			ahead := 0
			for _, o := range e.running {
				if o.team == "etabA" && o.grant.Priority <= r.grant.Priority {
					ahead++
				}
			}
			for _, o := range e.waiting {
				if o.team == "etabA" && o.grant.Priority <= r.grant.Priority && o.arrival < r.arrival {
					ahead++
				}
			}
			if ahead > maxBehind {
				maxBehind = ahead
			}
		}
	}
	r := simulateC(t, scenario{
		clients: map[string]int{"etabA": 40, "etabB": 40},
		start:   map[string]int{"etabA": 0, "etabB": 200},
	}, 6000, 0, c, c*(simPrompt+simLength), observe)
	reservedB := r.pool.Snapshot()[1].Reserved
	if reservedB != 2 {
		t.Fatalf("reserved B %d", reservedB)
	}
	if maxA > c-reservedB {
		t.Fatalf("A held %d within-share requests, more than C - reserved_B = %d", maxA, c-reservedB)
	}
	if maxBehind > c-reservedB {
		t.Fatalf("a within-share request of B waited behind %d equal-priority requests of A", maxBehind)
	}
	if maxA != c-reservedB {
		t.Fatalf("A never filled its reservation (max %d): the scenario does not saturate", maxA)
	}
	for _, v := range r.e.preempted {
		if !v.grant.Borrowing {
			t.Fatalf("within-share request of %s preempted at C=%d", v.team, c)
		}
	}
	t.Logf("C=%d: A within-share max %d, B waited %d step-requests, at most behind %d of A; %d preemptions, all borrowers",
		c, maxA, bWaitingSeen, maxBehind, len(r.e.preempted))
}

// An elastic pool (#75): SetCapacity re-apportions exactly as NewPool
// would at the new C, and requests in flight keep their status.
func TestSetCapacityReapportions(t *testing.T) {
	shares := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}
	p := NewPool("p", 4, shares, 0, nil)
	g := p.Acquire("a", Interactive, 1)
	g2 := p.Acquire("a", Interactive, 1)
	if g.Borrowing || g2.Borrowing {
		t.Fatal("a's 2 within-share slots at C=4")
	}
	for _, c := range []int{0, 1, 7, 10, 3, 4} {
		if old := p.SetCapacity(c); old == c && c != 4 {
			t.Fatalf("SetCapacity(%d) returned %d", c, old)
		}
		want := NewPool("w", c, shares, 0, nil).Reserved()
		got := p.Reserved()
		if p.Capacity() != c || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("C=%d: reserved %v, want %v", c, got, want)
		}
	}
	p.SetCapacity(0)
	if g3 := p.Acquire("a", Interactive, 1); !g3.Borrowing {
		t.Error("C=0: a new request is within share")
	} else {
		g3.Release(0)
	}
	if _, ok := p.TryAcquire("b", Interactive, 1); ok {
		t.Error("TryAcquire at C=0 with 2 in flight")
	}
	g.Release(0)
	g2.Release(0)
	if p.SetCapacity(-3); p.Capacity() != 0 {
		t.Error("negative capacity")
	}
}
