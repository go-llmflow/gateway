package gateway

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

func TestMinorUnits(t *testing.T) {
	for _, c := range []struct {
		v    float64
		dec  int
		want int64
	}{
		{0.125, 2, 12}, {0.135, 2, 14}, {0.145, 2, 14}, {2.5, 0, 2}, {3.5, 0, 4}, {-0.125, 2, -12}, {0.0004, 2, 0}, {1.2345, 3, 1234}, {1.0, 2, 100},
	} {
		// 0.135 and 0.145 are not exact in binary: the expectation is what half-even gives on the float.
		if got := minorUnits(c.v, c.dec); got != c.want {
			t.Errorf("minorUnits(%v, %d) = %d, want %d", c.v, c.dec, got, c.want)
		}
	}
	for _, c := range []struct {
		m    int64
		dec  int
		want string
	}{{5, 2, "0.05"}, {-5, 2, "-0.05"}, {123, 0, "123"}, {7, 3, "0.007"}, {12345, 2, "123.45"}, {0, 2, "0.00"}} {
		if got := formatMinor(c.m, c.dec); got != c.want {
			t.Errorf("formatMinor(%d, %d) = %q, want %q", c.m, c.dec, got, c.want)
		}
	}
}

func energyEnv(t *testing.T, mode string) *env {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: mode, WPrefill: 1, WDecode: 1}
	return newEnv(t, cfg)
}

func saveEnergy(t *testing.T, e *env, at time.Time, total, idle int64, org string) {
	t.Helper()
	var act []energy.Attributed
	if total-idle > 0 {
		act = []energy.Attributed{{Consumer: energy.Consumer{Org: org, Team: org}, MJ: total - idle}}
	}
	if err := e.st.SaveEnergy(context.Background(), store.EnergyInterval{Upstream: "n", Start: at, End: at.Add(time.Minute), TotalMJ: total, IdleMJ: idle}, act); err != nil {
		t.Fatal(err)
	}
}

func fluidsByOrg(t *testing.T, e *env, from, to string) map[string]float64 {
	t.Helper()
	resp := e.do("GET", "/admin/fluids?group_by=org&from="+from+"&to="+to, master, "")
	body := readAll(t, resp)
	var a struct {
		Rows []struct {
			Org string
			KWh float64
		}
	}
	if err := json.Unmarshal([]byte(body), &a); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	out := map[string]float64{}
	for _, r := range a.Rows {
		out[r.Org] += r.KWh
	}
	return out
}

// A period that ends in the middle of a day still has that morning's energy,
// in the chargeback as in /admin/fluids (#110: the day-string comparison
// dropped it).
func TestChargebackKeepsEnergyWhenToIsNotMidnight(t *testing.T) {
	e := energyEnv(t, energy.IdleByShare)
	saveEnergy(t, e, time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC), 2*kWhMJ, 0, "etabA")
	saveEnergy(t, e, time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC), 4*kWhMJ, 0, "etabA") // after the end
	resp := e.do("GET", "/admin/chargeback?from=2026-10-01&to=2026-10-15T12:00:00Z", master, "")
	recs, err := csv.NewReader(strings.NewReader(readAll(t, resp))).ReadAll()
	if err != nil || len(recs) < 2 {
		t.Fatalf("%v %v", recs, err)
	}
	var kwh float64
	for _, r := range recs[1:] {
		v, _ := strconv.ParseFloat(r[4], 64)
		kwh += v
	}
	fl := fluidsByOrg(t, e, "2026-10-01", "2026-10-15T12:00:00Z")
	if !near(kwh, 2) || !near(fl["etabA"], 2) {
		t.Fatalf("chargeback %v kWh, fluids %v, want 2 and 2", kwh, fl)
	}
}

// With by_usage, the parts of a month add up to the month: the weights are
// the whole month's usage whatever window is asked (#110).
func TestByUsageIdleIsDecidedOnTheWholeMonth(t *testing.T) {
	e := energyEnv(t, energy.IdleByUsage)
	// 5 Oct: A active 3, idle 1. 20 Oct: B active 1, idle 1 (units of 1 kWh/10).
	u := int64(kWhMJ / 10)
	saveEnergy(t, e, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), 4*u, 1*u, "etabA")
	saveEnergy(t, e, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), 2*u, 1*u, "etabB")
	month := fluidsByOrg(t, e, "2026-10-01", "2026-11-01")
	h1 := fluidsByOrg(t, e, "2026-10-01", "2026-10-16")
	h2 := fluidsByOrg(t, e, "2026-10-16", "2026-11-01")
	for _, org := range []string{"etabA", "etabB"} {
		if !near(h1[org]+h2[org], month[org]) {
			t.Errorf("%s: %.6f + %.6f != %.6f kWh", org, h1[org], h2[org], month[org])
		}
	}
	// The month: usage 3:1, so the 2 units of idle are split 1.5 and 0.5.
	if !near(month["etabA"], 0.3+0.15) || !near(month["etabB"], 0.1+0.05) {
		t.Fatalf("month %v", month)
	}
}

// Rows of the file add up to its total in integers: each row is rounded once.
func TestChargebackMoneyIsMinorUnits(t *testing.T) {
	e := energyEnv(t, energy.IdleByShare)
	e.srv.cfg.Fluids.PricePerKWh = 0.333333
	e.srv.cfg.Orgs[0].CostCenter, e.srv.cfg.Orgs[1].CostCenter = "GL-A", "GL-B"
	// 1 kWh for each of two cost centres at 0.333333 per kWh: 0.33 each, and the
	// file's total is the sum of its rows (0.66), not the rounding of 0.666666.
	saveEnergy(t, e, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), kWhMJ, 0, "etabA")
	saveEnergy(t, e, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), kWhMJ, 0, "etabB")
	resp := e.do("GET", "/admin/chargeback?from=2026-10-01&to=2026-11-01&format=json", master, "")
	var a struct {
		Rows []ChargebackRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(readAll(t, resp)), &a); err != nil || len(a.Rows) == 0 {
		t.Fatalf("%v %+v", err, a)
	}
	var sum int64
	for _, r := range a.Rows {
		if r.FluidsCostMinor != 33 || r.FluidsCost != "0.33" {
			t.Fatalf("row %+v", r)
		}
		sum += r.FluidsCostMinor
	}
	if sum != 33*int64(len(a.Rows)) {
		t.Fatal(sum)
	}
}

// Small cells are not shown: one person is never a figure.
func TestAdoptionSmallCells(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL())) // the default threshold: 5
	at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	var recs []store.Record
	add := func(user, model string, day int) {
		recs = append(recs, store.Record{Time: at.AddDate(0, 0, day), Org: "etabA", Team: "etabA", User: user, KeyID: "k", Model: model, Pool: "b300", Status: 200})
	}
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5"} { // day 0: five people on model m1
		add(u, "m1", 0)
	}
	add("u1", "m2", 1) // day 1: one person; models m2 and m3 each used by one
	add("u1", "m3", 1)
	if err := e.st.InsertRecords(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	resp := e.do("GET", "/admin/adoption?from=2026-10-01&to=2026-11-01", master, "")
	body := readAll(t, resp)
	var a struct {
		Active *int64 `json:"active_users"`
		Daily  []struct {
			Period     string
			Users      *int64 `json:"active_users"`
			Suppressed bool
		}
		Models []struct {
			Model      string
			Users      *int64 `json:"active_users"`
			Requests   *int64
			Suppressed bool
		}
		Min int `json:"min_users"`
	}
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	// Day 1 (one person) is hidden. Showing day 0 (5 requests) next to the week
	// (7 requests) would give day 1 by subtraction (7 - 5 = u1's 2 requests),
	// so day 0 is hidden too (secondary suppression).
	if a.Min != 5 || a.Active == nil || *a.Active != 5 || len(a.Daily) != 2 || !a.Daily[0].Suppressed || a.Daily[0].Users != nil || !a.Daily[1].Suppressed || a.Daily[1].Users != nil {
		t.Fatalf("daily: %s", body)
	}
	// m2 and m3 (1 each) are the SAME person (u1): one person, so m1 (5 people)
	// is hidden with them, or the period's 7 requests minus m1's 5 would be u1's.
	// The merged row then stands for 5 distinct people.
	if len(a.Models) != 1 || a.Models[0].Model != OtherModels || a.Models[0].Suppressed || a.Models[0].Users == nil || *a.Models[0].Users != 5 || *a.Models[0].Requests != 7 {
		t.Fatalf("models: %s", body)
	}
	if strings.Contains(body, `"m2"`) || strings.Contains(body, `"m3"`) {
		t.Fatalf("a suppressed model is named: %s", body)
	}
	// The opposite case: five more single-person models of five DIFFERENT
	// people join m2 and m3 in the merged row, whose union is 6 people: shown.
	var more []store.Record
	for i, u := range []string{"w1", "w2", "w3", "w4", "w5"} {
		more = append(more, store.Record{Time: at.AddDate(0, 0, 2), Org: "etabA", Team: "etabA", User: u, KeyID: "k", Model: "solo" + strconv.Itoa(i), Pool: "b300", Status: 200})
	}
	if err := e.st.InsertRecords(context.Background(), more); err != nil {
		t.Fatal(err)
	}
	body = readAll(t, e.do("GET", "/admin/adoption?from=2026-10-01&to=2026-11-01", master, ""))
	a.Models = nil
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	last := a.Models[len(a.Models)-1]
	if last.Model != OtherModels || last.Suppressed || last.Users == nil || *last.Users != 6 || last.Requests == nil || *last.Requests != 7 {
		t.Fatalf("merged row of 6 distinct people (u1, w1..w5), 7 requests: %s", body)
	}
}
