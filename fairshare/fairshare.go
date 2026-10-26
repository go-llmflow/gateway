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
	"sync/atomic"
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
	reserved       int // within-share slots (Apportion)
	inflight       int
	borrowing      int
	inflightTokens int64
	usage          float64 // decayed concurrency·seconds
	lf             float64
	rank           int
	// phantoms are requests that may be in flight but that no live grant
	// stands for (a replica that died, or the in-flight count a previous
	// owner persisted, see package ha): each is the time it stops counting.
	// They count as in flight within the reservation, the conservative
	// side: they never let a new request in "within share" that the
	// missing ones might have taken.
	phantoms []time.Time
}

// busy is the team's in flight, phantoms included.
func (t *team) busy() int { return t.inflight + len(t.phantoms) }

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
	reserved := Apportion(capacity, names, shares)
	for _, n := range names {
		p.addTeam(n, shares[n], reserved[n])
	}
	return p
}

// Apportion divides the C slots of a pool into integer reservations with
// the largest-remainder (Hamilton) method: every team first gets
// floor(share × C), then the slots left up to floor(Σ share × C) go one
// each to the largest fractional parts (ties broken by name order). The
// sum never exceeds C, and equals C when the shares sum to 1. Rounding
// each quota up instead (the first version) could promise more
// within-share slots than C: with C = 10 and 0.8125 / 0.1875 it gave
// 9 + 2 = 11, so one of B's guaranteed requests could wait in vLLM's queue
// at equal priority behind A's ninth.
func Apportion(capacity int, names []string, shares map[string]float64) map[string]int {
	const eps = 1e-9
	out := make(map[string]int, len(names))
	type rem struct {
		name string
		frac float64
	}
	var rems []rem
	total, given := 0.0, 0
	for _, n := range names {
		s := shares[n]
		if !(s > 0) {
			out[n] = 0
			continue
		}
		q := s * float64(capacity)
		total += q
		f := math.Floor(q + eps)
		out[n] = int(f)
		given += int(f)
		rems = append(rems, rem{n, q - f})
	}
	seats := int(math.Floor(total + eps))
	if seats > capacity {
		seats = capacity
	}
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].frac > rems[j].frac })
	for i := 0; given < seats && i < len(rems); i++ {
		out[rems[i].name]++
		given++
	}
	return out
}

func (p *Pool) addTeam(name string, share float64, reserved int) *team {
	t := &team{name: name, share: share, reserved: reserved}
	p.teams[name] = t
	p.order = append(p.order, t)
	return t
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.name }

// Capacity returns C.
func (p *Pool) Capacity() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capacity
}

// SetCapacity changes C (an elastic pool whose instances come and go, #75)
// and recomputes the within-share reservations with Apportion. Requests in
// flight keep their within-share or borrowing status; new requests are
// judged against the new reservations (and, for TryAcquire, the new C).
// It returns the previous C.
func (p *Pool) SetCapacity(capacity int) int {
	if capacity < 0 {
		capacity = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.capacity
	if capacity == old {
		return old
	}
	p.capacity = capacity
	names := make([]string, 0, len(p.order))
	shares := make(map[string]float64, len(p.order))
	for _, t := range p.order {
		names = append(names, t.name)
		shares[t.name] = t.share
	}
	sort.Strings(names)
	reserved := Apportion(capacity, names, shares)
	for _, t := range p.order {
		t.reserved = reserved[t.name]
	}
	return old
}

// Reserved returns the within-share slots of each organisation.
func (p *Pool) Reserved() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.order))
	for _, t := range p.order {
		out[t.name] = t.reserved
	}
	return out
}

// Grant is the decision for one request. Release must be called exactly
// once when the request has finished (successfully or not).
type Grant struct {
	pool      *Pool
	team      *team
	teamName  string
	tokens    int64
	Priority  int
	Borrowing bool
	// Saturated: when this request arrived, the pool already had C or more
	// requests in flight (the gateway's own view of saturation, #58).
	Saturated bool
	released  bool
	// remote, for a grant decided by another process (NewRemoteGrant):
	// called once by Release instead of updating a local pool.
	remote     func(consumed float64)
	remoteDone atomic.Bool
}

// NewRemoteGrant returns a grant decided elsewhere (by the replica that
// owns the pool, package ha). Release calls release once, with the
// consumption; the grant is not counted in any local Pool.
func NewRemoteGrant(teamName string, priority int, borrowing, saturated bool, release func(consumed float64)) *Grant {
	return &Grant{teamName: teamName, Priority: priority, Borrowing: borrowing, Saturated: saturated, remote: release}
}

// inflightLocked is the pool's requests in flight, phantoms included.
func (p *Pool) inflightLocked() int {
	n := 0
	for _, t := range p.order {
		n += t.busy()
	}
	return n
}

// expireLocked drops the phantoms whose time has come.
func (p *Pool) expireLocked(now time.Time) {
	for _, t := range p.order {
		k := 0
		for _, until := range t.phantoms {
			if now.Before(until) {
				t.phantoms[k] = until
				k++
			}
		}
		t.phantoms = t.phantoms[:k]
	}
}

func (p *Pool) teamLocked(name string) *team {
	t := p.teams[name]
	if t == nil {
		t = p.addTeam(name, 0, 0)
	}
	return t
}

// AddPhantoms counts n requests of a team as in flight until the given
// time, without grants: requests that may still run but that nobody can
// release (their replica died, or a previous owner's in-flight count).
func (p *Pool) AddPhantoms(teamName string, n int, until time.Time) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.teamLocked(teamName)
	for range n {
		t.phantoms = append(t.phantoms, until)
	}
}

// DropPhantom removes the team's phantom that would last longest and
// reports whether there was one: a request it stood for turned out to be
// a live grant after all.
func (p *Pool) DropPhantom(teamName string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.teams[teamName]
	if t == nil || len(t.phantoms) == 0 {
		return false
	}
	j := 0
	for i, u := range t.phantoms {
		if u.After(t.phantoms[j]) {
			j = i
		}
	}
	t.phantoms = append(t.phantoms[:j], t.phantoms[j+1:]...)
	return true
}

// Adopt counts a request decided before (by a previous owner of the pool,
// or admitted without one) as in flight with the standing it was given,
// and returns its grant. Nothing is decided: the priority is the one the
// request already runs with.
func (p *Pool) Adopt(teamName string, priority int, borrowing bool, estTokens int64) *Grant {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.teamLocked(teamName)
	t.inflight++
	t.inflightTokens += estTokens
	if borrowing {
		t.borrowing++
	}
	return &Grant{pool: p, team: t, teamName: teamName, tokens: estTokens, Priority: priority, Borrowing: borrowing}
}

// Charge adds consumption to a team's usage without a grant (the release
// of a request this pool never counted, package ha).
func (p *Pool) Charge(teamName string, consumed float64) {
	if !(consumed > 0) || math.IsInf(consumed, 1) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	p.teamLocked(teamName).usage += consumed
}

// Usage returns every team's decayed usage and in-flight count (phantoms
// included), decayed to now: what an owner persists for the next one.
func (p *Pool) Usage() (usage map[string]float64, inflight map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	p.expireLocked(p.now())
	usage, inflight = make(map[string]float64, len(p.order)), make(map[string]int, len(p.order))
	for _, t := range p.order {
		usage[t.name] = t.usage
		inflight[t.name] = t.busy()
	}
	return usage, inflight
}

// SetUsage sets a team's decayed usage (restored from a previous owner).
// Negative, NaN and infinite values count as 0.
func (p *Pool) SetUsage(teamName string, usage float64) {
	if !(usage > 0) || math.IsInf(usage, 1) {
		usage = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	p.teamLocked(teamName).usage = usage
}

// Lambda is the decay rate ln2/half-life in 1/s (0: no decay).
func (p *Pool) Lambda() float64 { return p.lambda }

// TryAcquire is Acquire only when the pool has room (fewer than C requests
// in flight), decided and recorded atomically: a fallback that moves a
// request here never pushes this pool past C, so two saturated pools
// cannot hand requests to each other (#58).
func (p *Pool) TryAcquire(teamName string, class Class, estTokens int64) (*Grant, bool) {
	return p.acquire(teamName, class, estTokens, true)
}

// Acquire records a new in-flight request of a team and returns its
// priority. A team unknown to the pool has a share of 0: it can only
// borrow.
func (p *Pool) Acquire(teamName string, class Class, estTokens int64) *Grant {
	g, _ := p.acquire(teamName, class, estTokens, false)
	return g
}

func (p *Pool) acquire(teamName string, class Class, estTokens int64, needRoom bool) (*Grant, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked(p.now())
	busy := p.inflightLocked()
	if needRoom && busy >= p.capacity {
		return nil, false
	}
	p.advance()
	t := p.teamLocked(teamName)
	g := &Grant{pool: p, team: t, teamName: teamName, tokens: estTokens, Saturated: busy >= p.capacity}
	// The test is on the number of within-share requests in flight, not
	// on the total: a team that keeps more requests in flight than its
	// reservation (any closed-loop client pool larger than share × C)
	// would otherwise never see its in-flight count drop below the
	// reservation again, and every request after the first ones would be
	// sent as a borrower. TestSaturationConvergesToShares fails with the
	// total-count rule (B obtains 4 % instead of 18.75 %).
	if t.inflight-t.borrowing+len(t.phantoms) < t.reserved {
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
	return g, true
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
	if g.remote != nil {
		if g.remoteDone.CompareAndSwap(false, true) {
			g.remote(consumed)
		}
		return
	}
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
func (g *Grant) Team() string { return g.teamName }

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
	Reserved       int // within-share slots, see Apportion
	InFlight       int
	Borrowing      int
	InFlightTokens int64
	Usage          float64
	LevelFairshare float64
	Rank           int
	// Phantoms are requests counted in flight without a grant (package ha:
	// a failover's safety margin, a silent replica's requests); InFlight
	// does not include them.
	Phantoms int
}

// Snapshot returns the current state of every team, in a stable order.
func (p *Pool) Snapshot() []TeamStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advance()
	p.expireLocked(p.now())
	p.rank()
	out := make([]TeamStat, 0, len(p.order))
	for _, t := range p.order {
		out = append(out, TeamStat{
			Team: t.name, Share: t.share, Reserved: t.reserved,
			InFlight: t.inflight, Borrowing: t.borrowing, InFlightTokens: t.inflightTokens,
			Usage: t.usage, LevelFairshare: t.lf, Rank: t.rank, Phantoms: len(t.phantoms),
		})
	}
	return out
}
