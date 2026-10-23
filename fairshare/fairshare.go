// Package fairshare decides the vLLM priority of every request so that a
// GPU pool shared by several teams behaves like a reservation with
// borrowing:
//
//   - each team owns share × C of the pool's sustainable concurrency C;
//   - a request that keeps its team under that reservation is sent
//     "within share" (priority 0 for interactive, 1 for batch);
//   - a request above it "borrows" idle capacity (priority 10 / 11 and
//     more), so it runs when nobody else needs the GPUs, and it is the one
//     vLLM preempts when they do.
//
// The mapping relies on two properties of vLLM's priority scheduler
// (vllm/v1/core/sched/scheduler.py and request_queue.py, opened
// 2026-10-23): the waiting queue is a min-heap on (priority,
// arrival_time), and when a running request runs out of KV blocks vLLM
// preempts the running request with the largest (priority, arrival_time).
// A waiting request never preempts a running one.
//
// Among borrowers, the order follows Slurm's Fair Tree level fairshare
// LF = S / U (https://slurm.schedmd.com/fair_tree.html): S is the team's
// share normalised over the teams of the pool, U its decayed usage
// normalised the same way. Usage is the consumption (cost units) of the
// team's finished requests, decayed with a half-life as Slurm's
// PriorityDecayHalfLife does
// (priority_multifactor.c applies usage_raw *= decay_factor^Δt with
// decay_factor = 1 - 0.693/half_life; this package uses the exact
// exponential exp(-ln2·Δt/half_life), of which Slurm's factor is the
// first-order expansion). The most over-served borrower gets the largest
// priority number, so it is admitted last and preempted first.
package fairshare

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Class is the service class of a request.
type Class int

const (
	// Interactive is latency sensitive.
	Interactive Class = 0
	// Batch is throughput oriented and yields to interactive work of the
	// same standing.
	Batch Class = 1
)

// Priority layout (lower runs earlier in vLLM).
const (
	// WithinShareBase is the priority of a request inside its team's
	// reservation, plus the class.
	WithinShareBase = 0
	// BorrowBase is the priority of the least over-served borrower, plus
	// the class.
	BorrowBase = 10
	// RankStep separates borrowers of successive fairshare ranks.
	RankStep = 10
	// MaxRank bounds the rank so priorities stay small.
	MaxRank = 9
)

// Pool tracks one capacity domain.
type Pool struct {
	mu       sync.Mutex
	name     string
	capacity int
	lambda   float64 // ln2 / half-life in 1/s; 0 disables decay
	now      func() time.Time
	last     time.Time
	teams    map[string]*team
	order    []*team
}

type team struct {
	name           string
	share          float64
	inflight       int
	borrowing      int
	inflightTokens int64
	usage          float64 // decayed concurrency·seconds
	lf             float64
	rank           int
}

// NewPool returns a pool of capacity C with the given team shares. A
// halfLife of 0 disables decay (usage accumulates forever). now may be nil
// for time.Now.
func NewPool(name string, capacity int, shares map[string]float64, halfLife time.Duration, now func() time.Time) *Pool {
	if now == nil {
		now = time.Now
	}
	p := &Pool{name: name, capacity: capacity, now: now, teams: map[string]*team{}}
	if halfLife > 0 {
		p.lambda = math.Ln2 / halfLife.Seconds()
	}
	p.last = now()
	names := make([]string, 0, len(shares))
	for n := range shares {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p.addTeam(n, shares[n])
	}
	return p
}

func (p *Pool) addTeam(name string, share float64) *team {
	t := &team{name: name, share: share}
	p.teams[name] = t
	p.order = append(p.order, t)
	return t
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.name }

// Capacity returns C.
func (p *Pool) Capacity() int { return p.capacity }

// Grant is the decision for one request. Release must be called exactly
// once when the request has finished (successfully or not).
type Grant struct {
	pool      *Pool
	team      *team
	tokens    int64
	Priority  int
	Borrowing bool
	released  bool
}

// Acquire records a new in-flight request of a team and returns its
// priority. A team unknown to the pool has a share of 0: it can only
// borrow.
func (p *Pool) Acquire(teamName string, class Class, estTokens int64) *Grant {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	t := p.teams[teamName]
	if t == nil {
		t = p.addTeam(teamName, 0)
	}
	g := &Grant{pool: p, team: t, tokens: estTokens}
	// The test is on the number of within-share requests in flight, not
	// on the total: a team that keeps more requests in flight than its
	// reservation (any closed-loop client pool larger than share × C)
	// would otherwise never see its in-flight count drop below the
	// reservation again, and every request after the first ones would be
	// sent as a borrower. TestSaturationConvergesToShares fails with the
	// total-count rule (B obtains 4 % instead of 18.75 %).
	if float64(t.inflight-t.borrowing) < t.share*float64(p.capacity) {
		g.Priority = WithinShareBase + int(class)
	} else {
		p.rank()
		r := t.rank
		if r > MaxRank {
			r = MaxRank
		}
		g.Priority = BorrowBase + RankStep*r + int(class)
		g.Borrowing = true
		t.borrowing++
	}
	t.inflight++
	t.inflightTokens += estTokens
	return g
}

// Release ends the request and charges the resources it actually
// consumed (cost units, e.g. GPU-hours; negative or NaN values count as
// 0) to the team's usage. It is idempotent: only the first call counts.
//
// Usage is charged from consumption, never from in-flight time: a request
// waiting in vLLM's queue is in flight for the gateway but consumes
// nothing, and charging it made a flooding team look as consumed as the
// team it was starving (TestSaturationConvergesToShares caught it).
func (g *Grant) Release(consumed float64) {
	p := g.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if g.released {
		return
	}
	g.released = true
	p.advance()
	g.team.inflight--
	g.team.inflightTokens -= g.tokens
	if g.Borrowing {
		g.team.borrowing--
	}
	if consumed > 0 && !math.IsInf(consumed, 1) {
		g.team.usage += consumed
	}
}

// Team returns the team name of the grant.
func (g *Grant) Team() string { return g.team.name }

// advance decays usage up to now: usage *= e^{-λΔt}.
func (p *Pool) advance() {
	now := p.now()
	dt := now.Sub(p.last).Seconds()
	if dt <= 0 {
		return
	}
	p.last = now
	if p.lambda == 0 {
		return
	}
	d := math.Exp(-p.lambda * dt)
	for _, t := range p.order {
		t.usage *= d
		if t.usage < 1e-300 {
			t.usage = 0
		}
	}
}

// rank computes LF = S/U for every team and a dense rank by decreasing LF
// (rank 0 = most under-served). Teams with equal LF share a rank, as
// sibling users do in Fair Tree.
func (p *Pool) rank() {
	var sumS, sumU float64
	for _, t := range p.order {
		sumS += t.share
		sumU += t.usage
	}
	for _, t := range p.order {
		s, u := 0.0, 0.0
		if sumS > 0 {
			s = t.share / sumS
		}
		if sumU > 0 {
			u = t.usage / sumU
		}
		switch {
		case u > 0:
			t.lf = s / u
		case s > 0:
			t.lf = math.Inf(1)
		default:
			t.lf = 0
		}
	}
	sorted := append([]*team(nil), p.order...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].lf > sorted[j].lf })
	r := 0
	for i, t := range sorted {
		if i > 0 && !sameLF(t.lf, sorted[i-1].lf) {
			r++
		}
		t.rank = r
	}
}

func sameLF(a, b float64) bool {
	if math.IsInf(a, 1) || math.IsInf(b, 1) {
		return a == b
	}
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

// TeamStat is a snapshot of one team in a pool.
type TeamStat struct {
	Team           string
	Share          float64
	Reserved       float64
	InFlight       int
	Borrowing      int
	InFlightTokens int64
	Usage          float64
	LevelFairshare float64
	Rank           int
}

// Snapshot returns the current state of every team, in a stable order.
func (p *Pool) Snapshot() []TeamStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	p.rank()
	out := make([]TeamStat, 0, len(p.order))
	for _, t := range p.order {
		out = append(out, TeamStat{
			Team: t.name, Share: t.share, Reserved: t.share * float64(p.capacity),
			InFlight: t.inflight, Borrowing: t.borrowing, InFlightTokens: t.inflightTokens,
			Usage: t.usage, LevelFairshare: t.lf, Rank: t.rank,
		})
	}
	return out
}
