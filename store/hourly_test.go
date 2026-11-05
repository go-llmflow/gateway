package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/energy"
)

// checkEnergyHourly: EnergyHourly is EnergyBetween by UTC hour (a time-of-use
// tariff prices each hour), the intervals carry what was learnt beside the
// GPUs, and the preemption measurement sums them per month and pool.
func checkEnergyHourly(t *testing.T, st interface {
	EnergyStore
	Store
}) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 3, 9, 50, 0, 0, time.UTC)
	save := func(start time.Time, mj, idle int64, org, pool string, pre int64) {
		iv := EnergyInterval{Upstream: "n", Start: start, End: start.Add(10 * time.Minute), TotalMJ: mj, IdleMJ: idle, Pool: pool,
			OverheadMJ: 7, OverheadSource: "redfish", Preemptions: pre}
		var act []energy.Attributed
		if mj-idle > 0 {
			act = []energy.Attributed{{Consumer: energy.Consumer{Org: org}, MJ: mj - idle}}
		}
		if err := st.SaveEnergy(ctx, iv, act); err != nil {
			t.Fatal(err)
		}
	}
	save(t0, 100, 10, "a", "p", 0)                      // 09:50 (hour 9)
	save(t0.Add(15*time.Minute), 200, 20, "a", "p", 3)  // 10:05 (hour 10)
	save(t0.Add(30*time.Minute), 400, 40, "b", "p", -1) // 10:20 (hour 10), counter unread
	save(t0.AddDate(0, 0, 1), 800, 80, "a", "p", 2)     // next day 09:50

	day, dayIdle, err := st.EnergyBetween(ctx, t0.Add(-time.Hour), t0.AddDate(0, 0, 3))
	if err != nil || len(day) != 3 { // a on the 3rd, b on the 3rd, a on the 4th
		t.Fatalf("by day: %+v %v", day, err)
	}
	for _, c := range day {
		if !c.At.IsZero() {
			t.Fatalf("a day cell carries an hour: %+v", c)
		}
	}
	hr, hrIdle, err := st.EnergyHourly(ctx, t0.Add(-time.Hour), t0.AddDate(0, 0, 3))
	if err != nil || len(hr) != 4 { // a@9, a@10, b@10, a@next 9
		t.Fatalf("by hour: %+v %v", hr, err)
	}
	var sumDay, sumHr int64
	for _, c := range day {
		sumDay += c.MJ
	}
	for _, c := range hr {
		sumHr += c.MJ
		if c.At.Minute() != 0 || c.At.Location() != time.UTC {
			t.Fatalf("an hourly cell is not at a UTC hour: %+v", c)
		}
	}
	if sumDay != sumHr || sumDay != 90+180+360+720 {
		t.Fatalf("the hours add up to %d, the days to %d", sumHr, sumDay)
	}
	var idleDay, idleHr int64
	for _, i := range dayIdle {
		idleDay += i.MJ
	}
	for _, i := range hrIdle {
		idleHr += i.MJ
		if i.At.IsZero() {
			t.Fatalf("an hourly idle row has no hour: %+v", i)
		}
	}
	if idleDay != idleHr || idleDay != 10+20+40+80 || len(hrIdle) != 3 { // the two intervals of hour 10 share a row
		t.Fatalf("idle by hour %d / by day %d: %+v", idleHr, idleDay, hrIdle)
	}

	oh, err := st.OverheadBetween(ctx, t0.Add(-time.Hour), t0.AddDate(0, 0, 3))
	if err != nil || len(oh) != 1 || oh[0].Month != "2026-10" || oh[0].Pool != "p" || oh[0].Source != "redfish" || oh[0].Intervals != 4 || oh[0].MJ != 28 {
		t.Fatalf("overhead %+v %v", oh, err)
	}
	rows, err := st.PreemptionBetween(ctx, t0.Add(-time.Hour), t0.AddDate(0, 0, 3))
	if err != nil || len(rows) != 1 {
		t.Fatalf("preemptions %+v %v", rows, err)
	}
	r := rows[0]
	if r.Month != "2026-10" || r.Pool != "p" || r.Intervals != 4 || r.Known != 3 || r.WithPreemptions != 2 || r.Preemptions != 5 ||
		r.TotalMJ != 100+200+400+800 || r.PreemptedMJ != 200+800 {
		t.Fatalf("%+v", r)
	}
}

func TestEnergyHourlyMemory(t *testing.T) { checkEnergyHourly(t, NewMemory()) }

func TestEnergyHourlyPostgres(t *testing.T) {
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
	checkEnergyHourly(t, p)
}
