// Package lending turns a period's finished requests into the evidence for
// the second rule of a shared-GPU agreement (#97): *each partner's usage is
// guaranteed pro rata to its investment, and a partner may borrow what the
// others leave idle.* Per organisation it reports
//
//   - the guaranteed capacity: reserved slots × hours of the period (the
//     reservation is fairshare.Apportion(C, shares), the very slots the
//     gateway's admission test uses);
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
//     lent − borrowed, which sums to zero over the organisations;
//   - the latency of the requests, in share against borrowing. The gateway
//     does not record the time a request spent in vLLM's queue, so this is
//     end-to-end latency, not a wait time.
//
// Accounting identities, held by a property test against the real
// fairshare.Pool: for every organisation, the matrix column of a borrower sums
// to what it borrowed; the lent total equals the borrowed total less the
// unattributed; the nets sum to 0.
//
// A request belongs to the period it finished in. Its interval is
// [End − latency, End]; GPU-hours are the gateway's cost unit.
package lending

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

// MaxRequests bounds one computation: the sweep keeps two events per request.
const MaxRequests = 2_000_000

// ErrTooMany is returned for more than MaxRequests requests.
var ErrTooMany = errors.New("lending: too many requests in the period; shorten it")

// Req is one finished request that consumed GPU-hours.
type Req struct {
	Org, Pool string
	End       time.Time
	LatencyMS float64
	Borrowing bool
	GPUHours  float64
}

// Pool is a capacity domain with the shares of the organisations.
type Pool struct {
	Name     string
	Capacity int
	Shares   map[string]float64
}

// Latency holds percentiles of end-to-end latency in milliseconds.
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
	// InShare and Borrow are the latency of the requests inside the
	// reservation and of those above it.
	InShare, Borrow Latency
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
}

// Compute builds the report of [from, to) from the requests that finished in it.
func Compute(from, to time.Time, pools []Pool, reqs []Req) (*Result, error) {
	if len(reqs) > MaxRequests {
		return nil, ErrTooMany
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("lending: from must be before to")
	}
	hours := to.Sub(from).Hours()
	orgIdx := map[string]int{}
	var orgs []string
	add := func(o string) int {
		if i, ok := orgIdx[o]; ok {
			return i
		}
		orgIdx[o] = len(orgs)
		orgs = append(orgs, o)
		return len(orgs) - 1
	}
	poolIdx := map[string]int{}
	reserved := make([][]int, len(pools)) // pool → org index → slots (filled after all orgs are known)
	for i, p := range pools {
		poolIdx[p.Name] = i
		names := make([]string, 0, len(p.Shares))
		for o := range p.Shares {
			names = append(names, o)
		}
		sort.Strings(names)
		for _, o := range names {
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
	for i, p := range pools {
		names := make([]string, 0, len(p.Shares))
		for o := range p.Shares {
			names = append(names, o)
		}
		sort.Strings(names)
		res := fairshare.Apportion(p.Capacity, names, p.Shares)
		reserved[i] = make([]int, n)
		for _, o := range names {
			reserved[i][orgIdx[o]] = res[o]
			stat[orgIdx[o]].GuaranteedHours += float64(res[o]) * hours
		}
	}

	res := &Result{From: from, To: to, Matrix: map[string]map[string]float64{}, Requests: len(reqs)}
	evs := make([][]event, len(pools))
	var inLat, bLat = make([][]float64, n), make([][]float64, n)
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
		if r.Borrowing {
			stat[oi].Borrowed += cost
			bLat[oi] = append(bLat[oi], lat)
		} else {
			stat[oi].UsedInShare += cost
			inLat[oi] = append(inLat[oi], lat)
		}
		if !ok { // a pool the configuration no longer has: counted, not placed in time
			if r.Borrowing {
				res.Unattributed += cost
			}
			continue
		}
		end := r.End.UnixMilli()
		start := end - int64(math.Ceil(lat))
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
		sweep(evs[pi], reserved[pi], n, lent, &res.Unattributed)
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
	}
	res.Orgs = stat
	return res, nil
}

// sweep integrates one pool's timeline: between two events the in-share
// concurrency and the borrowing rates are constant, the idle reservation of
// each organisation is max(0, reserved - in share), and each borrower's rate
// is split between the lenders by their idle slots.
func sweep(evs []event, reserved []int, n int, lent [][]float64, unattributed *float64) {
	sort.Slice(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	inShare := make([]int, n)
	rate := make([]float64, n)
	idle := make([]float64, n)
	for i := 0; i < len(evs); {
		t := evs[i].t
		for ; i < len(evs) && evs[i].t == t; i++ {
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
