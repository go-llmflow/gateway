package gateway

import (
	"context"
	"encoding/csv"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

func TestMonthSpans(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
	got := monthSpans(d(2026, 10, 20), d(2026, 12, 5))
	want := [][2]time.Time{{d(2026, 10, 20), d(2026, 11, 1)}, {d(2026, 11, 1), d(2026, 12, 1)}, {d(2026, 12, 1), d(2026, 12, 5)}}
	if len(got) != 3 {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if !got[i][0].Equal(want[i][0]) || !got[i][1].Equal(want[i][1]) {
			t.Fatalf("%d: %v, want %v", i, got[i], want[i])
		}
	}
	if len(monthSpans(d(2026, 1, 1), d(2026, 1, 1))) != 0 {
		t.Fatal("empty period has spans")
	}
}

// One row per cost centre and month; project over lab over organisation;
// what has no code is UNMAPPED, unallocated energy UNALLOCATED; the file adds
// up to the reports; each reader sees what the report would show them.
func TestChargeback(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Orgs[0].CostCenter = "GL-A"
	cfg.Orgs[0].Labs = map[string]string{"etabA": "genomics"}
	cfg.Orgs[0].LabCostCenter = map[string]string{"genomics": "GL-A-GEN"}
	cfg.Orgs[0].ProjectCostCenter = map[[2]string]string{{"etabA", "p1"}: "GL-A-P1"}
	// etabB has no code. Fluids: 1 EUR per IT kWh.
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	e := newEnv(t, cfg)
	ctx := context.Background()
	oct, nov := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	rec := func(org, lab, project string, cost float64, at time.Time, unit string) store.Record {
		return store.Record{Time: at, Org: org, Team: org, Lab: lab, Project: project, KeyID: "k", Model: "llama", Pool: "b300", Cost: cost, Unit: unit, Status: 200}
	}
	if err := e.st.InsertRecords(ctx, []store.Record{
		rec("etabA", "", "", 1, oct, ""),
		rec("etabA", "genomics", "", 2, oct, ""),
		rec("etabA", "genomics", "p1", 4, oct, ""),
		rec("etabB", "", "", 8, oct, ""),
		rec("etabB", "", "", 16, nov, ""),
		rec("etabA", "", "", 0.5, oct, "USD"), // an external provider: its own currency, its own row
	}); err != nil {
		t.Fatal(err)
	}
	// Energy: 1 kWh used by etabA on the 15th, 1 kWh idle, shares 0.75/0.25 (testConfig).
	iv := store.EnergyInterval{Upstream: "n", Start: oct, End: oct.Add(time.Minute), TotalMJ: 2 * kWhMJ, IdleMJ: kWhMJ}
	if err := e.st.SaveEnergy(ctx, iv, []energy.Attributed{{Consumer: energy.Consumer{Org: "etabA", Lab: "genomics", Team: "etabA", Project: "p1"}, MJ: kWhMJ}}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	get := func(tok string) [][]string {
		resp := e.do("GET", "/admin/chargeback?from=2026-10-01&to=2026-12-01", tok, "")
		body := readAll(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
		if err != nil {
			t.Fatalf("%v\n%s", err, body)
		}
		if strings.Join(recs[0], ",") != strings.Join(chargebackHeader, ",") {
			t.Fatalf("header %v", recs[0])
		}
		return recs[1:]
	}
	index := func(rows [][]string) map[string][]string {
		m := map[string][]string{}
		for _, r := range rows {
			m[r[0]+"|"+r[2]+"|"+r[8]] = r
		}
		return m
	}
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	rows := get(fin)
	m := index(rows)
	// Row key: period start | cost centre | provider currency. gpu, kwh, cost columns: 3, 4, 5.
	check := func(key string, gpu, kwh, cost float64) {
		t.Helper()
		r := m[key]
		if r == nil {
			t.Fatalf("no row %q in %v", key, rows)
		}
		if !near(num(r[3]), gpu) || !near(num(r[4]), kwh) || !near(num(r[5]), cost) {
			t.Fatalf("%s: %v, want gpu %v kwh %v cost %v", key, r, gpu, kwh, cost)
		}
	}
	check("2026-10-01|GL-A|", 1, 0, 0)
	check("2026-10-01|GL-A-GEN|", 2, 0, 0)
	// Active 1 kWh on the project, and etabA's 0.75 of the 1 kWh idle (by share).
	check("2026-10-01|GL-A-P1|", 4, 1.75, 1.75)
	check("2026-10-01|UNMAPPED|", 8, 0.25, 0.25) // etabB: its 0.25 of the idle energy, no code
	check("2026-11-01|UNMAPPED|", 16, 0, 0)
	check("2026-10-01|GL-A|USD", 0, 0, 0)
	if r := m["2026-10-01|GL-A|USD"]; r == nil || num(r[7]) != 0.5 || r[8] != "USD" {
		t.Fatalf("provider row %v", r)
	}
	// It adds up to the energy measured (2 kWh) and the GPU-hours recorded (31).
	var gpu, kwh float64
	for _, r := range rows {
		gpu += num(r[3])
		kwh += num(r[4])
	}
	if !near(gpu, 31) || !near(kwh, 2) {
		t.Fatalf("totals gpu %v kwh %v", gpu, kwh)
	}
	// An organisation's admin gets its own codes only.
	own := get(oa)
	for _, r := range own {
		if r[2] == CostCenterUnmapped || r[2] == CostCenterUnallocated {
			t.Fatalf("org admin sees %v", r)
		}
	}
	if len(own) == 0 {
		t.Fatal("org admin sees nothing of its own")
	}
	if code := e.do("GET", "/admin/chargeback?from=2026-10-01&to=2026-12-01&format=xml", fin, "").StatusCode; code != 400 {
		t.Fatalf("format: %d", code)
	}
}
