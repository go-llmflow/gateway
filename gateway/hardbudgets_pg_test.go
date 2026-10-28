package gateway

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// The property test of #87 on a real PostgreSQL (LLMFLOW_TEST_PG_DSN): three
// replicas admit random concurrent requests against the same budgets
// through the escrow, each request costing a random actual ≤ W; replicas
// are killed (their in-flight requests lost, their spooled records
// replayed by their next process, which then releases their grants) and
// restarted; the period rolls over; the limit is lowered and raised. At
// every sample and at the end, every period's spent is within its limit.
//
// Planted control: the same run with the pre-#87 admission (spent < limit,
// spent read from the store at a refresh, the cost charged at the end) must
// exceed the limit. If it does not, the test cannot see an overshoot.

const propLimit = 10 * nanoPerUnit // 10 units

type propClock struct{ ns atomic.Int64 }

func (c *propClock) now() time.Time { return time.Unix(0, c.ns.Load()).UTC() }

type propReplica struct {
	name string
	mu   sync.Mutex
	e    *escrow
	inc  string
	dead bool
	// spool holds the records of finished requests not yet stored; a
	// killed replica's spool is replayed by its next process.
	spool []store.Record
	// old: spools of dead incarnations waiting for the replay.
	old map[string][]store.Record
	// control mode: the store's spend as of the last refresh, plus the
	// cost of what finished since.
	seen  map[int64]int64
	local map[int64]int64
}

type propRun struct {
	t         *testing.T
	st        *store.Postgres
	clk       *propClock
	keys      []store.BudgetKey
	control   bool
	limit     atomic.Int64 // the team budget's current max, nano
	mu        sync.Mutex
	maxLim    map[int64]int64 // period start → the largest limit it had
	admits    atomic.Int64
	refused   atomic.Int64
	kills     atomic.Int64
	ids       atomic.Int64
	violation atomic.Pointer[string]
}

func (p *propRun) refs(now time.Time) []budgetRef {
	out := make([]budgetRef, len(p.keys))
	for i, k := range p.keys {
		out[i] = budgetRef{key: k, start: store.Day.Start(now)}
	}
	return out
}

func (p *propRun) restart(r *propReplica) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inc = fmt.Sprintf("%s-%d", r.name, p.ids.Add(1))
	r.e = newEscrow(p.st, store.Holder{Replica: r.name, Incarnation: r.inc}, 0.1, 50*time.Millisecond, time.Minute, p.clk.now)
	r.dead = false
	r.seen, r.local = map[int64]int64{}, map[int64]int64{}
}

// replay is the next process adopting its dead predecessors' spools: the
// records first, then (only once they are stored) the grants' release.
func (p *propRun) replay(ctx context.Context, r *propReplica) {
	r.mu.Lock()
	old := r.old
	r.old = map[string][]store.Record{}
	r.mu.Unlock()
	for inc, recs := range old {
		if err := p.st.InsertRecords(ctx, recs); err != nil {
			p.t.Error(err)
			return
		}
		if _, err := p.st.ReleaseIncarnation(ctx, inc); err != nil {
			p.t.Error(err)
		}
	}
}

func (p *propRun) flush(ctx context.Context, r *propReplica) {
	r.mu.Lock()
	recs := r.spool
	r.spool = nil
	r.mu.Unlock()
	if len(recs) > 0 {
		if err := p.st.InsertRecords(ctx, recs); err != nil {
			p.t.Error(err)
		}
	}
}

// request is one request on replica r: admitted (or not), served, settled.
func (p *propRun) request(ctx context.Context, r *propReplica, rng *rand.Rand) {
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return
	}
	e, inc := r.e, r.inc
	r.mu.Unlock()
	now := p.clk.now()
	start := store.Day.Start(now).UnixNano()
	w := 1 + rng.Int64N(propLimit/8)
	var h *hold
	if p.control {
		r.mu.Lock()
		ok := r.seen[start]+r.local[start] < p.limit.Load()
		r.mu.Unlock()
		if !ok {
			p.refused.Add(1)
			return
		}
	} else {
		var err error
		h, err = e.acquire(ctx, p.refs(now), w)
		var nr *noRoom
		if errors.As(err, &nr) {
			if p.refused.Add(1) == 1 {
				p.t.Logf("first refusal: %s room %d, W %d", nr.key, nr.room, w)
			}
			return
		}
		if err != nil {
			p.t.Error(err)
			return
		}
	}
	p.admits.Add(1)
	time.Sleep(time.Duration(rng.IntN(3000)) * time.Microsecond) // vLLM at work
	actual := rng.Int64N(w + 1)
	if rng.IntN(4) == 0 {
		actual = w // the worst case happens
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead || r.inc != inc {
		return // the process died with the request in flight: lost
	}
	rec := store.Record{ID: fmt.Sprintf("p%d", p.ids.Add(1)), Time: p.clk.now(), AdmittedAt: now, Org: "o", Team: "t", KeyID: "k", Model: "m",
		Cost: float64(actual) / nanoPerUnit, CostNano: actual, Status: 200}
	if h != nil {
		h.settle(actual)
		rec.HoldReplica, rec.HoldIncarnation, rec.HoldNano = r.name, inc, w
		for _, ref := range h.refs {
			rec.Holds = append(rec.Holds, store.HoldRef{Key: ref.key, Start: ref.start})
		}
	} else {
		r.local[start] += actual
	}
	r.spool = append(r.spool, rec)
}

// truth is the cost of the stored records admitted in each period.
func (p *propRun) truth(ctx context.Context) map[time.Time]int64 {
	out := map[time.Time]int64{}
	rows, err := p.st.UsageByAdmission(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, r := range rows {
		out[store.Day.Start(r.At)] += r.CostNano
	}
	return out
}

// setLimit records a limit in force during the current period: a past
// period is held to the largest limit it had (a lowering in the next
// period does not apply to it).
func (p *propRun) setLimit(v int64) {
	p.limit.Store(v)
	p.mu.Lock()
	defer p.mu.Unlock()
	s := store.Day.Start(p.clk.now()).UnixNano()
	p.maxLim[s] = max(p.maxLim[s], v)
}

func (p *propRun) check(ctx context.Context, where string) {
	now := p.clk.now()
	p.setLimit(p.limit.Load()) // the limit carries into a new period
	for i, start := range []time.Time{store.Day.Start(now), store.Day.Start(now).AddDate(0, 0, -1)} {
		for _, k := range p.keys {
			st, err := p.st.Ledger(ctx, k, start)
			if err != nil {
				p.t.Error(err)
				return
			}
			lim := p.limit.Load()
			if i == 1 {
				p.mu.Lock()
				lim = p.maxLim[start.UnixNano()]
				p.mu.Unlock()
				if lim == 0 {
					lim = propLimit
				}
			}
			if k.Kind == store.ScopeOrg {
				lim = 1000 * nanoPerUnit
			}
			if st.Spent > lim {
				msg := fmt.Sprintf("%s: %s at %s: spent %d > limit %d", where, k, start.Format(time.RFC3339), st.Spent, lim)
				p.violation.CompareAndSwap(nil, &msg)
			}
		}
	}
}

func runProperty(t *testing.T, control bool, seed uint64) (exceeded bool, admits int64) {
	direct, _ := freshDB(t, "")
	ctx := context.Background()
	st, err := store.OpenPostgres(ctx, direct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clk := &propClock{}
	clk.ns.Store(time.Date(2026, 10, 27, 23, 59, 50, 0, time.UTC).UnixNano())
	// The team budget is the one that fills; the org budget, far larger,
	// makes every hold a two-key, all-or-nothing growth.
	p := &propRun{t: t, st: st, clk: clk, control: control,
		keys: []store.BudgetKey{{Kind: store.ScopeTeam, Name: "t"}, {Kind: store.ScopeOrg, Name: "o"}}, maxLim: map[int64]int64{}}
	p.setLimit(propLimit)
	if err := st.SetBudgetChecked(ctx, store.Budget{BudgetKey: p.keys[0], Period: store.Day, MaxCost: 10}, clk.now(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBudgetChecked(ctx, store.Budget{BudgetKey: p.keys[1], Period: store.Day, MaxCost: 1000}, clk.now(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	reps := make([]*propReplica, 3)
	for i := range reps {
		reps[i] = &propReplica{name: fmt.Sprintf("r%d", i), old: map[string][]store.Record{}}
		p.restart(reps[i])
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Workers: four per replica.
	for i, r := range reps {
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, uint64(i*16+j)))
				for {
					select {
					case <-stop:
						return
					default:
					}
					p.request(ctx, r, rng)
				}
			}()
		}
	}
	// Each replica's accounting loop, escrow tick and (control) refresh.
	for _, r := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(10 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
				}
				p.flush(ctx, r)
				p.replay(ctx, r)
				r.mu.Lock()
				e := r.e
				r.mu.Unlock()
				e.tick(ctx, false)
				if control {
					rows, err := st.UsageByAdmission(ctx)
					if err == nil {
						seen := map[int64]int64{}
						for _, row := range rows {
							seen[store.Day.Start(row.At).UnixNano()] += row.CostNano
						}
						r.mu.Lock()
						r.seen, r.local = seen, map[int64]int64{}
						r.mu.Unlock()
					}
				}
			}
		}()
	}
	// The clock (one simulated second per 10 ms: midnight after 100 ms),
	// the killer, the administrator and the sampler.
	// A fixed number of ticks, however slow the machine: 150 simulated
	// seconds across midnight, 30 kills, 21 limit changes.
	clockDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(clockDone)
		rng := rand.New(rand.NewPCG(seed, 999))
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for n := 0; n < 150; n++ {
			<-tk.C
			clk.ns.Add(int64(time.Second))
			if n%5 == 2 { // kill a replica; it comes back at once
				r := reps[rng.IntN(len(reps))]
				r.mu.Lock()
				r.dead = true
				r.old[r.inc] = r.spool // spooled: durable; in flight: lost
				r.spool = nil
				r.mu.Unlock()
				p.kills.Add(1)
				p.restart(r)
			}
			if n%7 == 3 && !control { // lower (refused when below what is committed) or raise
				max := []float64{6, 8, 10, 12}[rng.IntN(4)]
				if v := int64(max) * nanoPerUnit; v > p.limit.Load() {
					p.setLimit(v) // a raise: the sampler may see its spend at once
				}
				err := st.SetBudgetChecked(ctx, store.Budget{BudgetKey: p.keys[0], Period: store.Day, MaxCost: max}, clk.now(), time.Minute, false)
				switch {
				case err == nil:
					p.setLimit(int64(max) * nanoPerUnit)
				case !errors.Is(err, store.ErrBelowCommitted):
					t.Error(err)
				}
			}
			if !control {
				p.check(ctx, fmt.Sprintf("sample %d", n))
			}
		}
	}()
	<-clockDone
	close(stop)
	wg.Wait()
	// The end: every replica's spool stored, every dead one replayed.
	for _, r := range reps {
		p.flush(ctx, r)
		r.mu.Lock()
		r.old[r.inc+"-final"] = nil
		r.mu.Unlock()
		p.replay(ctx, r)
	}
	if !control {
		p.check(ctx, "end")
		if v := p.violation.Load(); v != nil {
			t.Fatal(*v)
		}
		// The ledger agrees with the records.
		for start, cost := range p.truth(ctx) {
			led, _ := st.Ledger(ctx, p.keys[0], start)
			if led.Spent != cost {
				t.Fatalf("period %s: ledger %d, records %d", start, led.Spent, cost)
			}
		}
	}
	if p.kills.Load() < 30 || p.admits.Load() == 0 || p.refused.Load() == 0 {
		t.Fatalf("the run exercised nothing: %d kills, %d admitted, %d refused", p.kills.Load(), p.admits.Load(), p.refused.Load())
	}
	// Exceeded: a period's cost above the largest limit it ever had (the
	// control keeps 10; the run raises up to 12).
	for _, cost := range p.truth(ctx) {
		if control && cost > propLimit || cost > propLimit*12/10 {
			exceeded = true
		}
	}
	t.Logf("control=%v: %d admitted, %d refused, %d kills, truth %v", control, p.admits.Load(), p.refused.Load(), p.kills.Load(), p.truth(ctx))
	return exceeded, p.admits.Load()
}

func TestPropertyNeverExceededPG(t *testing.T) {
	pgAdminDSN(t)
	for seed := uint64(1); seed <= 3; seed++ {
		if exceeded, _ := runProperty(t, false, seed); exceeded {
			t.Fatalf("seed %d: a budget was exceeded", seed)
		}
	}
	// The planted control: the old admission on the same workload.
	if exceeded, _ := runProperty(t, true, 1); !exceeded {
		t.Fatal("control: the pre-#87 check did not exceed the limit on this workload: the test cannot see an overshoot")
	}
}
