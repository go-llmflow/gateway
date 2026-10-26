package ha

import (
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

// engine is the owner's fair-share state of one pool: the pool itself and
// the grants it counts, by replica. Every method is called with the
// pool's lock held (poolState.mu).
type engine struct {
	pool  *fairshare.Pool
	epoch int64
	// grants by key (instance + " " + grant id).
	grants map[string]*engGrant
	// replicas: when each instance last reported (monotonic clock).
	replicas map[string]time.Time
	// released: keys released recently, so a late report or a duplicate
	// release does not count a request twice.
	released map[string]time.Time
	maxReq   time.Duration
	hb       time.Duration
}

type engGrant struct {
	g       *fairshare.Grant
	inst    string
	org     string
	created time.Time
}

func newEngine(spec PoolSpec, epoch int64, now func() time.Time, maxReq, hb time.Duration) *engine {
	return &engine{
		pool:  fairshare.NewPool(spec.Name, spec.Capacity, spec.Shares, spec.HalfLife, now),
		epoch: epoch, grants: map[string]*engGrant{}, replicas: map[string]time.Time{},
		released: map[string]time.Time{}, maxReq: maxReq, hb: hb,
	}
}

func gkey(inst, id string) string { return inst + " " + id }

// decide admits a request (TryAcquire when needRoom). A duplicate of a
// request already decided gets the same answer.
func (e *engine) decide(inst, id, org string, class fairshare.Class, tokens int64, needRoom bool, now time.Time) (*fairshare.Grant, bool) {
	k := gkey(inst, id)
	if eg := e.grants[k]; eg != nil {
		return eg.g, true
	}
	if _, gone := e.released[k]; gone {
		return nil, false
	}
	var g *fairshare.Grant
	if needRoom {
		var ok bool
		if g, ok = e.pool.TryAcquire(org, class, tokens); !ok {
			return nil, false
		}
	} else {
		g = e.pool.Acquire(org, class, tokens)
	}
	e.grants[k] = &engGrant{g: g, inst: inst, org: org, created: now}
	if _, seen := e.replicas[inst]; !seen {
		e.replicas[inst] = now
	}
	return g, true
}

// release ends a request and charges it. A request this engine never
// counted is charged all the same; when it was decided under an earlier
// epoch it was part of the in-flight count the previous owner left, so one
// phantom of its organisation goes.
func (e *engine) release(inst string, r wireRelease, now time.Time) {
	k := gkey(inst, r.ID)
	if eg := e.grants[k]; eg != nil {
		eg.g.Release(r.Consumed)
		delete(e.grants, k)
		e.released[k] = now
		return
	}
	if _, gone := e.released[k]; gone {
		return
	}
	e.released[k] = now
	e.pool.Charge(r.Org, r.Consumed)
	if r.Epoch < e.epoch {
		e.pool.DropPhantom(r.Org)
	}
}

// report takes a replica's list of live grants: those this engine does
// not count are adopted (decided by a previous owner, or admitted while
// there was none), and those it counts for that replica but that the list
// no longer has are released, once they are old enough not to be answers
// still on their way.
func (e *engine) report(inst string, live []wireGrant, now time.Time) {
	e.replicas[inst] = now
	listed := make(map[string]bool, len(live))
	for _, w := range live {
		k := gkey(inst, w.ID)
		listed[k] = true
		if e.grants[k] != nil {
			continue
		}
		if _, gone := e.released[k]; gone {
			continue
		}
		g := e.pool.Adopt(w.Org, w.Priority, w.Borrowing, w.Tokens)
		e.grants[k] = &engGrant{g: g, inst: inst, org: w.Org, created: now}
		if w.Epoch < e.epoch {
			// Counted in the previous owner's in-flight (a phantom).
			e.pool.DropPhantom(w.Org)
		}
	}
	for k, eg := range e.grants {
		if eg.inst == inst && !listed[k] && now.Sub(eg.created) > 2*e.hb {
			eg.g.Release(0)
			delete(e.grants, k)
			e.released[k] = now
		}
	}
}

// tick turns the grants of replicas silent for 3 heartbeats into
// phantoms (their requests may still run: they count until
// max_request_duration after they were seen first) and forgets old
// releases.
func (e *engine) tick(self string, now time.Time, wall time.Time) {
	for inst, last := range e.replicas {
		if inst == self || now.Sub(last) <= 3*e.hb {
			continue
		}
		for k, eg := range e.grants {
			if eg.inst != inst {
				continue
			}
			eg.g.Release(0)
			delete(e.grants, k)
			left := e.maxReq - now.Sub(eg.created)
			if left > 0 {
				e.pool.AddPhantoms(eg.org, 1, wall.Add(left))
			}
		}
		delete(e.replicas, inst)
	}
	for k, t := range e.released {
		if now.Sub(t) > 4*e.hb+maxSkew/10 {
			delete(e.released, k)
		}
	}
}
