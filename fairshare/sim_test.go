package fairshare

import (
	"container/heap"
	"math"
	"testing"
	"time"
)

// The simulator models vLLM's v1 priority scheduler closely enough to test
// the priority mapping (vllm/v1/core/sched/scheduler.py,
// request_queue.py):
//
//   - the waiting queue is a min-heap on (priority, arrival);
//   - every step each running request decodes one token and needs one more
//     KV block; when none is free, the running request with the largest
//     (priority, arrival) is preempted, frees its blocks and goes back to
//     the waiting queue (it keeps its priority and arrival, and restarts);
//   - waiting requests are admitted only while blocks are free; a waiting
//     request never preempts a running one.
//
// Clients are closed loops: each one resubmits as soon as its request
// finishes. The gateway side is the Pool under test: Acquire when a client
// submits, Release when the engine finishes the request.

type simReq struct {
	team     string
	grant    *Grant
	arrival  int
	prompt   int
	length   int
	done     int // tokens decoded
	blocks   int
	running  bool
	finished bool
}

type waitHeap []*simReq

func (h waitHeap) Len() int { return len(h) }
func (h waitHeap) Less(i, j int) bool {
	if h[i].grant.Priority != h[j].grant.Priority {
		return h[i].grant.Priority < h[j].grant.Priority
	}
	return h[i].arrival < h[j].arrival
}
func (h waitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *waitHeap) Push(x any)   { *h = append(*h, x.(*simReq)) }
func (h *waitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type engine struct {
	kv          int
	free        int
	waiting     waitHeap
	running     []*simReq
	seq         int
	preempted   []*simReq
	tokens      map[string]int // decoded tokens per team, after warm-up
	runningSum  map[string]int // Σ running requests per team per step
	usedSum     int
	steps       int
	measureFrom int
}

func newEngine(kv int) *engine {
	return &engine{kv: kv, free: kv, tokens: map[string]int{}, runningSum: map[string]int{}}
}

func (e *engine) submit(r *simReq) {
	e.seq++
	r.arrival = e.seq
	heap.Push(&e.waiting, r)
}

// worse reports whether a should be preempted before b.
func worse(a, b *simReq) bool {
	if a.grant.Priority != b.grant.Priority {
		return a.grant.Priority > b.grant.Priority
	}
	return a.arrival > b.arrival
}

func (e *engine) preempt(v *simReq) {
	for i, r := range e.running {
		if r == v {
			e.running = append(e.running[:i], e.running[i+1:]...)
			break
		}
	}
	e.free += v.blocks
	v.blocks, v.done, v.running = 0, 0, false
	e.preempted = append(e.preempted, v)
	heap.Push(&e.waiting, v)
}

// step advances the engine by one decode step and returns the requests
// that finished.
func (e *engine) step() []*simReq {
	var finished []*simReq
	// Decode: every running request needs one block.
	for _, r := range append([]*simReq(nil), e.running...) {
		if !r.running {
			continue // preempted earlier in this step
		}
		for e.free == 0 {
			victim := e.running[0]
			for _, c := range e.running[1:] {
				if worse(c, victim) {
					victim = c
				}
			}
			e.preempt(victim)
			if victim == r {
				break
			}
		}
		if !r.running {
			continue
		}
		e.free--
		r.blocks++
		r.done++
		if e.steps >= e.measureFrom {
			e.tokens[r.team]++
		}
	}
	// Finish.
	kept := e.running[:0]
	for _, r := range e.running {
		if r.done >= r.length {
			e.free += r.blocks
			r.blocks, r.running, r.finished = 0, false, true
			finished = append(finished, r)
			continue
		}
		kept = append(kept, r)
	}
	e.running = kept
	// Admit, in heap order, while the prompt fits.
	for e.waiting.Len() > 0 {
		r := e.waiting[0]
		if e.free < r.prompt {
			break
		}
		heap.Pop(&e.waiting)
		e.free -= r.prompt
		r.blocks = r.prompt
		r.running = true
		e.running = append(e.running, r)
	}
	if e.steps >= e.measureFrom {
		for _, r := range e.running {
			e.runningSum[r.team]++
		}
		e.usedSum += e.kv - e.free
	}
	e.steps++
	return finished
}

type scenario struct {
	clients map[string]int
	start   map[string]int // step at which a team's clients start
}

type simResult struct {
	e        *engine
	pool     *Pool
	borrowed int
	total    int
}

// Engine and workload sizes: prompt 10 blocks, 40 decoded tokens, so a
// request peaks at 50 blocks; C = 16 requests fit in 800 of the 1000
// blocks, which makes C sustainable: within-share requests alone can never
// exhaust the KV cache.
const (
	simKV       = 1000
	simCapacity = 16
	simPrompt   = 10
	simLength   = 40
)

func simulate(t *testing.T, sc scenario, steps, warmup int) simResult {
	return simulateC(t, sc, steps, warmup, simCapacity, simKV, nil)
}

// simulateC runs a scenario on a pool of capacity c and an engine of kv
// blocks, calling observe after every step when it is not nil.
func simulateC(t *testing.T, sc scenario, steps, warmup, c, kv int, observe func(*engine)) simResult {
	t.Helper()
	clock := time.Unix(0, 0)
	now := func() time.Time { return clock }
	pool := NewPool("b300", c, map[string]float64{"etabA": 0.8125, "etabB": 0.1875}, 2000*time.Second, now)
	e := newEngine(kv)
	e.measureFrom = warmup
	res := simResult{e: e, pool: pool}
	submit := func(team string) {
		g := pool.Acquire(team, Interactive, simPrompt+simLength)
		if e.steps >= warmup {
			res.total++
			if g.Borrowing {
				res.borrowed++
			}
		}
		e.submit(&simReq{team: team, grant: g, prompt: simPrompt, length: simLength})
	}
	for s := 0; s < steps; s++ {
		for team, n := range sc.clients {
			if sc.start[team] == s {
				for i := 0; i < n; i++ {
					submit(team)
				}
			}
		}
		for _, r := range e.step() {
			r.grant.Release(float64(simPrompt + simLength))
			submit(r.team)
		}
		if observe != nil {
			observe(e)
		}
		clock = clock.Add(time.Second)
	}
	return res
}

func fractions(m map[string]int) map[string]float64 {
	total := 0
	for _, v := range m {
		total += v
	}
	out := map[string]float64{}
	for k, v := range m {
		out[k] = float64(v) / float64(total)
	}
	return out
}

func TestAloneTeamGetsEverything(t *testing.T) {
	for _, team := range []string{"etabA", "etabB"} {
		t.Run(team, func(t *testing.T) {
			r := simulate(t, scenario{clients: map[string]int{team: 64}, start: map[string]int{team: 0}}, 4000, 500)
			util := float64(r.e.usedSum) / float64(simKV*(r.e.steps-r.e.measureFrom))
			if util < 0.9 {
				t.Fatalf("%s alone uses %.2f of the KV cache, want >= 0.90", team, util)
			}
			if f := fractions(r.e.tokens)[team]; f != 1 {
				t.Fatalf("%s token fraction %v", team, f)
			}
			// It got far more than its reservation.
			avgRunning := float64(r.e.runningSum[team]) / float64(r.e.steps-r.e.measureFrom)
			reserved := r.pool.Snapshot()[0].Reserved
			if team == "etabB" {
				reserved = r.pool.Snapshot()[1].Reserved
			}
			if avgRunning < 1.5*float64(reserved) {
				t.Fatalf("%s runs %.1f requests on average, reservation %d: borrowing did not happen", team, avgRunning, reserved)
			}
			t.Logf("%s alone: KV utilisation %.3f, mean running %.1f (reservation %d), preemptions %d",
				team, util, avgRunning, reserved, len(r.e.preempted))
		})
	}
}

func TestSaturationConvergesToShares(t *testing.T) {
	// A floods the pool first; B arrives later with as many clients.
	r := simulate(t, scenario{
		clients: map[string]int{"etabA": 64, "etabB": 64},
		start:   map[string]int{"etabA": 0, "etabB": 1000},
	}, 20000, 3000)
	f := fractions(r.e.runningSum)
	t.Logf("running fraction A=%.4f B=%.4f, borrowed requests %d/%d, preemptions %d",
		f["etabA"], f["etabB"], r.borrowed, r.total, len(r.e.preempted))
	if math.Abs(f["etabA"]-0.8125) > 0.03 || math.Abs(f["etabB"]-0.1875) > 0.03 {
		t.Fatalf("shares did not converge: A=%.4f B=%.4f", f["etabA"], f["etabB"])
	}
	tf := fractions(r.e.tokens)
	if math.Abs(tf["etabB"]-0.1875) > 0.03 {
		t.Fatalf("token share of B %.4f", tf["etabB"])
	}
}

func TestBorrowerIsPreempted(t *testing.T) {
	r := simulate(t, scenario{
		clients: map[string]int{"etabA": 64, "etabB": 64},
		start:   map[string]int{"etabA": 0, "etabB": 1000},
	}, 8000, 0)
	if len(r.e.preempted) == 0 {
		t.Fatal("no preemption happened: the scenario does not exercise the property")
	}
	for _, v := range r.e.preempted {
		if !v.grant.Borrowing {
			t.Fatalf("a within-share request of %s (priority %d) was preempted", v.team, v.grant.Priority)
		}
	}
	t.Logf("%d preemptions, all of borrowers", len(r.e.preempted))
}

// The control for TestBorrowerIsPreempted: with every request sent at the
// same priority (what a gateway that forwards no priority does), vLLM
// preempts by arrival only, and within-reservation work of B is lost.
func TestControlWithoutPrioritiesPreemptsReservedWork(t *testing.T) {
	e := newEngine(simKV)
	e.measureFrom = 3000
	submit := func(team string) {
		e.submit(&simReq{team: team, grant: &Grant{Priority: 0}, prompt: simPrompt, length: simLength})
	}
	for i := 0; i < 64; i++ {
		submit("etabA")
	}
	for s := 0; s < 20000; s++ {
		if s == 1000 {
			for i := 0; i < 64; i++ {
				submit("etabB")
			}
		}
		for _, r := range e.step() {
			submit(r.team)
		}
	}
	f := fractions(e.runningSum)
	t.Logf("control, every request at priority 0: running fraction B=%.4f", f["etabB"])
	if f["etabB"] < 0.3 {
		t.Fatalf("without priorities B should get about its client fraction, got %.3f", f["etabB"])
	}
}

func TestPriorityLayout(t *testing.T) {
	clock := time.Unix(0, 0)
	p := NewPool("p", 4, map[string]float64{"a": 0.5, "b": 0.5}, time.Hour, func() time.Time { return clock })
	g1 := p.Acquire("a", Interactive, 10)
	g2 := p.Acquire("a", Batch, 10)
	if g1.Priority != 0 || g2.Priority != 1 || g1.Borrowing || g2.Borrowing {
		t.Fatalf("within share: %d %d", g1.Priority, g2.Priority)
	}
	g0 := p.Acquire("a", Interactive, 0)
	if !g0.Borrowing || g0.Priority != BorrowBase {
		t.Fatalf("no usage yet: every team ties at rank 0, got %d", g0.Priority)
	}
	g0.Release(5)
	clock = clock.Add(time.Minute)
	g3 := p.Acquire("a", Interactive, 10)
	if !g3.Borrowing {
		t.Fatal("third request of a should borrow")
	}
	// a has usage, b none: b has LF=+Inf (rank 0), a rank 1.
	if g3.Priority != BorrowBase+RankStep+0 {
		t.Fatalf("borrow priority %d", g3.Priority)
	}
	h1 := p.Acquire("b", Batch, 1)
	h2 := p.Acquire("b", Batch, 1)
	h3 := p.Acquire("b", Batch, 1)
	if h1.Priority != 1 || h2.Priority != 1 || h3.Priority != BorrowBase+1 {
		t.Fatalf("b priorities %d %d %d", h1.Priority, h2.Priority, h3.Priority)
	}
	unknown := p.Acquire("stranger", Interactive, 1)
	if !unknown.Borrowing {
		t.Fatal("unknown team must borrow")
	}
	st := p.Snapshot()
	if st[0].Team != "a" || st[0].InFlight != 3 || st[0].Borrowing != 1 || st[0].InFlightTokens != 30 {
		t.Fatalf("snapshot %+v", st[0])
	}
	g3.Release(1)
	g3.Release(1000) // idempotent
	if p.Snapshot()[0].InFlight != 2 || p.Snapshot()[0].Borrowing != 0 {
		t.Fatal("release")
	}
	if g1.Team() != "a" || p.Name() != "p" || p.Capacity() != 4 {
		t.Fatal("accessors")
	}
}

func TestDecayHalvesUsage(t *testing.T) {
	clock := time.Unix(0, 0)
	p := NewPool("p", 1, map[string]float64{"a": 1}, time.Hour, func() time.Time { return clock })
	g := p.Acquire("a", Interactive, 0)
	clock = clock.Add(10 * time.Second)
	g.Release(10)
	u0 := p.Snapshot()[0].Usage
	if u0 != 10 {
		t.Fatalf("usage after a request that consumed 10: %v", u0)
	}
	clock = clock.Add(time.Hour)
	if u1 := p.Snapshot()[0].Usage; math.Abs(u1-u0/2) > 1e-9 {
		t.Fatalf("after one half-life: %v, want %v", u1, u0/2)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		g := p.Acquire("a", Interactive, 0)
		g.Release(bad)
	}
	if u := p.Snapshot()[0].Usage; math.Abs(u-u0/2) > 1e-9 {
		t.Fatalf("invalid consumption was charged: %v", u)
	}
	// No decay.
	q := NewPool("q", 1, map[string]float64{"a": 1}, 0, func() time.Time { return clock })
	g = q.Acquire("a", Interactive, 0)
	clock = clock.Add(5 * time.Second)
	g.Release(5)
	clock = clock.Add(1000 * time.Hour)
	if u := q.Snapshot()[0].Usage; u != 5 {
		t.Fatalf("undecayed usage %v", u)
	}
	if NewPool("r", 1, nil, 0, nil).now == nil {
		t.Fatal("default clock")
	}
}

func TestRankTiesAndZeroShare(t *testing.T) {
	clock := time.Unix(0, 0)
	p := NewPool("p", 2, map[string]float64{"a": 0.5, "b": 0.5, "z": 0}, time.Hour, func() time.Time { return clock })
	ga := p.Acquire("a", Interactive, 0)
	gb := p.Acquire("b", Interactive, 0)
	clock = clock.Add(time.Minute)
	ga.Release(3)
	gb.Release(3)
	st := p.Snapshot()
	if st[0].Rank != st[1].Rank {
		t.Fatalf("equal LF must share a rank: %+v", st)
	}
	if st[2].LevelFairshare != 0 || st[2].Rank != 1 {
		t.Fatalf("zero share, zero usage: %+v", st[2])
	}
}
