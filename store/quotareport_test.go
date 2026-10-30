package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func checkQuotaReportStore(t *testing.T, st interface {
	QuotaReportStore
	WarnStore
	ProfileStore
}) {
	ctx := context.Background()
	d := func(day int, h int) time.Time { return time.Date(2026, 10, day, h, 0, 0, 0, time.UTC) }
	mark := func(user, win string, th int, instance, at time.Time) {
		t.Helper()
		if fired, err := st.MarkWarning(ctx, WarnMark{User: user, Window: win, Threshold: th, Instance: instance, At: at}, nil); err != nil || !fired {
			t.Fatalf("mark %s %s %d: %v %v", user, win, th, fired, err)
		}
	}
	// Alice crosses 75 in two weekly windows: both are events, though the mark
	// keeps only the last instance. Bob crosses 95 once; one event is outside.
	mark("alice", "week", 75, d(5, 0), d(7, 10))
	mark("alice", "week", 75, d(12, 0), d(14, 10)) // a later instance of the same mark
	mark("bob", "week", 95, d(5, 0), d(8, 9))
	mark("carol", "week", 75, d(12, 0), d(40-30+20, 9)) // 2026-10-30
	mark("dave", "week", 75, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC))
	got, err := st.WarnEventsBetween(ctx, d(1, 0), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 100)
	if err != nil || len(got) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0] != (WarnEventRow{"alice", "week", 75, 2}) || got[1] != (WarnEventRow{"bob", "week", 95, 1}) || got[2].User != "carol" {
		t.Fatalf("%+v", got)
	}
	if _, err := st.WarnEventsBetween(ctx, d(1, 0), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 2); err != ErrTooMany {
		t.Fatalf("bound: %v", err)
	}

	// Refusals add up per day, then over the period.
	if err := st.AddRefusals(ctx, d(5, 3), []RefusalRow{{"etabA", "t1", "p1", RefusalProfileWeek, 2}, {"etabA", "t1", "", RefusalBudget, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRefusals(ctx, d(6, 3), []RefusalRow{{"etabA", "t1", "p1", RefusalProfileWeek, 3}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRefusals(ctx, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), []RefusalRow{{"etabA", "t1", "p1", RefusalProfileWeek, 9}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRefusals(ctx, d(5, 3), []RefusalRow{{"x", "y", "z", "nonsense", 1}}); err == nil {
		t.Fatal("a bad kind accepted")
	}
	rf, err := st.RefusalsBetween(ctx, d(1, 0), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || len(rf) != 2 || rf[0] != (RefusalRow{"etabA", "t1", "", RefusalBudget, 1}) || rf[1] != (RefusalRow{"etabA", "t1", "p1", RefusalProfileWeek, 5}) {
		t.Fatalf("%+v %v", rf, err)
	}

	// Assignments in force at some time in the period.
	if err := st.AssignProfile(ctx, Assignment{User: "alice", Profile: "p1", From: d(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AssignProfile(ctx, Assignment{User: "alice", Profile: "p2", From: d(10, 0)}); err != nil { // ends p1, starts p2
		t.Fatal(err)
	}
	if err := st.AssignProfile(ctx, Assignment{User: "bob", Profile: "p1", From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AssignProfile(ctx, Assignment{User: "bob", Profile: "", From: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)}); err != nil { // ended before the period
		t.Fatal(err)
	}
	as, err := st.AssignmentsBetween(ctx, d(1, 0), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || len(as) != 2 || as[0].Profile != "p1" || as[1].Profile != "p2" {
		t.Fatalf("%+v %v", as, err)
	}
}

func TestQuotaReportStoreMemory(t *testing.T) { checkQuotaReportStore(t, NewMemory()) }

func TestQuotaReportStorePostgres(t *testing.T) {
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dropAll(t, p)
	p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	checkQuotaReportStore(t, p)
	// The marks of an earlier release become events when the schema is next
	// applied: the state that exists is not lost to the report.
	at := time.Date(2026, 12, 3, 9, 0, 0, 0, time.UTC)
	if _, err := p.pool.Exec(ctx, `INSERT INTO llmflow_warn_marks (user_name, win, threshold, instance, rolling, armed, fired_at) VALUES ('zed', 'week', 75, $1, false, false, $1)`, at); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ev, err := p.WarnEventsBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour), 10)
	if err != nil || len(ev) != 1 || ev[0].User != "zed" {
		t.Fatalf("backfill: %+v %v", ev, err)
	}
}
