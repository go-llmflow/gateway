package gateway

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
)

// Property (#58): whatever the order of arrivals and departures, a request
// admitted within its organisation's share is never moved to a fallback,
// and a fallback pool taken through TryAcquire never exceeds its capacity.
func TestPropertyNoWithinShareRequestMoves(t *testing.T) {
	shares := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}
	orgs := []string{"a", "b", "c", "x"} // x has no share: it always borrows
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, seed*7919))
		c1, c2 := 1+r.IntN(6), 1+r.IntN(6)
		primary := &group{cfg: config.ModelGroup{Name: "p", FallbackOn: []string{"unavailable", "saturated"}},
			pool: localPool{fairshare.NewPool("p", c1, shares, time.Hour, nil)}}
		target := fairshare.NewPool("t", c2, shares, time.Hour, nil)
		var live []*fairshare.Grant
		for step := 0; step < 300; step++ {
			if len(live) > 0 && r.IntN(3) == 0 {
				i := r.IntN(len(live))
				live[i].Release(1)
				live = append(live[:i], live[i+1:]...)
				continue
			}
			g, _ := primary.pool.acquire(orgs[r.IntN(len(orgs))], fairshare.Interactive, 10)
			move := movesOnSaturation(primary, g)
			if move && !g.Borrowing {
				t.Fatalf("seed %d step %d: a within-share request would move", seed, step)
			}
			if move {
				if tg, ok := target.TryAcquire(g.Team(), fairshare.Interactive, 10); ok {
					g.Release(0)
					g = tg
				}
			}
			live = append(live, g)
			inT := 0
			for _, s := range target.Snapshot() {
				inT += s.InFlight
			}
			if inT > c2 {
				t.Fatalf("seed %d step %d: the fallback pool holds %d > C=%d", seed, step, inT, c2)
			}
		}
	}
}
