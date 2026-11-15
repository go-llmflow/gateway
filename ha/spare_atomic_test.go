package ha

import (
	"sync"
	"testing"

	"github.com/go-llmflow/gateway/fairshare"
)

// gateway#180: "9 requests in flight at the upstream for a capacity of 8". The
// guarantee of a spare-only borrower is that it takes a slot only when the pool
// has room AT THE MOMENT it is admitted, whichever replica it arrives at, and
// that the check and the take are one step (the owner of the pool decides under
// one lock). It is not that the upstream never holds more than C requests: the
// owners are never refused (their requests queue by priority at the engine), so
// owners admitted after the borrower may bring the total above C.
//
// Two replicas, a start barrier, the pool at its edge (3 of 4 slots busy): of 16
// borrowers asking at once, from both replicas, exactly one is admitted.
func TestSpareOnlyAdmissionIsOneStepAcrossReplicas(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	m2 := startMember(t, st, "r2", nil)
	if !owner(m1) || owner(m2) {
		t.Fatal("the first replica started should own the pool")
	}
	for round := 0; round < 30; round++ {
		var held []*fairshare.Grant
		for i := 0; i < 3; i++ {
			held = append(held, acquire(t, []*member{m1, m2}[i%2], "a"))
		}
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			admitted []*fairshare.Grant
			start    = make(chan struct{})
		)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(m *member) {
				defer wg.Done()
				<-start
				g, ok, err := m.n.Acquire("p", "t", fairshare.Interactive, 100, true)
				if err == nil && ok {
					mu.Lock()
					admitted = append(admitted, g)
					mu.Unlock()
				}
			}([]*member{m1, m2}[i%2])
		}
		close(start)
		wg.Wait()
		if len(admitted) != 1 {
			t.Fatalf("round %d: %d spare-only requests admitted into a pool with 1 free slot of 4", round, len(admitted))
		}
		// the owners are never refused: the pool may be taken past C by them
		over := acquire(t, m2, "a")
		over.Release(0)
		for _, g := range append(held, admitted...) {
			g.Release(0)
		}
		eventually(t, 2e9, "releases reach the owner", func() bool {
			for _, s := range m1.n.Snapshot("p") {
				if s.InFlight != 0 {
					return false
				}
			}
			return true
		})
	}
}
