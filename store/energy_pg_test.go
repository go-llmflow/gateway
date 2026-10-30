package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/energy"
)

// checkEnergyStore is the contract of EnergyStore, run against Memory and
// PostgreSQL alike.
func checkEnergyStore(t *testing.T, st interface {
	EnergyStore
	Store
}) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 3, 23, 59, 0, 0, time.UTC)
	rec := func(org, up string, p, c, cached int64, at time.Time, unit string) Record {
		return Record{Time: at, Org: org, Team: org, KeyID: "k", Model: "m", Pool: "p", Upstream: up, PromptTokens: p, CompletionTokens: c,
			CachedTokens: cached, Status: 200, Unit: unit}
	}
	if err := st.InsertRecords(ctx, []Record{
		rec("a", "http://n1:8000", 100, 10, 40, t0.Add(time.Second), ""),
		rec("a", "http://n1:8000", 50, 5, 0, t0.Add(2*time.Second), ""),
		rec("b", "http://n2:8000", 7, 7, 0, t0.Add(2*time.Second), ""),
		rec("b", "http://n1:8000", 9, 9, 0, t0.Add(3*time.Second), "USD"), // an external provider: no GPU
		rec("a", "http://n1:8000", 1000, 1000, 0, t0.Add(time.Hour), ""),  // another interval
	}); err != nil {
		t.Fatal(err)
	}
	work, err := st.WorkBetween(ctx, []string{"http://n1:8000"}, t0, t0.Add(time.Minute))
	if err != nil || len(work) != 1 || work[0].Org != "a" || work[0].Prefill != 60+50 || work[0].Decode != 15 {
		t.Fatalf("work %+v %v", work, err)
	}
	iv := EnergyInterval{Upstream: "n1", Start: t0, End: t0.Add(time.Minute), TotalMJ: 1000, IdleMJ: 100, GPUs: 2}
	act := []energy.Attributed{{Consumer: energy.Consumer{Org: "a", User: "u"}, MJ: 600}, {Consumer: energy.Consumer{Org: "b"}, MJ: 300}}
	if err := st.SaveEnergy(ctx, iv, act); err != nil {
		t.Fatal(err)
	}
	// Another replica's overlapping interval is refused, an adjacent one is not.
	over := iv
	over.Start, over.End = t0.Add(30*time.Second), t0.Add(90*time.Second)
	if err := st.SaveEnergy(ctx, over, nil); err != ErrConflict {
		t.Fatalf("overlap: %v", err)
	}
	next := EnergyInterval{Upstream: "n1", Start: t0.Add(time.Minute), End: t0.Add(2 * time.Minute), TotalMJ: 50, IdleMJ: 50}
	if err := st.SaveEnergy(ctx, next, nil); err != nil {
		t.Fatal(err)
	}
	if last, err := st.LastEnergy(ctx, "n1"); err != nil || !last.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("last %v %v", last, err)
	}
	if last, _ := st.LastEnergy(ctx, "nope"); !last.IsZero() {
		t.Fatalf("last of unknown %v", last)
	}
	// By day (UTC): the first interval starts on the 3rd (23:59), the adjacent one on the 4th.
	cells, idle, err := st.EnergyBetween(ctx, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, c := range cells {
		sum += c.MJ
		if c.Day != "2026-10-03" {
			t.Fatalf("cell day %s", c.Day)
		}
	}
	var idleSum int64
	for _, i := range idle {
		idleSum += i.MJ
	}
	if len(cells) != 2 || sum != 900 || idleSum != 150 || len(idle) != 2 {
		t.Fatalf("cells %+v idle %+v", cells, idle)
	}
	// A period that excludes the interval's start reads nothing of it.
	cells, idle, _ = st.EnergyBetween(ctx, t0.Add(time.Second), t0.Add(time.Hour))
	if len(cells) != 0 || len(idle) != 1 || idle[0].MJ != 50 {
		t.Fatalf("cells %+v idle %+v", cells, idle)
	}
}

func TestEnergyStoreMemory(t *testing.T) { checkEnergyStore(t, NewMemory()) }

func TestEnergyStorePostgres(t *testing.T) {
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
	checkEnergyStore(t, p)
}
