package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRollingPeriod(t *testing.T) {
	for _, c := range []struct {
		p    Period
		d    time.Duration
		ok   bool
		name string
	}{
		{"rolling:5h", 5 * time.Hour, true, "hours"},
		{"rolling:5m", 5 * time.Minute, true, "one bucket"},
		{"rolling:90m", 90 * time.Minute, true, "minutes"},
		{"rolling:168h", 168 * time.Hour, true, "the longest"},
		{"rolling:169h", 0, false, "too long"},
		{"rolling:4m", 0, false, "shorter than a bucket"},
		{"rolling:7m", 0, false, "not whole buckets"},
		{"rolling:60m", 0, false, "not the canonical spelling (1h)"},
		{"rolling:5h0m", 0, false, "not canonical"},
		{"rolling:-5h", 0, false, "negative"},
		{"rolling:", 0, false, "empty"},
		{"rolling:5H", 0, false, "unit case"},
		{"Rolling:5h", 0, false, "prefix case"},
		{"rolling:999999999999h", 0, false, "overflow"},
		{"week", 0, false, "calendar"},
	} {
		d, ok := c.p.Rolling()
		if ok != c.ok || d != c.d {
			t.Errorf("%s (%s): got %v %v", c.p, c.name, d, ok)
		}
		if ok && RollingPeriod(d) != c.p {
			t.Errorf("%s: spelled back as %s", c.p, RollingPeriod(d))
		}
		if c.p != "week" && c.p.Valid() != c.ok {
			t.Errorf("%s: Valid %v", c.p, c.p.Valid())
		}
	}
	now := time.Date(2026, 10, 28, 14, 7, 31, 0, time.UTC)
	p := RollingPeriod(5 * time.Hour)
	if !p.Start(now).Equal(RollingStart) || !p.End(RollingStart).Equal(RollingStart) {
		t.Fatal("a rolling period's start is RollingStart")
	}
	if got := BucketOf(now); !got.Equal(time.Date(2026, 10, 28, 14, 5, 0, 0, time.UTC)) {
		t.Fatalf("bucket %v", got)
	}
	if got := BucketOf(time.Unix(-1, 0)); !got.Equal(time.Unix(-300, 0).UTC()) {
		t.Fatalf("bucket before the epoch %v", got)
	}
	// The window counts every bucket overlapping (now-5h, now]: from 09:05.
	if got := p.SpendSince(now); !got.Equal(time.Date(2026, 10, 28, 9, 5, 0, 0, time.UTC)) {
		t.Fatalf("since %v", got)
	}
	// Whole again once the bucket of now (14:05-14:10) has left: 19:10.
	if got := p.ResetAt(now); !got.Equal(time.Date(2026, 10, 28, 19, 10, 0, 0, time.UTC)) {
		t.Fatalf("reset %v", got)
	}
	// Weeks are ISO 8601 weeks: Monday 00:00 UTC.
	if got := Week.ResetAt(now); !got.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) || got.Weekday() != time.Monday {
		t.Fatalf("week reset %v", got)
	}
	if ValidateBudget(Budget{BudgetKey: BudgetKey{Kind: ScopeProfileSession, Name: "u"}, Period: p, MaxCost: 1, SoftLimit: 0.5}) == nil {
		t.Fatal("a soft limit on a rolling window")
	}
	if err := ValidateBudget(Budget{BudgetKey: BudgetKey{Kind: ScopeProfileSession, Name: "u"}, Period: p, MaxCost: 1}); err != nil {
		t.Fatal(err)
	}
}

// rollingReset: the time the bucket that frees enough leaves the window.
func TestRollingReset(t *testing.T) {
	now := time.Date(2026, 10, 28, 14, 7, 0, 0, time.UTC)
	d := time.Hour
	b := func(hm string, v int64) bucketSpend {
		at, _ := time.Parse("15:04", hm)
		return bucketSpend{time.Date(2026, 10, 28, at.Hour(), at.Minute(), 0, 0, time.UTC), v}
	}
	bs := []bucketSpend{b("13:05", 3), b("13:30", 2), b("14:05", 5)}
	for _, c := range []struct {
		short int64
		want  string
	}{
		{1, "14:10"}, {3, "14:10"}, {4, "14:35"}, {5, "14:35"}, {10, "15:10"},
		{11, "15:10"}, // more than the buckets hold (grants): the whole window
	} {
		got := rollingReset(bs, now, d, c.short)
		if got.Format("15:04") != c.want {
			t.Errorf("short %d: %s, want %s", c.short, got.Format("15:04"), c.want)
		}
	}
}

// contractRolling: a rolling window's ledger counts the buckets in the
// window, gives the room back as they leave it, says when, and settles held
// and unheld records in the bucket of their admission.
func contractRolling(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	per := RollingPeriod(time.Hour)
	k := BudgetKey{Kind: ScopeProfileSession, Name: "rolling-ann"}
	a := Holder{Replica: "rr", Incarnation: "ri"}
	// A record from before the budget, 40 minutes ago, and one of the same
	// user beyond the profile (overflow), which the session does not count.
	must(s.InsertRecords(ctx, []Record{
		{ID: "roll-0", Time: t0.Add(-40 * time.Minute), AdmittedAt: t0.Add(-40 * time.Minute), Team: "rt", KeyID: "rk", User: k.Name, Cost: 0.2, CostNano: 2 * unitNano / 10},
		{ID: "roll-ov", Time: t0, AdmittedAt: t0, Team: "rt", KeyID: "rk", User: k.Name, Cost: 5, CostNano: 5 * unitNano, Overflow: true},
	}))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: per, MaxCost: 1}, t0, time.Minute, false))
	led := func(at time.Time) LedgerState {
		t.Helper()
		st, err := s.LedgerAt(ctx, k, RollingStart, at)
		must(err)
		return st
	}
	grow := func(at time.Time, need int64) ([]GrantResult, error) {
		return s.GrowGrants(ctx, a, at, time.Minute, []GrantRequest{{Key: k, Start: RollingStart, Need: need, Want: need}})
	}
	res, err := grow(t0, 5*unitNano/10)
	if err != nil || res[0].Grown != 5*unitNano/10 || res[0].Room != 3*unitNano/10 {
		t.Fatalf("first growth: %+v %v", res, err)
	}
	if st := led(t0); st.Spent != 2*unitNano/10 || st.Granted != 5*unitNano/10 {
		t.Fatalf("seeded: %+v", st)
	}
	// A held request settles in its admission's bucket: 0.4 of the 0.5.
	rec := Record{ID: "roll-1", Time: t0.Add(time.Minute), AdmittedAt: t0, Team: "rt", KeyID: "rk", User: k.Name, Cost: 0.4, CostNano: 4 * unitNano / 10,
		HoldReplica: a.Replica, HoldIncarnation: a.Incarnation, HoldNano: 5 * unitNano / 10, Holds: []HoldRef{{Key: k, Start: RollingStart}}}
	must(s.InsertRecords(ctx, []Record{rec}))
	if st := led(t0); st.Spent != 6*unitNano/10 || st.Granted != unitNano/10 {
		t.Fatalf("settled: %+v", st)
	}
	// An unheld record (admitted elsewhere before the budget was known).
	must(s.InsertRecords(ctx, []Record{{ID: "roll-2", Time: t0.Add(10 * time.Minute), AdmittedAt: t0.Add(10 * time.Minute), Team: "rt", KeyID: "rk",
		User: k.Name, Cost: 0.2, CostNano: 2 * unitNano / 10}}))
	at := t0.Add(10 * time.Minute)
	if st := led(at); st.Spent != 8*unitNano/10 {
		t.Fatalf("unheld: %+v", st)
	}
	// Full: 1 - 0.8 - 0.1 = 0.1 left. Asking 0.35 fails; the room comes
	// back when the 09:20 bucket (0.2) and the 10:00 one (0.4) have left:
	// 0.25 short, so 11:05.
	_, err = grow(at, 35*unitNano/100)
	var nr *NoRoomError
	if !errors.As(err, &nr) || nr.Room != unitNano/10 || !nr.Reset.Equal(time.Date(2026, 10, 28, 11, 5, 0, 0, time.UTC)) {
		t.Fatalf("refusal: %+v", err)
	}
	// 09:20's bucket leaves at 10:25: 0.2 back.
	if st := led(t0.Add(25 * time.Minute)); st.Spent != 6*unitNano/10 {
		t.Fatalf("after the oldest bucket left: %+v", st)
	}
	must(s.ReturnGrants(ctx, a, []GrantReturn{{Key: k, Start: RollingStart, Amount: unitNano / 10}}))
	later := time.Date(2026, 10, 28, 11, 5, 0, 0, time.UTC)
	res, err = grow(later, 35*unitNano/100)
	if err != nil || res[0].Room != 45*unitNano/100 {
		t.Fatalf("after the reset: %+v %v", res, err)
	}
	// A limit below what the window committed is refused, and allowed once
	// the window has emptied.
	if err := s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: per, MaxCost: 0.3}, later, time.Minute, false); !errors.Is(err, ErrBelowCommitted) {
		t.Fatalf("lowered below the window: %v", err)
	}
	must(s.ReturnGrants(ctx, a, []GrantReturn{{Key: k, Start: RollingStart, Amount: 35 * unitNano / 100}}))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: per, MaxCost: 0.3}, later, time.Minute, false))
	// Deleted, then the user works, then created again: the records stored
	// meanwhile count (they settled in no row).
	must(s.DeleteBudget(ctx, k))
	must(s.InsertRecords(ctx, []Record{{ID: "roll-3", Time: later, AdmittedAt: later, Team: "rt", KeyID: "rk", User: k.Name, Cost: 0.25, CostNano: 25 * unitNano / 100}}))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: per, MaxCost: 1}, later, time.Minute, false))
	if st := led(later); st.Spent != 45*unitNano/100 { // 10:10's 0.2 and 11:05's 0.25
		t.Fatalf("created again: %+v", st)
	}
	// The same for a calendar budget.
	kd := BudgetKey{Kind: ScopeProfileWeek, Name: "rolling-ann"}
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: kd, Period: Week, MaxCost: 10}, later, time.Minute, false))
	wk := Week.Start(later)
	// (the memory store makes a row at its first growth)
	touch := func(k BudgetKey, start time.Time) {
		t.Helper()
		_, err := s.GrowGrants(ctx, a, later, time.Minute, []GrantRequest{{Key: k, Start: start}})
		must(err)
	}
	touch(kd, wk)
	if st, _ := s.LedgerAt(ctx, kd, wk, later); st.Spent != 105*unitNano/100 {
		t.Fatalf("week seeded (0.2+0.4+0.2+0.25, no overflow): %+v", st)
	}
	must(s.DeleteBudget(ctx, kd))
	must(s.InsertRecords(ctx, []Record{{ID: "roll-4", Time: later, AdmittedAt: later, Team: "rt", KeyID: "rk", User: k.Name, Cost: 1, CostNano: unitNano}}))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: kd, Period: Week, MaxCost: 10}, later, time.Minute, false))
	if st, _ := s.LedgerAt(ctx, kd, wk, later); st.Spent != 205*unitNano/100 {
		t.Fatalf("week created again: %+v", st)
	}
	// The overflow budget counts only the overflow record.
	ko := BudgetKey{Kind: ScopeProfileOverflow, Name: "rolling-ann"}
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: ko, Period: Month, MaxCost: 10}, later, time.Minute, false))
	touch(ko, Month.Start(later))
	if st, _ := s.LedgerAt(ctx, ko, Month.Start(later), later); st.Spent != 5*unitNano {
		t.Fatalf("overflow: %+v", st)
	}
	// A request whose replica still sees a calendar period on a budget now
	// rolling (or the reverse) is refused, not granted on the wrong row.
	if _, err := s.GrowGrants(ctx, a, later, time.Minute, []GrantRequest{{Key: k, Start: Day.Start(later), Need: 1}}); !errors.As(err, &nr) {
		t.Fatalf("period mismatch: %v", err)
	}
}

func contractProfiles(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	must(s.AssignProfile(ctx, Assignment{User: "ann@lab.fr", Profile: "2d", SetBy: "hr", From: t0}))
	must(s.AssignProfile(ctx, Assignment{User: "bob", Profile: "q", Overflow: true, SetBy: "hr", From: t0}))
	must(s.AssignProfile(ctx, Assignment{User: "ann@lab.fr", Profile: "3d", SetBy: "fin", From: t0.Add(time.Hour)}))
	must(s.AssignProfile(ctx, Assignment{User: "bob", Profile: "", SetBy: "fin", From: t0.Add(2 * time.Hour)}))
	cur, err := s.Assignments(ctx)
	must(err)
	if len(cur) != 1 || cur[0].User != "ann@lab.fr" || cur[0].Profile != "3d" || cur[0].SetBy != "fin" || cur[0].To != nil {
		t.Fatalf("in force: %+v", cur)
	}
	h, err := s.AssignmentHistory(ctx, "ann@lab.fr")
	must(err)
	if len(h) != 2 || h[0].Profile != "2d" || h[0].To == nil || !h[0].To.Equal(t0.Add(time.Hour)) || h[1].To != nil {
		t.Fatalf("history: %+v", h)
	}
	if h, _ := s.AssignmentHistory(ctx, "bob"); len(h) != 1 || !h[0].Overflow || h[0].To == nil {
		t.Fatalf("removed: %+v", h)
	}
	for _, bad := range []Assignment{{User: "", Profile: "q"}, {User: "a b", Profile: "q"}, {User: "x", Profile: "Q"},
		{User: "x", Profile: "a-very-long-profile-identifier-xx"}, {User: "x\n", Profile: "q"}} {
		if s.AssignProfile(ctx, bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	must(s.SetWorkday(ctx, Workday{Unit: "", Amount: 0.8, SetBy: "admin", SetAt: t0}))
	must(s.SetWorkday(ctx, Workday{Unit: "USD", Amount: 6, SetBy: "admin", SetAt: t0}))
	must(s.SetWorkday(ctx, Workday{Unit: "USD", Amount: 13, SetBy: "fin", SetAt: t0.Add(time.Hour)}))
	ws, err := s.Workdays(ctx)
	must(err)
	if len(ws) != 2 || ws[0].Unit != "" || ws[1].Amount != 13 || ws[1].SetBy != "fin" {
		t.Fatalf("workdays: %+v", ws)
	}
	for _, bad := range []Workday{{Unit: "USD", Amount: 0}, {Unit: "USD", Amount: -1}, {Unit: "gpu_hours", Amount: 1}, {Unit: "U$", Amount: 1}, {Unit: "USD", Amount: 2e9}} {
		if s.SetWorkday(ctx, bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// The report's profile dimension: the profile in force at admission.
	must(s.InsertRecords(ctx, []Record{
		{ID: "prof-1", Time: t0, Team: "pt", User: "ann@lab.fr", Profile: "2d", Cost: 1},
		{ID: "prof-2", Time: t0.Add(2 * time.Hour), Team: "pt", User: "ann@lab.fr", Profile: "3d", Cost: 2},
		{ID: "prof-3", Time: t0.Add(2 * time.Hour), Team: "pt", User: "ann@lab.fr", Profile: "3d", Cost: 3},
	}))
	rows, err := s.Report(ctx, t0, t0.Add(3*time.Hour), []string{"profile", "user"})
	must(err)
	got := map[string]float64{}
	for _, r := range rows {
		if r.User == "ann@lab.fr" {
			got[r.Profile] += r.Cost
		}
	}
	if len(got) != 2 || got["2d"] != 1 || got["3d"] != 5 {
		t.Fatalf("report by profile: %+v", rows)
	}
	// The measured load of a pool: cost and busy time per unit.
	must(s.InsertRecords(ctx, []Record{
		{ID: "pl-1", Time: t0, Team: "pt", Pool: "pl-pool", Cost: 2, LatencyMS: 1_800_000},
		{ID: "pl-2", Time: t0.Add(time.Hour), Team: "pt", Pool: "pl-pool", Cost: 1, LatencyMS: 1_800_000},
		{ID: "pl-3", Time: t0.Add(time.Hour), Team: "pt", Pool: "pl-pool", Unit: "USD", Cost: 7, LatencyMS: 1000},
		{ID: "pl-4", Time: t0.Add(5 * time.Hour), Team: "pt", Pool: "pl-pool", Cost: 100, LatencyMS: 1000},
	}))
	loads, err := s.PoolLoad(ctx, t0, t0.Add(3*time.Hour))
	must(err)
	var gpu, usd *PoolLoad
	for i := range loads {
		switch l := &loads[i]; {
		case l.Pool == "pl-pool" && l.Unit == "":
			gpu = l
		case l.Pool == "pl-pool" && l.Unit == "USD":
			usd = l
		}
	}
	if gpu == nil || gpu.Cost != 3 || gpu.BusySeconds != 3600 || gpu.Requests != 2 || usd == nil || usd.Cost != 7 {
		t.Fatalf("pool load: %+v", loads)
	}
}

func TestRollingMemory(t *testing.T)  { contractRolling(t, NewMemory()) }
func TestProfilesMemory(t *testing.T) { contractProfiles(t, NewMemory()) }
