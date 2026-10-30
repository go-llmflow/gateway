// Package lending turns a period's finished requests into the evidence for
// the second rule of a shared-GPU agreement (#97): *each partner's usage is
// guaranteed pro rata to its investment, and a partner may borrow what the
// others leave idle.* Per organisation it reports
//
//   - the guaranteed capacity: reserved slots × hours of the period, with C
//     as it was at each instant (a pool fed by Slurm jobs has a C that
//     changes; its history is integrated, not its configured value). The
//     reservation is fairshare.Apportion(C, shares), the very slots the
//     gateway's admission test uses;
//   - the GPU-hours used within the share and the GPU-hours borrowed (the
//     request's Borrowing flag, set at admission by fairshare.Pool);
//   - the GPU-hours lent: borrowing is served by the capacity other
//     organisations reserved and did not use at that moment. At every instant
//     the borrowing rate of an organisation is split between the lenders in
//     proportion to their idle reserved slots (reservation minus in-share
//     requests in flight). Borrowing that finds no idle reservation (the
//     lenders' slots were taken back, or C is over-subscribed) is booked as
//     Unattributed and counted, never dropped;
//   - the lending matrix (lender × borrower, GPU-hours) and the net balance
//     lent − borrowed from others, which sums to zero over the organisations;
//   - the admission wait of the requests, in share against borrowing: the time
//     between a request's arrival and the fair-share grant, the evidence that
//     the guarantee held. It is the gateway's queue, not vLLM's: the time a
//     request then spends in vLLM's own queue is not recorded.
//
// A request holds its slot from the grant to its end: [End − latency + wait,
// End]. Accounting identities, held by a property test against the real
// fairshare.Pool: for every organisation, the matrix column of a borrower
// plus its self-served part is what it borrowed; the lent total equals the
// borrowed total less the unattributed; the nets sum to 0.
//
// A request belongs to the period it finished in; GPU-hours are the gateway's
// cost unit.
package lending

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

// MaxRequests bounds one computation: the sweep keeps two events per request,
// about 200 bytes with the rows it is built from.
const MaxRequests = 500_000

// ErrTooMany is returned for more than MaxRequests requests.
var ErrTooMany = errors.New("lending: too many requests in the period; shorten it")

// Req is one finished request that consumed GPU-hours.
type Req struct {
	Org, Pool string
	End       time.Time
	// LatencyMS is arrival to end, WaitMS the admission wait inside it.
	LatencyMS, WaitMS float64
	Borrowing         bool
	GPUHours          float64
}

// CapPoint is a pool's C from At on.
type CapPoint struct {
	At time.Time
	C  int
}

// Pool is a capacity domain with the shares of the organisations. Timeline is
// the history of its C (sorted by time; the last point at or before an instant
// is the C then); without it, or before its first point, Capacity applies.
type Pool struct {
	Name     string
	Capacity int
	Timeline []CapPoint
	Shares   map[string]float64
}

// Latency holds percentiles in milliseconds.
type Latency struct {
	N             int
	P50, P95, P99 float64
}

// OrgStat is the balance of one organisation.
type OrgStat struct {
	Org string
	// GuaranteedHours is reserved slots × hours, over the pools.
	GuaranteedHours float64
	// UsedInShare and Borrowed are GPU-hours; Lent is the organisation's idle
	// reservation used by others.
	UsedInShare, Borrowed, Lent float64
	// BorrowedFromOthers is the part of Borrowed served by other
	// organisations' idle reservation; SelfServed the part that ran while the
	// organisation's own reservation was idle again (admitted as a borrower,
	// served by its own slot). Borrowed = BorrowedFromOthers + SelfServed +
	// the report's Unattributed share.
	BorrowedFromOthers, SelfServed float64
	// Net is Lent - BorrowedFromOthers: positive for a net lender. The nets
	// of all organisations sum to 0.
	Net float64
	// InShare and Borrow are the end-to-end latency of the requests inside
	// the reservation and of those above it; WaitInShare and WaitBorrow their
	// admission wait.
	InShare, Borrow         Latency
	WaitInShare, WaitBorrow Latency
}

// Result is the lending evidence of a period.
type Result struct {
	From, To time.Time
	Orgs     []OrgStat
	// Matrix[lender][borrower] in GPU-hours.
	Matrix map[string]map[string]float64
	// Unattributed is borrowed GPU-hours found no idle reservation to be
	// charged to.
	Unattributed float64
	Requests     int
}

type event struct {
	t     int64 // unix milliseconds
	org   int
	share int     // ±1 on the org's in-share concurrency
	rate  float64 // ± GPU-hours per millisecond of a borrowing request
	cap   int     // 1 + index into the pool's capacity segments, 0 for none
}

type capSeg struct {
	t        int64 // from this instant; math.MinInt64 for the first
	reserved []int
}

// reservations is Apportion over the organisations, as a slice by index.
func reservations(c int, names []string, shares map[string]float64, orgIdx map[string]int, n int) []int {
	res := fairshare.Apportion(c, names, shares)
	out := make([]int, n)
	for _, o := range names {
		out[orgIdx[o]] = res[o]
	}
	return out
}

// capacityAt is the C of a pool at instant t.
func (p Pool) capacityAt(t time.Time) int {
	c := p.Capacity
	for _, pt := range p.Timeline {
		if pt.At.After(t) {
			break
		}
		c = pt.C
	}
	return max(c, 0)
}

// Compute builds the report of [from, to) from the requests that finished in it.
func Compute(from, to time.Time, pools []Pool, reqs []Req) (*Result, error) {
	if len(reqs) > MaxRequests {
		return nil, ErrTooMany
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("lending: from must be before to")
	}
	orgIdx := map[string]int{}
	var orgs []string
	add := func(o string) {
		if _, ok := orgIdx[o]; !ok {
			orgIdx[o] = len(orgs)
			orgs = append(orgs, o)
		}
	}
	poolIdx := map[string]int{}
	for i, p := range pools {
		poolIdx[p.Name] = i
		for o := range p.Shares {
			add(o)
		}
	}
	for _, r := range reqs {
		add(r.Org)
	}
	sort.Strings(orgs) // stable output order
	for i, o := range orgs {
		orgIdx[o] = i
	}
	n := len(orgs)
	stat := make([]OrgStat, n)
	for i, o := range orgs {
		stat[i].Org = o
	}
	caps := make([][]capSeg, len(pools))
	for i, p := range pools {
		names := make([]string, 0, len(p.Shares))
		for o := range p.Shares {
			names = append(names, o)
		}
		sort.Strings(names)
		// Segments of constant C inside [from, to): the guarantee is their sum.
		bounds := []time.Time{from}
		for _, pt := range p.Timeline {
			if pt.At.After(from) && pt.At.Before(to) {
				bounds = append(bounds, pt.At)
			}
		}
		bounds = append(bounds, to)
		first := true
		for k := 0; k+1 < len(bounds); k++ {
			c := p.capacityAt(bounds[k])
			res := reservations(c, names, p.Shares, orgIdx, n)
			h := bounds[k+1].Sub(bounds[k]).Hours()
			for o := 0; o < n; o++ {
				stat[o].GuaranteedHours += float64(res[o]) * h
			}
			t := int64(math.MinInt64)
			if !first {
				t = bounds[k].UnixMilli()
			}
			first = false
			caps[i] = append(caps[i], capSeg{t: t, reserved: res})
		}
	}

	res := &Result{From: from, To: to, Matrix: map[string]map[string]float64{}, Requests: len(reqs)}
	evs := make([][]event, len(pools))
	inLat, bLat := make([][]float64, n), make([][]float64, n)
	inWait, bWait := make([][]float64, n), make([][]float64, n)
	for _, r := range reqs {
		oi := orgIdx[r.Org]
		pi, ok := poolIdx[r.Pool]
		cost := r.GPUHours
		if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			cost = 0
		}
		lat := r.LatencyMS
		if math.IsNaN(lat) || lat < 1 {
			lat = 1
		}
		wait := r.WaitMS
		if math.IsNaN(wait) || wait < 0 {
			wait = 0
		}
		wait = math.Min(wait, lat-1) // the slot is held at least 1 ms
		if r.Borrowing {
			stat[oi].Borrowed += cost
			bLat[oi] = append(bLat[oi], lat)
			bWait[oi] = append(bWait[oi], wait)
		} else {
			stat[oi].UsedInShare += cost
			inLat[oi] = append(inLat[oi], lat)
			inWait[oi] = append(inWait[oi], wait)
		}
		if !ok { // a pool the configuration no longer has: counted, not placed in time
			if r.Borrowing {
				res.Unattributed += cost
			}
			continue
		}
		end := r.End.UnixMilli()
		start := end - int64(math.Ceil(lat-wait))
		if r.Borrowing {
			rate := cost / float64(end-start)
			evs[pi] = append(evs[pi], event{t: start, org: oi, rate: rate}, event{t: end, org: oi, rate: -rate})
		} else {
			evs[pi] = append(evs[pi], event{t: start, org: oi, share: 1}, event{t: end, org: oi, share: -1})
		}
	}
	lent := make([][]float64, n) // lender → borrower
	for i := range lent {
		lent[i] = make([]float64, n)
	}
	for pi := range pools {
		for k := 1; k < len(caps[pi]); k++ {
			evs[pi] = append(evs[pi], event{t: caps[pi][k].t, cap: k + 1})
		}
		sweep(evs[pi], caps[pi], n, lent, &res.Unattributed)
	}
	for i := range stat {
		for j := range stat {
			if lent[i][j] <= 0 {
				continue
			}
			if i == j {
				// A request admitted as a borrower that ran while its own
				// organisation had idle reservation: it used its own slot.
				stat[i].SelfServed += lent[i][j]
				continue
			}
			stat[i].Lent += lent[i][j]
			stat[j].BorrowedFromOthers += lent[i][j]
			if res.Matrix[orgs[i]] == nil {
				res.Matrix[orgs[i]] = map[string]float64{}
			}
			res.Matrix[orgs[i]][orgs[j]] = lent[i][j]
		}
	}
	for i := range stat {
		stat[i].Net = stat[i].Lent - stat[i].BorrowedFromOthers
		stat[i].InShare, stat[i].Borrow = percentiles(inLat[i]), percentiles(bLat[i])
		stat[i].WaitInShare, stat[i].WaitBorrow = percentiles(inWait[i]), percentiles(bWait[i])
	}
	res.Orgs = stat
	return res, nil
}

// sweep integrates one pool's timeline: between two events the in-share
// concurrency, the borrowing rates and the reservations are constant, the idle
// reservation of each organisation is max(0, reserved - in share), and each
// borrower's rate is split between the lenders by their idle slots.
func sweep(evs []event, caps []capSeg, n int, lent [][]float64, unattributed *float64) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	inShare := make([]int, n)
	rate := make([]float64, n)
	idle := make([]float64, n)
	reserved := caps[0].reserved
	for i := 0; i < len(evs); {
		t := evs[i].t
		for ; i < len(evs) && evs[i].t == t; i++ {
			if evs[i].cap > 0 {
				reserved = caps[evs[i].cap-1].reserved
				continue
			}
			inShare[evs[i].org] += evs[i].share
			rate[evs[i].org] += evs[i].rate
		}
		if i >= len(evs) {
			break
		}
		dt := float64(evs[i].t - t)
		var sumIdle float64
		for o := 0; o < n; o++ {
			idle[o] = float64(max(reserved[o]-inShare[o], 0))
			sumIdle += idle[o]
		}
		for b := 0; b < n; b++ {
			if rate[b] <= 1e-18 {
				continue
			}
			gh := rate[b] * dt
			if sumIdle == 0 {
				*unattributed += gh
				continue
			}
			for o := 0; o < n; o++ {
				if idle[o] > 0 {
					lent[o][b] += gh * idle[o] / sumIdle
				}
			}
		}
	}
}

func percentiles(v []float64) Latency {
	if len(v) == 0 {
		return Latency{}
	}
	sort.Float64s(v)
	at := func(p float64) float64 { // nearest rank
		k := int(math.Ceil(p*float64(len(v)))) - 1
		return v[min(max(k, 0), len(v)-1)]
	}
	return Latency{N: len(v), P50: at(0.50), P95: at(0.95), P99: at(0.99)}
}
