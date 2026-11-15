package ha

import (
	"testing"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

// gateway#180 (review): an owner request admitted while no owner can decide (the
// lease lapsed, a forward timed out) lives only in the replica's own grant list.
// The pool's count must include it as soon as an owner can know: a spare-only
// borrower must not be admitted into a pool that the owners' requests fill.

// The owner replica's own degraded grant is adopted by its engine (at the next
// heartbeat), so a borrower asking for room is refused.
func TestAnOwnerRequestAdmittedWithoutAnOwnerCountsWhenTheOwnerReturns(t *testing.T) {
	st := NewMemStore(nil)
	m := startMember(t, st, "r1", nil)
	if !owner(m) {
		t.Fatal("the member should own the pool")
	}
	ps := m.n.pools["p"]
	for i := 0; i < 3; i++ { // capacity 4: three owner requests in flight
		acquire(t, m, "a")
	}
	var degraded *fairshare.Grant
	for try := 0; try < 20 && degraded == nil; try++ {
		// the validity lapses (a missed renewal): nobody can decide
		ps.mu.Lock()
		ps.validUntil = time.Time{}
		ps.mu.Unlock()
		before := m.n.Degraded()
		g, ok, err := m.n.Acquire("p", "b", fairshare.Interactive, 100, false)
		if err == nil && ok && m.n.Degraded() == before+1 {
			degraded = g
		} else if err == nil && ok {
			g.Release(0) // the renewal won the race: decided normally, try again
		}
		time.Sleep(20 * time.Millisecond)
	}
	if degraded == nil {
		t.Skip("could not admit a request while the validity was lapsed")
	}
	// the owner is back (the next renewal) and the heartbeats run: the pool holds 4
	time.Sleep(6 * m.n.cfg.Heartbeat)
	g, ok, err := m.n.Acquire("p", "t", fairshare.Interactive, 100, true)
	if err == nil && ok {
		g.Release(0)
		t.Fatalf("a spare-only borrower was admitted into a pool of 4 holding 4 owner requests (3 decided, 1 admitted while no owner decided)")
	}
}

// A request admitted before any owner decided (epoch 0) was never part of the
// in-flight count a previous owner left: its release does not end a phantom.
func TestReleasingAnUnownedGrantKeepsThePhantoms(t *testing.T) {
	e := newEngine(PoolSpec{Name: "p", Capacity: 4, Shares: map[string]float64{"a": 0.5, "b": 0.5}, HalfLife: time.Hour}, 2, time.Now, time.Second, 100*time.Millisecond)
	e.pool.AddPhantoms("a", 1, time.Now().Add(time.Minute))
	e.release("r1", wireRelease{ID: "x", Org: "a", Epoch: 0}, time.Now())
	if _, inflight := e.pool.Usage(); inflight["a"] != 1 {
		t.Fatalf("the phantom of a request decided by a previous owner went with the release of one no owner decided: in flight %d, want 1", inflight["a"])
	}
	// and one decided by the previous owner (epoch 1 < 2) does drop its phantom
	e.release("r1", wireRelease{ID: "y", Org: "a", Epoch: 1}, time.Now())
	if _, inflight := e.pool.Usage(); inflight["a"] != 0 {
		t.Fatalf("the release of a request of the previous epoch must drop its phantom: in flight %d", inflight["a"])
	}
}
