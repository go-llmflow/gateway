package gateway

import (
	"math"
	"sort"
	"testing"
	"time"
)

func octoberSpans() [][2]time.Time {
	return monthSpans(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
}

func pctIs(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// Hand-computed, October 2026 (31 days), GPU-hours in milli-GPU-hours:
//
//	CC-A  org budget 1000/month        -> 1000.000 (overridden by the team budget)
//	      team budget 600/week         -> 600*31/7 = 2657.142857 -> 2657.143
//	      two allowances of 100/week   -> 200*31/7 =  885.714286 ->  885.714
//	      actual 3000                  -> 3000.000
//	      against budget: +342.857 = 342857/2657143 = 12.9 % (under 15: not flagged by it)
//	      against estimate: +2114.286 = 238.7 -> 23.9 %  (flagged)
//	CC-B  budget 100/month, actual 150: +50.000, +50.0 %: planted overrun, flagged
//	CC-C  actual 12.5, no budget, no estimate: shown, no variance, not flagged
//	CC-D  budget 100/month, no spend: actual 0, -100.0 %, flagged
func TestVarianceGolden(t *testing.T) {
	in := varianceInputs{
		Months: octoberSpans(),
		Budgets: []varianceLine{
			{CostCenter: "CC-A", Amount: 1000},
			{CostCenter: "CC-A", Amount: 600, PeriodDays: 7, Team: true},
			{CostCenter: "CC-B", Amount: 100},
			{CostCenter: "CC-D", Amount: 100},
		},
		Allowances: []varianceLine{
			{CostCenter: "CC-A", Amount: 100, PeriodDays: 7},
			{CostCenter: "CC-A", Amount: 100, PeriodDays: 7},
		},
		Actual: map[varianceKey]float64{
			{"2026-10", "CC-A", ""}: 3000,
			{"2026-10", "CC-B", ""}: 150,
			{"2026-10", "CC-C", ""}: 12.5,
		},
		Threshold: 15,
	}
	rows := buildVariance(in)
	if len(rows) != 4 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	a, b, c, d := rows[0], rows[1], rows[2], rows[3]
	if a.CostCenter != "CC-A" || a.BudgetedMinor != 2657143 || a.EstimatedMinor != 885714 || a.ActualMinor != 3000000 {
		t.Errorf("CC-A: %+v", a)
	}
	if a.BudgetVarMinor != 342857 || a.EstimateVarMinor != 2114286 {
		t.Errorf("CC-A variance: %+v", a)
	}
	pctIs(t, "CC-A budget %", a.BudgetVarPct, 12.9)
	pctIs(t, "CC-A estimate %", a.EstimateVarPct, 238.7)
	if !a.OverThreshold {
		t.Error("CC-A: the estimate is 238 % over, must be flagged")
	}
	if b.CostCenter != "CC-B" || b.BudgetedMinor != 100000 || b.ActualMinor != 150000 || b.BudgetVarMinor != 50000 || !b.OverThreshold {
		t.Errorf("planted overrun CC-B: %+v", b)
	}
	pctIs(t, "CC-B %", b.BudgetVarPct, 50)
	if c.CostCenter != "CC-C" || c.HasBudget || c.HasEstimate || c.OverThreshold || c.BudgetVarPct != nil || c.ActualMinor != 12500 {
		t.Errorf("CC-C: %+v", c)
	}
	if d.CostCenter != "CC-D" || d.ActualMinor != 0 || d.BudgetVarMinor != -100000 || !d.OverThreshold {
		t.Errorf("CC-D: %+v", d)
	}
	pctIs(t, "CC-D %", d.BudgetVarPct, -100)
	bud, _, act, bv, _ := b.Amounts()
	if bud != "100.000" || act != "150.000" || bv != "50.000" {
		t.Errorf("amounts: %s %s %s", bud, act, bv)
	}
}

// A threshold under the variance flags; over it does not.
func TestVarianceThreshold(t *testing.T) {
	in := varianceInputs{
		Months:  octoberSpans(),
		Budgets: []varianceLine{{CostCenter: "X", Amount: 100}},
		Actual:  map[varianceKey]float64{{"2026-10", "X", ""}: 112},
	}
	in.Threshold = 12
	if r := buildVariance(in); r[0].OverThreshold {
		t.Error("12.0 % is not beyond 12")
	}
	in.Threshold = 11.9
	if r := buildVariance(in); !r[0].OverThreshold {
		t.Error("12.0 % is beyond 11.9")
	}
}

// A period crossing two months prices each month on its own days, and two
// units never meet.
func TestVarianceMonthsAndUnits(t *testing.T) {
	spans := monthSpans(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	in := varianceInputs{
		Months:  spans,
		Budgets: []varianceLine{{CostCenter: "X", Amount: 310}, {CostCenter: "X", Unit: "EUR", Amount: 300}},
		Actual:  map[varianceKey]float64{{"2026-10", "X", ""}: 160, {"2026-11", "X", "EUR"}: 1},
	}
	in.Threshold = 15
	r := buildVariance(in)
	// Oct 16..Nov 1 = 16 days of 31: 310*16/31 = 160; Nov 1..16 = 15 of 30: 310*15/30 = 155.
	want := []struct {
		month, unit string
		budget      int64
	}{{"2026-10", "EUR", 15484}, {"2026-10", "gpu_hours", 160000}, {"2026-11", "EUR", 15000}, {"2026-11", "gpu_hours", 155000}}
	if len(r) != len(want) {
		t.Fatalf("%d rows: %+v", len(r), r)
	}
	for i, w := range want {
		if r[i].Month != w.month || r[i].Unit != w.unit || r[i].BudgetedMinor != w.budget {
			t.Errorf("row %d: %+v, want %+v", i, r[i], w)
		}
	}
}

func FuzzBuildVariance(f *testing.F) {
	f.Add(1000.0, 600.0, 3000.0, 100.0, uint8(7), 15.0, int64(0))
	f.Add(0.0, 0.0, 0.0, 0.0, uint8(0), 0.0, int64(30))
	f.Add(math.NaN(), math.Inf(1), -5.0, 1e300, uint8(255), math.NaN(), int64(400))
	f.Fuzz(func(t *testing.T, orgAmt, teamAmt, actual, allow float64, days uint8, thr float64, offset int64) {
		from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(offset%700))
		in := varianceInputs{
			Months: monthSpans(from, from.AddDate(0, 0, 1+int(days))),
			Budgets: []varianceLine{
				{CostCenter: "A", Amount: orgAmt},
				{CostCenter: "A", Amount: teamAmt, PeriodDays: float64(days % 8), Team: true},
			},
			Allowances: []varianceLine{{CostCenter: "A", Amount: allow, PeriodDays: 7}},
			Actual:     map[varianceKey]float64{},
			Threshold:  thr,
		}
		for _, sp := range in.Months {
			in.Actual[varianceKey{sp[0].Format("2006-01"), "A", ""}] = actual
		}
		rows := buildVariance(in)
		if len(rows) != len(in.Months) {
			t.Fatalf("%d rows for %d months", len(rows), len(in.Months))
		}
		if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].Month < rows[j].Month }) {
			t.Fatal("rows not sorted")
		}
		for _, r := range rows {
			if r.HasBudget && r.BudgetVarMinor != r.ActualMinor-r.BudgetedMinor {
				t.Fatalf("variance is not actual - budget: %+v", r)
			}
			if r.HasEstimate && r.EstimateVarMinor != r.ActualMinor-r.EstimatedMinor {
				t.Fatalf("estimate variance: %+v", r)
			}
			for _, p := range []*float64{r.BudgetVarPct, r.EstimateVarPct} {
				if p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0)) {
					t.Fatalf("non-finite percentage: %+v", r)
				}
			}
			if r.BudgetedMinor <= 0 && r.BudgetVarPct != nil {
				t.Fatalf("a percentage over a zero or negative budget: %+v", r)
			}
		}
	})
}
