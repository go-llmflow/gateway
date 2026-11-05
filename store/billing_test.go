package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// checkBillingStore is the contract of BillingStore and MeterStore, run
// against Memory and PostgreSQL alike.
func checkBillingStore(t *testing.T, st interface {
	BillingStore
	MeterStore
	Store
}) {
	ctx := context.Background()

	// Closed months: stored once, read back byte for byte, never replaced.
	if _, ok, err := st.ClosedMonth(ctx, "2026-10"); ok || err != nil {
		t.Fatalf("an open month: %v %v", ok, err)
	}
	payload := []byte(`{"b":1.10,  "a":[1,2,3]}`) // bytes, not JSON: no normalisation
	c := ClosedMonth{Month: "2026-10", ClosedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), ClosedBy: "admin", Checksum: "abc", Payload: payload}
	if err := st.CloseMonth(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.ClosedMonth(ctx, "2026-10")
	if err != nil || !ok || string(got.Payload) != string(payload) || got.Checksum != "abc" || got.ClosedBy != "admin" || !got.ClosedAt.Equal(c.ClosedAt) {
		t.Fatalf("read back: %+v %v %v", got, ok, err)
	}
	c.Payload = []byte(`{"other":true}`)
	if err := st.CloseMonth(ctx, c); !errors.Is(err, ErrConflict) {
		t.Fatalf("closing twice: %v", err)
	}
	again, _, _ := st.ClosedMonth(ctx, "2026-10")
	if string(again.Payload) != string(payload) {
		t.Fatalf("the closed month changed: %s", again.Payload)
	}
	if err := st.CloseMonth(ctx, ClosedMonth{Month: "2026-09", ClosedAt: c.ClosedAt, ClosedBy: "admin", Checksum: "z", Payload: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if ms, err := st.ClosedMonths(ctx); err != nil || len(ms) != 2 || ms[0] != "2026-09" || ms[1] != "2026-10" {
		t.Fatalf("months %v %v", ms, err)
	}

	// Network: the bytes of the records that finished in the window.
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	rec := func(id, org string, at time.Time, in, out int64) Record {
		return Record{ID: id, Time: at, Org: org, Team: org, KeyID: "k", Model: "m", Pool: "p", Status: 200, BytesIn: in, BytesOut: out}
	}
	if err := st.InsertRecords(ctx, []Record{
		rec("n1", "a", t0, 100, 1000), rec("n2", "a", t0.Add(time.Hour), 50, 500), rec("n3", "b", t0, 7, 70),
		rec("n4", "a", t0.AddDate(0, 0, 2), 9, 9), rec("n5", "c", t0, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	net, err := st.NetworkBetween(ctx, t0.Add(-time.Hour), t0.AddDate(0, 0, 1))
	if err != nil || len(net) != 2 {
		t.Fatalf("network %+v %v", net, err)
	}
	for _, n := range net {
		if n.Day != "2026-10-05" || n.Org == "a" && (n.In != 150 || n.Out != 1500) || n.Org == "b" && (n.In != 7 || n.Out != 70) {
			t.Fatalf("network cell %+v", n)
		}
	}

	// Storage: end-of-day totals, cumulative over the days (a usage row and
	// the request-log objects), carried into a window that starts later.
	if err := st.AddStorage(ctx, "2026-10-04", "a", 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.AddStorage(ctx, "2026-10-04", "a", 500); err != nil { // the same day adds up
		t.Fatal(err)
	}
	if err := st.AddStorage(ctx, "2026-10-06", "b", 200); err != nil {
		t.Fatal(err)
	}
	cells, err := st.StorageBetween(ctx, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	for _, c := range cells {
		by[c.Day+"|"+c.Org] = c.Bytes
	}
	// a: 1500 of log objects from the 4th, plus its usage rows from the 5th (the
	// size of a row is the store's: at least a byte); b: 200 from the 6th, plus
	// its row from the 5th.
	if by["2026-10-04|a"] != 1500 || by["2026-10-05|a"] <= 1500 || by["2026-10-06|a"] < by["2026-10-05|a"] || by["2026-10-04|b"] != 0 ||
		by["2026-10-05|b"] <= 0 || by["2026-10-06|b"] <= by["2026-10-05|b"]+199 {
		t.Fatalf("storage %v", by)
	}
	// A window that starts after the writes still carries what was stored.
	late, err := st.StorageBetween(ctx, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	if err != nil || len(late) < 2 {
		t.Fatalf("late %+v %v", late, err)
	}
	for _, c := range late {
		if c.Day != "2026-10-20" || c.Bytes < 1000 && c.Org == "a" {
			t.Fatalf("late cell %+v", c)
		}
	}
}

func TestBillingStoreMemory(t *testing.T) { checkBillingStore(t, NewMemory()) }

func TestBillingStorePostgres(t *testing.T) {
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
	checkBillingStore(t, p)
	// A closed month is immutable even for the database's own user.
	if _, err := p.pool.Exec(ctx, `UPDATE llmflow_billing_closed SET checksum = 'x'`); err == nil {
		t.Fatal("a closed month was updated")
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM llmflow_billing_closed`); err == nil {
		t.Fatal("a closed month was deleted")
	}
	if _, err := p.pool.Exec(ctx, `TRUNCATE llmflow_billing_closed`); err == nil {
		t.Fatal("the closed months were truncated")
	}
	if ms, err := p.ClosedMonths(ctx); err != nil || len(ms) != 2 {
		t.Fatalf("the closed months after the refused writes: %v %v", ms, err)
	}
}
