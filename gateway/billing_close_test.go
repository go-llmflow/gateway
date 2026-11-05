package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
)

type cbOut struct {
	Rows   []ChargebackRow `json:"rows"`
	Method billingMethod   `json:"method"`
	Closed []closedRef     `json:"closed_months"`
}

func chargebackJSON(t *testing.T, e *env, from, to string) cbOut {
	t.Helper()
	resp := e.do("GET", "/admin/chargeback?format=json&from="+from+"&to="+to, master, "")
	body := readAll(t, resp)
	var o cbOut
	if err := json.Unmarshal([]byte(body), &o); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s %v", resp.StatusCode, body, err)
	}
	return o
}

func totals(o cbOut) (total, adj int64, byCC map[string]int64) {
	byCC = map[string]int64{}
	for _, r := range o.Rows {
		total += r.TotalCostMinor
		adj += r.AdjustmentMinor
		byCC[r.CostCenter] += r.TotalCostMinor
	}
	return
}

// Close a month, change the PUE: the closed month does not move, the change
// is booked as an adjustment on the next open month, and the totals reconcile
// (what was billed plus the adjustment is what the live computation gives).
func TestCloseMonthFreezesAndAdjustsTheNextOne(t *testing.T) {
	e := energyEnv(t, energy.IdleByShare)
	e.srv.cfg.Orgs[0].CostCenter, e.srv.cfg.Orgs[1].CostCenter = "GL-A", "GL-B"
	e.srv.now = func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) }
	saveEnergy(t, e, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), kWhMJ, 0, "etabA")
	saveEnergy(t, e, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), 3*kWhMJ, 0, "etabB")

	live := chargebackJSON(t, e, "2026-10-01", "2026-11-01")
	if tot, _, _ := totals(live); tot != 400 || len(live.Closed) != 0 {
		t.Fatalf("live October: %d %+v", tot, live.Closed)
	}

	// Refusals: the current month, a month that does not exist, and a caller
	// that is not a proxy admin.
	if resp := e.do("POST", "/admin/billing/close?month=2026-11", master, ""); resp.StatusCode != 400 {
		t.Fatalf("the current month: %d", resp.StatusCode)
	}
	if resp := e.do("POST", "/admin/billing/close?month=2027-01", master, ""); resp.StatusCode != 400 {
		t.Fatalf("a future month: %d", resp.StatusCode)
	}
	if resp := e.do("POST", "/admin/billing/close?month=2026-13", master, ""); resp.StatusCode != 400 {
		t.Fatalf("a bad month: %d", resp.StatusCode)
	}
	_, key := e.newKey("etabA")
	if resp := e.do("POST", "/admin/billing/close?month=2026-10", key, ""); resp.StatusCode == 200 {
		t.Fatal("a team key closed a month")
	}

	resp := e.do("POST", "/admin/billing/close?month=2026-10", master, "")
	if body := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(body, `"checksum"`) {
		t.Fatalf("close: %d %s", resp.StatusCode, body)
	}
	if resp := e.do("POST", "/admin/billing/close?month=2026-10", master, ""); resp.StatusCode != 409 {
		t.Fatalf("closing twice: %d", resp.StatusCode)
	}
	// The close is in the audit log.
	if a := readAll(t, e.do("GET", "/admin/audit?limit=200", master, "")); !strings.Contains(a, "billing.close") {
		t.Fatalf("the close is not audited: %s", a)
	}

	// A corrected PUE: the live computation of October is now twice as much,
	// the closed month is what was billed.
	e.srv.cfg.Fluids.PUE = 2
	closed := chargebackJSON(t, e, "2026-10-01", "2026-11-01")
	if tot, adj, _ := totals(closed); tot != 400 || adj != 0 || len(closed.Closed) != 1 || closed.Closed[0].Month != "2026-10" ||
		closed.Method.MethodVersion != BillingMethodVersion {
		t.Fatalf("closed October moved: %d %d %+v", tot, adj, closed.Closed)
	}
	// ... and a window that is not the whole month is computed live.
	if part := chargebackJSON(t, e, "2026-10-01", "2026-10-16"); len(part.Closed) != 0 {
		t.Fatalf("a part of a closed month read the snapshot: %+v", part.Closed)
	}

	// November (open) carries the difference: +1.00 and +3.00, a credit note
	// would be negative.
	nov := chargebackJSON(t, e, "2026-11-01", "2026-12-01")
	tot, adj, byCC := totals(nov)
	if adj != 400 || tot != 400 || byCC["GL-A"] != 100 || byCC["GL-B"] != 300 {
		t.Fatalf("November: total %d adjustment %d %v", tot, adj, byCC)
	}
	// What was billed plus the adjustment is the live computation of October.
	if bill, _, _ := totals(closed); bill+adj != 800 {
		t.Fatalf("does not reconcile: %d + %d != 800", bill, adj)
	}

	// Close November in December: it keeps its adjustment, and December does
	// not book it a second time.
	e.srv.now = func() time.Time { return time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC) }
	if resp := e.do("POST", "/admin/billing/close?month=2026-11", master, ""); resp.StatusCode != 200 {
		t.Fatalf("close November: %d", resp.StatusCode)
	}
	nov2 := chargebackJSON(t, e, "2026-11-01", "2026-12-01")
	if _, adj2, _ := totals(nov2); adj2 != 400 || len(nov2.Closed) != 1 {
		t.Fatalf("closed November: adjustment %d %+v", adj2, nov2.Closed)
	}
	dec := chargebackJSON(t, e, "2026-12-01", "2027-01-01")
	if tot, adj3, _ := totals(dec); adj3 != 0 || tot != 0 {
		t.Fatalf("December books October's adjustment again: %d %d", tot, adj3)
	}
	// The listing and one snapshot.
	if l := readAll(t, e.do("GET", "/admin/billing/closed", master, "")); !strings.Contains(l, "2026-10") || !strings.Contains(l, "2026-11") {
		t.Fatalf("list: %s", l)
	}
	snap := readAll(t, e.do("GET", "/admin/billing/closed?month=2026-10", master, ""))
	if !strings.Contains(snap, `"pue":1`) || !strings.Contains(snap, `"method_version":2`) {
		t.Fatalf("the snapshot does not carry its parameters: %.300s", snap)
	}
}

// The method is stamped on every report, in a header for a CSV.
func TestReportsStampTheirMethod(t *testing.T) {
	e := energyEnv(t, energy.IdleByUsage)
	e.srv.cfg.Fluids.FixedCosts = []config.FixedCost{{Name: "racks", Monthly: 10, Charge: config.ChargeAll, Basis: config.BasisEnergy}}
	for _, path := range []string{"/admin/fluids?from=2026-10-01&to=2026-11-01&format=csv", "/admin/chargeback?from=2026-10-01&to=2026-11-01"} {
		resp := e.do("GET", path, master, "")
		readAll(t, resp)
		if resp.Header.Get("X-Llmflow-Method-Version") != "2" || resp.Header.Get("X-Llmflow-Gateway-Version") == "" ||
			!strings.Contains(resp.Header.Get("X-Llmflow-Method"), `"racks"`) || !strings.Contains(resp.Header.Get("X-Llmflow-Method"), `"idle_energy":"by_usage"`) {
			t.Errorf("%s: %v", path, resp.Header)
		}
	}
}

func TestParseBillingMonth(t *testing.T) {
	for _, ok := range []string{"2026-10", "2000-01", "2099-12"} {
		if _, err := ParseBillingMonth(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "2026-13", "2026-00", "1999-12", "2100-01", "2026-1", "2026-10-01", " 2026-10", "2026-10\n", "２０２６-10"} {
		if _, err := ParseBillingMonth(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzParseBillingMonth(f *testing.F) {
	for _, s := range []string{"2026-10", "", "2026-13", "2026-10\n", "9999-99", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseBillingMonth(s)
		if err != nil {
			return
		}
		if m.Format("2006-01") != s || m.Day() != 1 || m.Location() != time.UTC {
			t.Fatalf("%q parsed to %v", s, m)
		}
	})
}

// However a month is cut, the parts of a fixed cost add up to the whole month,
// per organisation, and a whole month is the configured amount.
func FuzzFixedCostParts(f *testing.F) {
	f.Add(int64(180001), int64(1), int64(40), int64(17), int64(6), int64(3))
	f.Add(int64(1), int64(86399), int64(1), int64(1), int64(1), int64(1))
	f.Fuzz(func(t *testing.T, amount, cut1, cut2, wa, wb, wc int64) {
		if amount < 1 || amount > 1e12 {
			return
		}
		for _, w := range []int64{wa, wb, wc} {
			if w < 0 || w > 1e9 {
				return
			}
		}
		oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		end := oct.AddDate(0, 1, 0)
		span := int64(end.Sub(oct) / time.Second)
		c1, c2 := oct.Add(time.Duration(((cut1%span)+span)%span)*time.Second), oct.Add(time.Duration(((cut2%span)+span)%span)*time.Second)
		if c2.Before(c1) {
			c1, c2 = c2, c1
		}
		f := &config.Fluids{PUE: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByUsage,
			FixedCosts: []config.FixedCost{{Name: "x", Monthly: float64(amount) / 100, Charge: config.ChargeAll, Basis: config.BasisEnergy}}}
		cells := func() []fluidsCell {
			c := func(org string, mj int64) fluidsCell {
				return fluidsCell{Day: "2026-10-05", Consumer: energy.Consumer{Org: org, User: org}, ActiveMJ: mj}
			}
			return []fluidsCell{c("a", wa), c("b", wb), c("c", wc)}
		}
		shares := func(string) map[string]float64 { return map[string]float64{"a": 1, "b": 1, "c": 1} }
		env := envOf(shares, cells())
		whole, n := fixedByOrg(allocateFixedCosts(cells(), f, oct, end, env))
		if n != amount {
			t.Fatalf("the month is %d, want %d", n, amount)
		}
		sum := map[string]int64{}
		for _, w := range [][2]time.Time{{oct, c1}, {c1, c2}, {c2, end}} {
			if !w[0].Before(w[1]) {
				continue
			}
			part, _ := fixedByOrg(allocateFixedCosts(cells(), f, w[0], w[1], env))
			for o, v := range part {
				sum[o] += v
			}
		}
		for o, v := range whole {
			if sum[o] != v {
				t.Fatalf("%s: parts %d, month %d", o, sum[o], v)
			}
		}
	})
}
