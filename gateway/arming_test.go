package gateway

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// The tracker's state machine, property-checked: a report covers every
// request pending in the budget's scope when the budget was loaded
// (admitted in its period), never one tracked after it, only goes down,
// and reaches 0 once they are all settled.
func TestTrackerReports(t *testing.T) {
	k := store.BudgetKey{Kind: store.ScopeTeam, Name: "t"}
	other := store.BudgetKey{Kind: store.ScopeTeam, Name: "u"}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for seed := uint64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewPCG(seed, 1))
		tr := newTracker()
		var ids []string
		var want int64 // in k's scope, admitted today, before the snapshot
		n := 0
		add := func(counted bool) {
			n++
			id := fmt.Sprintf("r%d", n)
			ids = append(ids, id)
			keys := []store.BudgetKey{other}
			if rng.IntN(2) == 0 {
				keys = append(keys, k)
			}
			at := now
			if rng.IntN(5) == 0 {
				at = now.Add(-24 * time.Hour) // yesterday: another period
			}
			w := 1 + rng.Int64N(100)
			tr.track(id, keys, w, at)
			if counted && inKeys(k, keys) && !at.Before(store.Day.Start(now)) {
				want += w
			}
		}
		for i := rng.IntN(20); i > 0; i-- {
			add(true)
		}
		tr.sync([]store.Budget{{BudgetKey: k, Period: store.Day, Arming: true}, {BudgetKey: other, Period: store.Day}})
		for i := rng.IntN(10); i > 0; i-- {
			add(false) // after the snapshot: held by then, not reported
		}
		reps := tr.due(now)
		if len(reps) != 1 || reps[0].pre != want {
			t.Fatalf("seed %d: first report %+v, want %d", seed, reps, want)
		}
		last := want
		tr.reported(k, last, false)
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		for _, id := range ids {
			tr.untrack(id)
			for _, r := range tr.due(now) {
				if r.pre > last {
					t.Fatalf("seed %d: the report went up: %d after %d", seed, r.pre, last)
				}
				last = r.pre
				tr.reported(k, r.pre, false)
			}
		}
		if last != 0 {
			t.Fatalf("seed %d: %d left when everything settled", seed, last)
		}
		tr.sync([]store.Budget{{BudgetKey: k, Period: store.Day}}) // active
		if len(tr.due(now)) != 0 {
			t.Fatalf("seed %d: an active budget is still reported", seed)
		}
	}
}
