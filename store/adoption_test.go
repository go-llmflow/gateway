package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func checkAdoption(t *testing.T, st interface {
	AdoptionStore
	Store
}) {
	ctx := context.Background()
	wed := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC) // week of Monday 2026-10-12
	rec := func(org, user, model string, at time.Time) Record {
		return Record{Time: at, Org: org, Team: org, User: user, KeyID: "k", Model: model, Pool: "p", Status: 200}
	}
	if err := st.InsertRecords(ctx, []Record{
		rec("a", "alice", "m1", wed), rec("a", "alice", "m1", wed.Add(time.Hour)), rec("a", "bob", "m2", wed),
		rec("a", "", "m1", wed),                       // a service key: a request, no person
		rec("b", "carol", "m1", wed.AddDate(0, 0, 1)), // Thursday
		rec("a", "alice", "m1", wed.AddDate(0, 0, 6)), // the next Tuesday: next week
	}); err != nil {
		t.Fatal(err)
	}
	from, to := wed.Add(-24*time.Hour), wed.AddDate(0, 0, 10)
	a, err := st.Adoption(ctx, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Users != 3 || len(a.Daily) != 3 || a.Daily[0] != (AdoptionPoint{"2026-10-14", 2, 4}) || a.Daily[1].ActiveUsers != 1 {
		t.Fatalf("daily %+v users %d", a.Daily, a.Users)
	}
	if len(a.Weekly) != 2 || a.Weekly[0] != (AdoptionPoint{"2026-10-12", 3, 5}) || a.Weekly[1] != (AdoptionPoint{"2026-10-19", 1, 1}) {
		t.Fatalf("weekly %+v", a.Weekly)
	}
	if len(a.Models) != 2 || a.Models[0] != (AdoptionModel{"m1", 2, 5}) || a.Models[1] != (AdoptionModel{"m2", 1, 1}) {
		t.Fatalf("models %+v", a.Models)
	}
	// Restricted to one organisation, or to none.
	if a, _ = st.Adoption(ctx, from, to, []string{"b"}); a.Users != 1 || len(a.Daily) != 1 {
		t.Fatalf("org b: %+v", a)
	}
	if a, _ = st.Adoption(ctx, from, to, []string{}); a.Users != 0 || len(a.Daily) != 0 || len(a.Models) != 0 {
		t.Fatalf("no org: %+v", a)
	}
}

func TestAdoptionMemory(t *testing.T) { checkAdoption(t, NewMemory()) }

func TestAdoptionPostgres(t *testing.T) {
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
	checkAdoption(t, p)
}
