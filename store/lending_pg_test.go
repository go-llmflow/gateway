package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func checkLendingStore(t *testing.T, st interface {
	LendingStore
	Store
}) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rec := func(org string, at time.Time, status int, unit string, borrow bool) Record {
		return Record{Time: at, Org: org, Team: org, KeyID: "k", Model: "m", Pool: "p", Status: status, Unit: unit, Borrowing: borrow, Cost: 2, LatencyMS: 1500}
	}
	if err := st.InsertRecords(ctx, []Record{
		rec("a", t0, 200, "", false),
		rec("b", t0.Add(time.Second), 200, "", true),
		rec("b", t0.Add(2*time.Second), 429, "", false),    // refused: not served
		rec("b", t0.Add(3*time.Second), 200, "USD", false), // a provider: no GPU
		rec("a", t0.Add(48*time.Hour), 200, "", false),     // after the period
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LendingRequests(ctx, t0, t0.Add(time.Hour), 10)
	if err != nil || len(rows) != 2 || rows[0].Org != "a" || !rows[1].Borrowing || rows[1].Cost != 2 || rows[1].LatencyMS != 1500 {
		t.Fatalf("%+v %v", rows, err)
	}
	if _, err := st.LendingRequests(ctx, t0, t0.Add(time.Hour), 1); err != ErrTooMany {
		t.Fatalf("bound: %v", err)
	}
}

func TestLendingStoreMemory(t *testing.T) { checkLendingStore(t, NewMemory()) }

func TestLendingStorePostgres(t *testing.T) {
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
	checkLendingStore(t, p)
}
