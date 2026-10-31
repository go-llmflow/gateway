package gateway

import (
	"sort"
	"strconv"

	"github.com/go-ooxml/ooxml/xlsx"
)

// The quota, profile and variance parts of the decision report (#97): the
// views of the PDF and the sheets of the workbook. The figures are computed in
// quota_report.go and variance.go; this file only lays them out.

// row7 is a table row of up to seven cells, already text.
type row7 struct{ A, B, C, D, E, F, G string }

// quotaView is the quota section as text tables.
type quotaView struct {
	Has                               bool
	Notes                             []string
	Soft, Refusals, Profiles, Oversub []row7
}

func optInt(p *int64) string {
	if p == nil {
		return "suppressed"
	}
	return strconv.FormatInt(*p, 10)
}

func optNum(p *float64) string {
	if p == nil {
		return "suppressed"
	}
	return fmtNum(*p)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// oversubSummary condenses the sampled days of each partner: the days sampled,
// the days above 1.0, the peak and the average ratio. Suppressed days (a cell
// under the small-cell threshold) and days without a measured capacity are not
// in the ratios.
type oversubSummary struct {
	Org                   string
	Days, Above, Measured int
	Peak, Sum             float64
	Suppressed            int
}

func summariseOversub(rows []OversubRow) []oversubSummary {
	by := map[string]*oversubSummary{}
	for _, r := range rows {
		s := by[r.Org]
		if s == nil {
			s = &oversubSummary{Org: r.Org}
			by[r.Org] = s
		}
		s.Days++
		if r.Suppressed || r.Ratio == nil {
			if r.Suppressed {
				s.Suppressed++
			}
			continue
		}
		s.Measured++
		s.Sum += *r.Ratio
		if *r.Ratio > s.Peak {
			s.Peak = *r.Ratio
		}
		if *r.Ratio > 1 {
			s.Above++
		}
	}
	out := make([]oversubSummary, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Org < out[j].Org })
	return out
}

// view is nil-safe: a store without the quota tables has no section.
func (q *QuotaReport) view() quotaView {
	if q == nil {
		return quotaView{}
	}
	v := quotaView{Has: true, Notes: q.Notes}
	for _, r := range q.Soft {
		v.Soft = append(v.Soft, row7{A: r.Org, B: orDash(r.Lab), C: r.Window, D: strconv.Itoa(r.Threshold) + " %", E: optInt(r.Events), F: optInt(r.People)})
	}
	for _, r := range q.Refusals {
		v.Refusals = append(v.Refusals, row7{A: r.Org, B: orDash(r.Lab), C: orDash(r.Profile), D: r.Kind, E: optInt(r.Refusals)})
	}
	for _, r := range q.Profiles {
		util := "n/a"
		if r.Utilisation != nil {
			util = pctS(*r.Utilisation)
		} else if r.Suppressed {
			util = "suppressed"
		}
		v.Profiles = append(v.Profiles, row7{A: r.Org, B: orDash(r.Lab), C: r.Profile, D: optInt(r.People), E: optNum(r.AssignedDays), F: optNum(r.UsedDays), G: util})
	}
	for _, s := range summariseOversub(q.Oversub) {
		peak, avg := "suppressed", "suppressed"
		if s.Measured > 0 {
			peak, avg = fmtNum(s.Peak), fmtNum(s.Sum/float64(s.Measured))
		}
		v.Oversub = append(v.Oversub, row7{A: s.Org, B: strconv.Itoa(s.Days), C: strconv.Itoa(s.Above), D: peak, E: avg})
	}
	return v
}

// varianceView is a variance row as text.
type varianceView struct {
	Month, CostCenter, Unit, Budgeted, Estimated, Actual, BudgetVar, EstimateVar, Flag string
}

func varianceNote(limit float64) string {
	return "Variance is actual minus budgeted (positive: over budget) and as a percentage of the budgeted amount; " +
		"Budgeted is the declared budgets prorated to the month (a team budget over the organisation's), Estimated the weekly profile allowances of the partner's people. " +
		"Amounts are in minor units rounded once, half to even; units are never added. A row is flagged beyond " + strconv.FormatFloat(limit, 'f', -1, 64) +
		" % (variance_threshold_pct; FinOps Foundation, Budgeting: 20 % Crawl, 15 % Walk, 12 % Run)."
}

func (d *decisionData) varianceViews() []varianceView {
	var out []varianceView
	for _, r := range d.Variance {
		bud, est, act, bv, ev := r.Amounts()
		v := varianceView{Month: r.Month, CostCenter: r.CostCenter, Unit: r.Unit, Budgeted: "no budget", Estimated: "no estimate", Actual: act, BudgetVar: "n/a", EstimateVar: "n/a"}
		if r.HasBudget {
			v.Budgeted = bud
			v.BudgetVar = bv + " (" + pctText(r.BudgetVarPct) + ")"
		}
		if r.HasEstimate {
			v.Estimated = est
			v.EstimateVar = ev + " (" + pctText(r.EstimateVarPct) + ")"
		}
		if r.OverThreshold {
			v.Flag = "beyond " + strconv.FormatFloat(d.VarianceLimit, 'f', -1, 64) + " %"
		}
		out = append(out, v)
	}
	return out
}

// ---- the workbook ----

func suppressedCell(p *int64) xlsx.Cell {
	if p == nil {
		return xlsx.String("suppressed")
	}
	return xlsx.Number(float64(*p))
}

func suppressedNum(p *float64) xlsx.Cell {
	if p == nil {
		return xlsx.String("suppressed")
	}
	return xlsx.Number(*p).Format(xlsx.Decimal(3))
}

func pctCell(p *float64) xlsx.Cell {
	if p == nil {
		return xlsx.String("n/a")
	}
	return xlsx.Number(*p / 100).Format(xlsx.Percent(1))
}

// writeQuotaSheets adds the variance and quota sheets to the workbook.
func writeQuotaSheets(d *decisionData, sheet func(name string, widths []float64, head ...string) (*xlsx.Sheet, error)) error {
	vs, err := sheet("Variance", []float64{10, 14, 10}, "Month", "Cost centre", "Unit", "Budgeted", "Estimated", "Actual", "Against budget", "Against budget %", "Against estimate", "Against estimate %", "Beyond threshold")
	if err != nil {
		return err
	}
	for _, r := range d.Variance {
		bud, est, bv, ev := xlsx.String("no budget"), xlsx.String("no estimate"), xlsx.String("n/a"), xlsx.String("n/a")
		if r.HasBudget {
			bud, bv = money(r.BudgetedMinor, r.Decimals), money(r.BudgetVarMinor, r.Decimals)
		}
		if r.HasEstimate {
			est, ev = money(r.EstimatedMinor, r.Decimals), money(r.EstimateVarMinor, r.Decimals)
		}
		flag := "no"
		if r.OverThreshold {
			flag = "yes"
		}
		if err := vs.AppendRow(xlsx.String(r.Month), xlsx.String(r.CostCenter), xlsx.String(r.Unit), bud, est, money(r.ActualMinor, r.Decimals),
			bv, pctCell(r.BudgetVarPct), ev, pctCell(r.EstimateVarPct), xlsx.String(flag)); err != nil {
			return err
		}
	}
	q := d.Quotas
	if q == nil {
		return nil
	}
	ss, err := sheet("Soft quotas", []float64{16, 14, 10}, "Partner", "Lab", "Window", "Threshold %", "Events", "People")
	if err != nil {
		return err
	}
	for _, r := range q.Soft {
		if err := ss.AppendRow(xlsx.String(r.Org), xlsx.String(r.Lab), xlsx.String(r.Window), xlsx.Number(float64(r.Threshold)), suppressedCell(r.Events), suppressedCell(r.People)); err != nil {
			return err
		}
	}
	rs, err := sheet("Refusals", []float64{16, 14, 14, 20}, "Partner", "Lab", "Profile", "Kind", "Refusals")
	if err != nil {
		return err
	}
	for _, r := range q.Refusals {
		if err := rs.AppendRow(xlsx.String(r.Org), xlsx.String(r.Lab), xlsx.String(r.Profile), xlsx.String(r.Kind), suppressedCell(r.Refusals)); err != nil {
			return err
		}
	}
	ps, err := sheet("Profiles", []float64{16, 14, 14}, "Partner", "Lab", "Profile", "People", "Assigned days", "Used days", "Used / assigned")
	if err != nil {
		return err
	}
	for _, r := range q.Profiles {
		if err := ps.AppendRow(xlsx.String(r.Org), xlsx.String(r.Lab), xlsx.String(r.Profile), suppressedCell(r.People), suppressedNum(r.AssignedDays), suppressedNum(r.UsedDays), pctCell(r.Utilisation)); err != nil {
			return err
		}
	}
	os, err := sheet("Oversubscription", []float64{16, 12}, "Partner", "Day", "People", "Assigned days per week", "Capacity days per week", "Ratio")
	if err != nil {
		return err
	}
	for _, r := range q.Oversub {
		ratio := suppressedNum(r.Ratio)
		if r.Ratio == nil && !r.Suppressed {
			ratio = xlsx.String("capacity not measured")
		}
		if err := os.AppendRow(xlsx.String(r.Org), xlsx.String(r.Day), suppressedCell(r.People), suppressedNum(r.AssignedDaysPerWeek), suppressedNum(r.CapacityDaysPerWeek), ratio); err != nil {
			return err
		}
	}
	return nil
}
