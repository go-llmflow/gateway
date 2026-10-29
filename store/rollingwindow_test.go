package store

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Spend in a rolling window is never lost while it can still fall inside a
// window that may apply (#98): widening a window finds the earlier spend.

// rollingWiden is the issue's repro: 0.9 spent, 2h30 pass with the 1h window
// locked, the window widened to 5h: the 0.9 is inside it.
func rollingWiden(s Store) error {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	k := BudgetKey{Kind: ScopeProfileSession, Name: "rv-ann"}
	a := Holder{Replica: "rv", Incarnation: "rvi"}
	steps := []func() error{
		func() error {
			return s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: RollingPeriod(time.Hour), MaxCost: 1}, t0, time.Minute, false)
		},
		func() error {
			_, err := s.GrowGrants(ctx, a, t0, time.Minute, []GrantRequest{{Key: k, Start: RollingStart, Need: 1}})
			return err
		},
		func() error {
			return s.InsertRecords(ctx, []Record{{ID: "rv-1", Time: t0, AdmittedAt: t0, Team: "rt", KeyID: "rk", User: k.Name, Cost: 0.9, CostNano: 9 * unitNano / 10}})
		},
	}
	later := t0.Add(150 * time.Minute)
	steps = append(steps,
		func() error {
			_, err := s.GrowGrants(ctx, a, later, time.Minute, []GrantRequest{{Key: k, Start: RollingStart, Need: 1}})
			return err
		},
		func() error {
			return s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: RollingPeriod(5 * time.Hour), MaxCost: 1}, later, time.Minute, false)
		})
	for _, f := range steps {
		if err := f(); err != nil {
			return err
		}
	}
	st, err := s.LedgerAt(ctx, k, RollingStart, later)
	if err != nil {
		return err
	}
	if st.Spent != 9*unitNano/10 {
		return fmt.Errorf("spent %d nano after widening to 5h, want %d", st.Spent, 9*unitNano/10)
	}
	return nil
}

// rollingWindowsProperty: under random traffic and random window changes
// (widening and narrowing), the ledger's spent equals an oracle that sums
// the records whose bucket overlaps the current window.
func rollingWindowsProperty(s Store, seed int64) error {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	windows := []time.Duration{time.Hour, 2 * time.Hour, 5 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour}
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	k := BudgetKey{Kind: ScopeProfileSession, Name: fmt.Sprintf("rw-prop-%d", seed)}
	a := Holder{Replica: "rw", Incarnation: fmt.Sprintf("rwi-%d", seed)}
	d := windows[rng.Intn(len(windows))]
	set := func() error {
		return s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: RollingPeriod(d), MaxCost: 1000}, now, time.Minute, false)
	}
	grow := func() error {
		_, err := s.GrowGrants(ctx, a, now, time.Minute, []GrantRequest{{Key: k, Start: RollingStart, Need: 1}})
		return err
	}
	if err := set(); err != nil {
		return err
	}
	if err := grow(); err != nil {
		return err
	}
	type rec struct {
		at   time.Time
		nano int64
	}
	var recs []rec
	for i := 0; i < 60; i++ {
		now = now.Add(time.Duration(rng.Intn(180)) * time.Minute)
		switch rng.Intn(4) {
		case 0, 1: // traffic
			n := int64(1+rng.Intn(50)) * unitNano / 1000
			id := fmt.Sprintf("rw-%d-%d", seed, i)
			if err := s.InsertRecords(ctx, []Record{{ID: id, Time: now, AdmittedAt: now, Team: "rt", KeyID: "rk", User: k.Name, Cost: float64(n) / unitNano, CostNano: n}}); err != nil {
				return err
			}
			recs = append(recs, rec{now, n})
		case 2: // the lock path, which prunes
			if err := grow(); err != nil {
				return err
			}
		case 3: // the window changes: wider or narrower
			d = windows[rng.Intn(len(windows))]
			if err := set(); err != nil {
				return err
			}
		}
		var want int64
		for _, r := range recs {
			if !BucketOf(r.at).Before(WindowStart(now, d)) {
				want += r.nano
			}
		}
		st, err := s.LedgerAt(ctx, k, RollingStart, now)
		if err != nil {
			return err
		}
		if st.Spent != want {
			return fmt.Errorf("seed %d step %d (window %v, at %v): spent %d, oracle %d", seed, i, d, now, st.Spent, want)
		}
	}
	return nil
}

func contractRollingWindows(t *testing.T, mk func() Store, seeds int) {
	t.Helper()
	if err := rollingWiden(mk()); err != nil {
		t.Fatalf("widen: %v", err)
	}
	s := mk()
	for seed := int64(1); seed <= int64(seeds); seed++ {
		if err := rollingWindowsProperty(s, seed); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRollingWindowsMemory(t *testing.T) {
	contractRollingWindows(t, func() Store { return NewMemory() }, 200)
}

// The planted control: with the old pruning (twice the CURRENT window) the
// repro and the property both fail, so they can see the defect.
func TestRollingWindowsControl(t *testing.T) {
	old := retainFrom
	defer func() { retainFrom = old }()
	retainFrom = func(now time.Time, d time.Duration) time.Time { return WindowStart(now, d).Add(-d) }
	if err := rollingWiden(NewMemory()); err == nil {
		t.Fatal("the old pruning passed the repro: the control is blind")
	}
	failed := 0
	for seed := int64(1); seed <= 200; seed++ {
		if rollingWindowsProperty(NewMemory(), seed) != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("the old pruning passed the property: the control is blind")
	}
	t.Logf("old pruning fails %d/200 seeds", failed)
}
