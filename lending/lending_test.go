package lending

import (
	"container/heap"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

type pending struct {
	at  time.Time
	g   *fairshare.Grant
	org string
	t0  time.Time
}

type pq []pending

func (q pq) Len() int             { return len(q) }
func (q pq) Less(i, j int) bool   { return q[i].at.Before(q[j].at) }
func (q pq) Swap(i, j int)        { q[i], q[j] = q[j], q[i] }
func (q *pq) Push(x any)          { *q = append(*q, x.(pending)) }
func (q *pq) Pop() any            { o := *q; x := o[len(o)-1]; *q = o[:len(o)-1]; return x }
func (q pq) peek() time.Time      { return q[0].at }
func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// drive runs the real fairshare.Pool on a random timeline of requests and
// returns what the gateway would have recorded: every request's org, end,
// latency, Borrowing flag as the pool decided it, and one slot-hour per hour
// of its duration as its cost.
func drive(seed int64, capacity int, shares map[string]float64, steps int) ([]Req, time.Time, time.Time) {
	rng := rand.New(rand.NewSource(seed))
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	start := clock
	pool := fairshare.NewPool("p", capacity, shares, 0, func() time.Time { return clock })
	names := []string{"a", "b", "c"}
	var q pq
	var out []Req
	finish := func(p pending) {
		clock = p.at
		p.g.Release(0)
		d := p.at.Sub(p.t0)
		out = append(out, Req{Org: p.org, Pool: "p", End: p.at, LatencyMS: float64(d.Milliseconds()), Borrowing: p.g.Borrowing, GPUHours: d.Hours()})
	}
	for i := 0; i < steps; i++ {
		next := clock.Add(time.Duration(1+rng.Intn(4000)) * time.Millisecond)
		for q.Len() > 0 && !q.peek().After(next) {
			finish(heap.Pop(&q).(pending))
		}
		clock = next
		// A bursty client: one org is much hotter than the others for a while.
		hot := names[(i/300)%3]
		org := hot
		if rng.Intn(3) == 0 {
			org = names[rng.Intn(3)]
		}
		if g, ok := pool.TryAcquire(org, fairshare.Interactive, 100); ok {
			heap.Push(&q, pending{at: clock.Add(time.Duration(500+rng.Intn(30000)) * time.Millisecond), g: g, org: org, t0: clock})
		}
	}
	for q.Len() > 0 {
		finish(heap.Pop(&q).(pending))
	}
	return out, start, clock.Add(time.Second)
}

// Property, against the real fair-share pool: the lending matrix reconciles
// with the pool's own accounting. Each borrower's column adds up to what the
// pool flagged as borrowed, the lent total equals the borrowed total, the
// nets sum to zero, a lender never lends more than its reservation left idle,
// and under TryAcquire (never past C) no borrowing is left without a lender.
func TestMatrixReconcilesWithFairShare(t *testing.T) {
	shares := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}
	for seed := int64(1); seed <= 25; seed++ {
		capacity := 4 + int(seed%9)
		reqs, from, to := drive(seed, capacity, shares, 1500)
		res, err := Compute(from, to, []Pool{{Name: "p", Capacity: capacity, Shares: shares}}, reqs)
		if err != nil {
			t.Fatal(err)
		}
		borrowed := map[string]float64{}
		inShare := map[string]float64{}
		for _, r := range reqs {
			if r.Borrowing {
				borrowed[r.Org] += r.GPUHours
			} else {
				inShare[r.Org] += r.GPUHours
			}
		}
		var sumLent, sumBorrowed, sumNet float64
		anyBorrow := false
		for _, o := range res.Orgs {
			col := o.SelfServed
			for _, lender := range res.Orgs {
				col += res.Matrix[lender.Org][o.Org]
			}
			if !near(col, borrowed[o.Org], 1e-6) || !near(o.Borrowed, borrowed[o.Org], 1e-9) || !near(o.UsedInShare, inShare[o.Org], 1e-9) {
				t.Fatalf("seed %d org %s: column %v, pool borrowed %v, stat %+v", seed, o.Org, col, borrowed[o.Org], o)
			}
			if o.Lent+o.SelfServed > o.GuaranteedHours-inShare[o.Org]+1e-6 {
				t.Fatalf("seed %d: %s lent %v of %v idle", seed, o.Org, o.Lent, o.GuaranteedHours-inShare[o.Org])
			}
			if res.Matrix[o.Org][o.Org] != 0 {
				t.Fatalf("seed %d: %s lends to itself", seed, o.Org)
			}
			sumLent += o.Lent
			sumBorrowed += o.Borrowed
			sumNet += o.Net
			anyBorrow = anyBorrow || o.Borrowed > 0
		}
		var self, fromOthers float64
		for _, o := range res.Orgs {
			self += o.SelfServed
			fromOthers += o.BorrowedFromOthers
		}
		if !near(sumLent, fromOthers, 1e-6) || !near(sumLent+self+res.Unattributed, sumBorrowed, 1e-6) || !near(sumNet, 0, 1e-6) {
			t.Fatalf("seed %d: lent %v + unattributed %v != borrowed %v", seed, sumLent, res.Unattributed, sumBorrowed)
		}
		if res.Unattributed > 1e-6 {
			t.Fatalf("seed %d: %v borrowed hours without an idle reservation under C", seed, res.Unattributed)
		}
		if !anyBorrow {
			t.Fatalf("seed %d: the scenario never borrowed; the property tested nothing", seed)
		}
	}
}

// A hand-computed case. Pool C=4, a=0.5 (2 slots), b=0.5 (2 slots). For one
// hour a runs 1 in-share request and b borrows 2 slots on top of its own 2:
// a has 1 idle slot, so b's borrowing is served by a, 1 slot-hour per slot...
func TestHandComputed(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := time.Hour
	reqs := []Req{
		{Org: "a", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1, Borrowing: true},
	}
	res, err := Compute(t0, t0.Add(2*h), []Pool{{Name: "p", Capacity: 4, Shares: map[string]float64{"a": .5, "b": .5}}}, reqs)
	if err != nil {
		t.Fatal(err)
	}
	a, b := res.Orgs[0], res.Orgs[1]
	if !near(a.GuaranteedHours, 4, 1e-9) || !near(a.UsedInShare, 1, 1e-9) || !near(a.Lent, 1, 1e-6) || !near(a.Net, 1, 1e-6) ||
		!near(b.Borrowed, 1, 1e-9) || !near(b.UsedInShare, 2, 1e-9) || !near(b.Net, -1, 1e-6) || !near(res.Matrix["a"]["b"], 1, 1e-6) {
		t.Fatalf("%+v", res)
	}
	if b.Borrow.N != 1 || b.InShare.N != 2 || b.Borrow.P50 != 3_600_000 {
		t.Fatalf("latency %+v", b)
	}
}

func TestBorrowingWithoutIdleIsUnattributed(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reqs := []Req{
		{Org: "a", Pool: "p", End: t0.Add(time.Hour), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(time.Hour), LatencyMS: 3_600_000, GPUHours: 1, Borrowing: true},
		{Org: "b", Pool: "gone", End: t0.Add(time.Hour), LatencyMS: 1000, GPUHours: .5, Borrowing: true},
	}
	res, _ := Compute(t0, t0.Add(time.Hour), []Pool{{Name: "p", Capacity: 1, Shares: map[string]float64{"a": 1}}}, reqs)
	if !near(res.Unattributed, 1.5, 1e-6) {
		t.Fatalf("unattributed %v", res.Unattributed)
	}
}

func TestBounds(t *testing.T) {
	t0 := time.Now()
	if _, err := Compute(t0, t0.Add(-time.Hour), nil, nil); err == nil {
		t.Fatal("inverted period accepted")
	}
	if _, err := Compute(t0, t0.Add(time.Hour), nil, make([]Req, MaxRequests+1)); err != ErrTooMany {
		t.Fatalf("%v", err)
	}
	// Hostile values never panic or produce NaN.
	res, err := Compute(t0, t0.Add(time.Hour), []Pool{{Name: "p", Capacity: 2, Shares: map[string]float64{"a": 1}}},
		[]Req{{Org: "a", Pool: "p", End: t0, LatencyMS: math.NaN(), GPUHours: math.Inf(1), Borrowing: true}, {Org: "b", Pool: "p", End: t0, LatencyMS: -5, GPUHours: -1}})
	if err != nil || math.IsNaN(res.Orgs[0].Borrowed) {
		t.Fatalf("%+v %v", res, err)
	}
}

func FuzzCompute(f *testing.F) {
	f.Add(int64(1), 3, 5, 0.5, 100.0)
	f.Fuzz(func(t *testing.T, ms int64, capacity, n int, share, lat float64) {
		t0 := time.UnixMilli(1_700_000_000_000)
		n = min(max(n, 0), 200)
		var reqs []Req
		for i := 0; i < n; i++ {
			reqs = append(reqs, Req{Org: string(rune('a' + i%3)), Pool: "p", End: t0.Add(time.Duration(ms%10_000_000+int64(i)) * time.Millisecond),
				LatencyMS: lat * float64(i), WaitMS: lat * float64(i%4), Borrowing: i%2 == 0, GPUHours: share * float64(i)})
		}
		c := min(max(capacity, 0), 1000)
		pool := Pool{Name: "p", Capacity: c, Shares: map[string]float64{"a": math.Abs(share), "b": 1},
			Timeline: []CapPoint{{t0.Add(time.Duration(ms%3_600_000) * time.Millisecond), c + 3}}}
		res, err := Compute(t0, t0.Add(time.Hour), []Pool{pool}, reqs)
		if err != nil {
			return
		}
		// Invariants, for any input: no NaN or negative figure, the nets sum
		// to zero, and what was borrowed is lent, self-served or unattributed.
		var borrowed, lent, self, net float64
		for _, o := range res.Orgs {
			for _, v := range []float64{o.GuaranteedHours, o.UsedInShare, o.Borrowed, o.Lent, o.SelfServed, o.BorrowedFromOthers} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < -1e-9 {
					t.Fatalf("bad figure %v in %+v", v, o)
				}
			}
			if math.IsNaN(o.Net) || math.IsInf(o.Net, 0) {
				t.Fatalf("net %+v", o)
			}
			borrowed += o.Borrowed
			lent += o.Lent
			self += o.SelfServed
			net += o.Net
			for _, l := range []Latency{o.InShare, o.Borrow, o.WaitInShare, o.WaitBorrow} {
				if l.N > 0 && !(l.P50 <= l.P95 && l.P95 <= l.P99) {
					t.Fatalf("percentiles out of order: %+v", l)
				}
			}
		}
		tol := 1e-6 * (1 + borrowed)
		if math.Abs(net) > tol || math.Abs(lent+self+res.Unattributed-borrowed) > tol {
			t.Fatalf("lent %v + self %v + unattributed %v != borrowed %v (net %v)", lent, self, res.Unattributed, borrowed, net)
		}
	})
}

// A pool fed by Slurm jobs has capacity = 0 in the configuration and a C that
// comes and goes. With its history the guarantee and the lending are those of
// the real C; with the configured value alone (the first version) every
// borrowing request was unattributed and nothing was guaranteed.
func TestSlurmPoolCapacityTimeline(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := time.Hour
	shares := map[string]float64{"a": .5, "b": .5}
	reqs := []Req{
		// b fills its own reservation (2 slots) and borrows one more, in the
		// second hour, when C = 4: a has 2 idle slots.
		{Org: "b", Pool: "p", End: t0.Add(2 * h), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(2 * h), LatencyMS: 3_600_000, GPUHours: 1},
		{Org: "b", Pool: "p", End: t0.Add(2 * h), LatencyMS: 3_600_000, GPUHours: 1, Borrowing: true},
	}
	tl := []CapPoint{{t0, 2}, {t0.Add(h), 4}}
	res, err := Compute(t0, t0.Add(2*h), []Pool{{Name: "p", Capacity: 0, Timeline: tl, Shares: shares}}, reqs)
	if err != nil {
		t.Fatal(err)
	}
	a, b := res.Orgs[0], res.Orgs[1]
	// a: 1 slot for an hour, then 2 for an hour = 3 slot-hours.
	if !near(a.GuaranteedHours, 3, 1e-9) || !near(b.GuaranteedHours, 3, 1e-9) || !near(a.Lent, 1, 1e-6) || !near(b.BorrowedFromOthers, 1, 1e-6) || res.Unattributed > 1e-9 {
		t.Fatalf("with the history: %+v unattributed %v", res.Orgs, res.Unattributed)
	}
	// The control: the configured capacity alone.
	res, _ = Compute(t0, t0.Add(2*h), []Pool{{Name: "p", Capacity: 0, Shares: shares}}, reqs)
	if res.Orgs[0].GuaranteedHours != 0 || res.Orgs[0].Lent != 0 || !near(res.Unattributed, 1, 1e-6) {
		t.Fatalf("control: %+v unattributed %v", res.Orgs, res.Unattributed)
	}
}

// The slot is held from the grant, not from the arrival: a request that waited
// half an hour in the gateway's queue did not hold its reservation then.
func TestAdmissionWaitIsNotSlotTime(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := time.Hour
	pool := []Pool{{Name: "p", Capacity: 2, Shares: map[string]float64{"a": .5, "b": .5}}}
	mk := func(wait float64) []Req {
		return []Req{
			{Org: "a", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, WaitMS: wait, GPUHours: 1},    // a's in-share request
			{Org: "b", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1},                  // b fills its reservation
			{Org: "b", Pool: "p", End: t0.Add(h), LatencyMS: 3_600_000, GPUHours: 1, Borrowing: true}, // and borrows
		}
	}
	// a waited 30 min: its slot was idle for the first half hour, so a lent
	// 0.5 GPU-hour then; the second half hour a was busy, nothing to lend.
	res, _ := Compute(t0, t0.Add(h), pool, mk(1_800_000))
	if a := res.Orgs[0]; !near(a.Lent, 0.5, 1e-6) || !near(res.Unattributed, 0.5, 1e-6) {
		t.Fatalf("with the wait: lent %v unattributed %v", a.Lent, res.Unattributed)
	}
	// The same requests counted from arrival (the first version): a never
	// idle, nothing lent.
	res, _ = Compute(t0, t0.Add(h), pool, mk(0))
	if a := res.Orgs[0]; a.Lent != 0 || !near(res.Unattributed, 1, 1e-6) {
		t.Fatalf("without the wait: lent %v unattributed %v", a.Lent, res.Unattributed)
	}
	// The wait is its own figure.
	res, _ = Compute(t0, t0.Add(h), pool, mk(1_800_000))
	if w := res.Orgs[0].WaitInShare; w.N != 1 || w.P50 != 1_800_000 {
		t.Fatalf("wait percentiles %+v", w)
	}
}
