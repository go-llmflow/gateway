package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// escrow admits requests against this process's grants in the budget
// ledger (store/ledger.go, hard budgets #87). A request is admitted when,
// for every budget period it falls under, the grant this process holds
// covers the worst-case cost of everything it admitted and has not
// finished:
//
//	admitted + W ≤ granted
//
// so the store's invariant spent + Σ granted ≤ limit bounds what can ever
// be spent. A grant that runs short grows in one store transaction (all
// the request's budgets or none) by the larger of what is missing and a
// share of the room left; near the limit, grants shrink to one request. A
// grant left unused for grant_idle goes back to the store.
//
// The local count of a grant runs ahead of the store's: a finished request
// lowers it at once by min(actual, W), the store when the request's record
// is stored (the same amount, in the insert's transaction). Until then the
// store holds more than this process uses, never less.
type escrow struct {
	st      store.BudgetLedger
	holder  store.Holder
	share   float64
	idle    time.Duration
	lendFor time.Duration // the longest a request runs
	now     func() time.Time

	mu    sync.Mutex
	cells map[cellKey]*grantCell
	// returns are grants to give back, retried until the store takes them.
	returns []store.GrantReturn

	refills   atomic.Int64
	refused   atomic.Int64
	exceeded  atomic.Int64
	returnErr atomic.Int64
}

type cellKey struct {
	k     store.BudgetKey
	start int64 // unix nanoseconds
}

// grantCell is this process's grant on one budget period.
type grantCell struct {
	granted  int64 // in the store, once this process's records are settled
	admitted int64 // Σ W of the requests admitted and not finished
	pending  int64 // Σ W of the requests waiting on a growth
	epoch    int64
	until    time.Time // the grant is not lent from then on (zero: never)
	used     time.Time
	// gen changes when the grant stops being lendable (a new epoch, the
	// end of a temporary increase): what a request admitted before leaves
	// unused then goes back to the store.
	gen int64
	// room is the store's room left as last seen (-1: unknown), at roomAt.
	room   int64
	roomAt time.Time
}

// noRoomTTL is how long a growth the store refused is not asked again for
// a request that needs more than the room it reported: an exhausted budget
// must not turn every request into a store transaction (the property test
// found the limit changes starved behind them).
const noRoomTTL = 250 * time.Millisecond

// budgetRef is a budget period a request falls under, with the budget's
// epoch as this process knows it.
type budgetRef struct {
	key    store.BudgetKey
	start  time.Time
	epoch  int64
	period store.Period
}

// reset is when the budget period resets.
func (r budgetRef) reset() time.Time { return r.period.End(r.start) }

// tightest returns the reset of the budget with the least room left for
// this process (the store's room as last seen plus its unused grant), or
// of the earliest to reset when none is known.
func (e *escrow) tightest(refs []budgetRef) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	var at time.Time
	best := int64(-1)
	for _, r := range refs {
		c := e.cells[cellKey{r.key, r.start.UnixNano()}]
		if c != nil && c.room >= 0 {
			if room := max(0, c.granted-c.admitted) + c.room; best < 0 || room < best {
				best, at = room, r.reset()
			}
		}
	}
	if best >= 0 {
		return at
	}
	for _, r := range refs {
		if at.IsZero() || r.reset().Before(at) {
			at = r.reset()
		}
	}
	return at
}

// resetOf is the reset of the budget k among refs.
func resetOf(refs []budgetRef, k store.BudgetKey) time.Time {
	for _, r := range refs {
		if r.key == k {
			return r.reset()
		}
	}
	return time.Time{}
}

// replicaName names this replica in the ledger: ha.replica_id, else the
// host name (an administrator releases a replica that never comes back by
// this name).
func replicaName(cfg *config.Config) string {
	if cfg.HA != nil && cfg.HA.ReplicaID != "" {
		return cfg.HA.ReplicaID
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "gateway"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newEscrow(st store.BudgetLedger, h store.Holder, share float64, idle, lendFor time.Duration, now func() time.Time) *escrow {
	return &escrow{st: st, holder: h, share: share, idle: idle, lendFor: lendFor, now: now, cells: map[cellKey]*grantCell{}}
}

// hold is one admitted request's reservation.
type hold struct {
	e    *escrow
	refs []budgetRef
	gens []int64
	w    int64
	at   time.Time
	done atomic.Bool
}

// noRoom refuses a request: a budget it falls under has less room left for
// this process than the request's worst-case cost.
type noRoom struct {
	key    store.BudgetKey
	room   int64 // nano-units this process could still get for it
	arming bool  // the budget is arming (arming.go)
}

func (e *noRoom) Error() string { return "budget exhausted" }

func (e *escrow) cellFor(r budgetRef) *grantCell {
	ck := cellKey{r.key, r.start.UnixNano()}
	c := e.cells[ck]
	if c == nil {
		c = &grantCell{epoch: r.epoch, room: -1}
		e.cells[ck] = c
	}
	return c
}

// lendable checks that a cell's grant may still be lent at now under the
// budget's epoch; when it may not, its unused part goes back (e.mu held).
//
// Epochs only grow (store.nextEpoch): a budget refreshed to a newer epoch
// than the grant's invalidates it; a cache older than the grant (the
// refresh lags the growth that returned the new epoch) does not.
func (e *escrow) lendable(r budgetRef, c *grantCell, now time.Time) {
	if r.epoch <= c.epoch && (c.until.IsZero() || now.Before(c.until)) {
		return
	}
	if free := c.granted - c.admitted; free > 0 {
		c.granted -= free
		e.returns = append(e.returns, store.GrantReturn{Key: r.key, Start: r.start, Amount: free})
	}
	// What stays granted covers the requests admitted under the old terms
	// only; their leftovers go back when they finish (settle, gen).
	c.gen++
	c.epoch, c.until, c.room = max(c.epoch, r.epoch), time.Time{}, -1
}

// acquire reserves w on every budget period of refs, growing the grants
// that run short. It returns *noRoom when a budget cannot hold w.
func (e *escrow) acquire(ctx context.Context, refs []budgetRef, w int64) (*hold, error) {
	if w < 0 {
		w = 0
	}
	if w > maxLedgerNano {
		e.refused.Add(1)
		return nil, &noRoom{key: refs[0].key, room: e.roomFor(refs)}
	}
	for attempt := 0; ; attempt++ {
		e.mu.Lock()
		now := e.now()
		var reqs []store.GrantRequest
		for _, r := range refs {
			c := e.cellFor(r)
			e.lendable(r, c, now)
			if need := c.admitted + c.pending + w - c.granted; need > 0 {
				if c.room >= 0 && need > c.room && time.Since(c.roomAt) < noRoomTTL {
					// The store said there is not enough, a moment ago.
					room := max(0, c.granted-c.admitted-c.pending) + c.room
					e.mu.Unlock()
					e.refused.Add(1)
					return nil, &noRoom{key: r.key, room: room}
				}
				reqs = append(reqs, store.GrantRequest{Key: r.key, Start: r.start, Need: need, Want: w, Share: e.share})
			}
		}
		if len(reqs) == 0 {
			h := &hold{e: e, refs: refs, gens: make([]int64, len(refs)), w: w, at: now}
			for i, r := range refs {
				c := e.cellFor(r)
				c.admitted += w
				c.used = now
				h.gens[i] = c.gen
			}
			e.mu.Unlock()
			return h, nil
		}
		if attempt >= 3 {
			// Others keep taking what each growth adds: refuse rather than
			// spin (the room exists, the client may retry).
			e.mu.Unlock()
			e.refused.Add(1)
			return nil, &noRoom{key: reqs[0].Key, room: e.roomFor(refs)}
		}
		for _, r := range refs {
			e.cellFor(r).pending += w
		}
		// Grants that stopped being lendable go back first: the store
		// would otherwise count them against this very growth.
		rets := e.returns
		e.returns = nil
		e.mu.Unlock()
		if len(rets) > 0 {
			if err := e.st.ReturnGrants(ctx, e.holder, mergeReturns(rets)); err != nil {
				e.returnErr.Add(1)
				e.mu.Lock()
				e.returns = append(e.returns, rets...)
				e.mu.Unlock()
			}
		}

		res, err := e.st.GrowGrants(ctx, e.holder, now, e.lendFor, reqs)

		e.mu.Lock()
		for _, r := range refs {
			e.cellFor(r).pending -= w
		}
		var nr *store.NoRoomError
		switch {
		case errors.As(err, &nr):
			for _, r := range refs {
				if r.key == nr.Key {
					c := e.cellFor(r)
					c.room, c.roomAt = nr.Room, time.Now() // real time: a rate limit, not a period
				}
			}
			e.mu.Unlock()
			e.refused.Add(1)
			if nr.Arming {
				// Not cached: the reports come in within a second.
				e.mu.Lock()
				for _, r := range refs {
					if r.key == nr.Key {
						e.cellFor(r).room = -1
					}
				}
				e.mu.Unlock()
				return nil, &noRoom{key: nr.Key, arming: true}
			}
			return nil, &noRoom{key: nr.Key, room: e.roomFor(refs)}
		case err != nil:
			e.mu.Unlock()
			return nil, err
		}
		e.refills.Add(1)
		for _, g := range res {
			c := e.cellFor(budgetRef{key: g.Key, start: g.Start, epoch: g.Epoch})
			if g.NoBudget {
				// Deleted meanwhile: nothing to hold on it until the next
				// refresh drops it from the request's budgets.
				c.granted += reqNeed(reqs, g.Key, g.Start)
				c.room = -1
				continue
			}
			c.granted += g.Grown
			c.room, c.roomAt = g.Room, time.Now()
			if g.Epoch > c.epoch {
				// A growth under a newer epoch than the grant's: what the
				// cell held before was lent under older terms.
				if free := c.granted - g.Grown - c.admitted; free > 0 && c.epoch != 0 {
					c.granted -= free
					e.returns = append(e.returns, store.GrantReturn{Key: g.Key, Start: g.Start, Amount: free})
					c.gen++
				}
				c.epoch = g.Epoch
			}
			if !g.Until.IsZero() && (c.until.IsZero() || g.Until.Before(c.until)) {
				c.until = g.Until
			}
		}
		e.mu.Unlock()
	}
}

// reqNeed is the need a request carried for one budget period.
func reqNeed(reqs []store.GrantRequest, k store.BudgetKey, start time.Time) int64 {
	for _, r := range reqs {
		if r.Key == k && r.Start.Equal(start) {
			return r.Need
		}
	}
	return 0
}

// roomFor is the most a request of refs could hold now: for each budget,
// this process's unused grant plus the store's room as last seen (unknown
// counts as no limit), the smallest of them.
func (e *escrow) roomFor(refs []budgetRef) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	best := int64(-1)
	for _, r := range refs {
		c := e.cells[cellKey{r.key, r.start.UnixNano()}]
		if c == nil || c.room < 0 {
			continue
		}
		room := max(0, c.granted-c.admitted-c.pending) + c.room
		if best < 0 || room < best {
			best = room
		}
	}
	if best < 0 {
		return maxLedgerNano
	}
	return best
}

// settle ends a hold whose request cost actual (0 when nothing was
// charged). The store settles the same amount when the request's record
// is stored; a request that leaves no record leaves its whole W in the
// grant, unused. It reports whether actual exceeded W (a bug in the bound:
// the truth is charged anyway).
func (h *hold) settle(actual int64) bool {
	if h == nil || !h.done.CompareAndSwap(false, true) {
		return false
	}
	e := h.e
	dec := min(max(actual, 0), h.w)
	e.mu.Lock()
	now := e.now()
	for i, r := range h.refs {
		c := e.cellFor(r)
		c.admitted -= h.w
		c.granted -= dec
		c.used = now
		if h.gens[i] != c.gen {
			// The grant stopped being lendable while the request ran: what
			// it did not use goes back.
			if left := h.w - dec; left > 0 {
				c.granted -= left
				e.returns = append(e.returns, store.GrantReturn{Key: r.key, Start: r.start, Amount: left})
			}
		}
	}
	e.mu.Unlock()
	if actual > h.w {
		e.exceeded.Add(1)
		return true
	}
	return false
}

// forgetRoom drops the refusals this process cached (an administrator
// changed a budget or released grants).
func (e *escrow) forgetRoom() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.cells {
		c.room = -1
	}
}

// tick gives back the grants left unused for idle (all of them when all
// is set: shutdown), forgets empty cells, and retries the returns the
// store refused.
func (e *escrow) tick(ctx context.Context, all bool) {
	e.mu.Lock()
	now := e.now()
	for ck, c := range e.cells {
		free := c.granted - c.admitted
		if free > 0 && c.pending == 0 && (all || now.Sub(c.used) >= e.idle) {
			c.granted -= free
			e.returns = append(e.returns, store.GrantReturn{Key: ck.k, Start: time.Unix(0, ck.start).UTC(), Amount: free})
		}
		if c.granted == 0 && c.admitted == 0 && c.pending == 0 && now.Sub(c.used) >= e.idle {
			delete(e.cells, ck)
		}
	}
	rets := e.returns
	e.returns = nil
	e.mu.Unlock()
	if len(rets) == 0 {
		return
	}
	rets = mergeReturns(rets)
	if err := e.st.ReturnGrants(ctx, e.holder, rets); err != nil {
		e.returnErr.Add(1)
		e.mu.Lock()
		e.returns = append(e.returns, rets...)
		e.mu.Unlock()
	}
}

// mergeReturns sums the returns of each budget period.
func mergeReturns(rets []store.GrantReturn) []store.GrantReturn {
	sum := map[cellKey]int64{}
	for _, r := range rets {
		sum[cellKey{r.Key, r.Start.UnixNano()}] += r.Amount
	}
	out := make([]store.GrantReturn, 0, len(sum))
	for ck, a := range sum {
		out = append(out, store.GrantReturn{Key: ck.k, Start: time.Unix(0, ck.start).UTC(), Amount: a})
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Key.String(), out[j].Key.String(); a != b {
			return a < b
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out
}

func (e *escrow) loop(ctx context.Context) {
	t := time.NewTicker(max(e.idle/2, 50*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
			e.tick(rctx, false)
			cancel()
		}
	}
}
