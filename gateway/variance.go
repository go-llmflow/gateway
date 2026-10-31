package gateway

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Budget against actual, per partner cost centre (#97, FinOps). Sources
// opened 2026-10-30:
//
//   - FinOps Foundation, Budgeting (finops.org/framework/capabilities/budgeting):
//     "A budget is set at approval and reconciled against actuals after the
//     fact"; "Establish acceptable variance thresholds on Budget to Forecast
//     and Budget to Actual" (maximum variance 20 % at Crawl, 15 % at Walk,
//     12 % at Run); KPI "Budget vs actual cost trends within established
//     percentage threshold of variance".
//   - FinOps Foundation, Forecasting (.../forecasting): forecast accuracy is
//     "((Forecasted Spend - Actual Spend) / Forecasted Spend)", "percentage
//     variance is the primary metric, dollar differences can also be
//     informative", "Estimates should carry its own accuracy record".
//   - FOCUS (focus.finops.org/focus-specification): the page opened lists
//     budget use cases by title only and defines no budget or variance
//     column; the report follows FinOps' framework pages, not FOCUS.
//
// A row is one cost centre, one calendar month and one unit (GPU-hours, or
// the currency of an external provider's spend): units are never added.
//
//	Budgeted:  the declared budgets (org and team scope, no per-model budget),
//	           prorated to the month; the most specific wins: the team budgets
//	           mapped to the cost centre, else the organisation's.
//	Estimated: the profile allocations: the weekly allowance of every person's
//	           profile budget (profile_week), prorated to the month, at the
//	           cost centre of the person's team.
//	Actual:    the chargeback's figure for the cost centre and month.
//	Variance:  actual - budgeted (positive: over budget), in the same minor
//	           unit, and as a percentage of the budgeted amount. The FinOps
//	           formulas divide (forecast - actual) by the forecast; here the
//	           sign is "over" so that an overrun reads positive, and the
//	           denominator is the same: the budgeted or estimated amount.
//
// Money is integer minor units, each amount rounded once, half to even: a
// currency's cents, and for GPU-hours the thousandth (milli-GPU-hour). The
// variance is the difference of the rounded amounts, an exact integer.

// Decimals of the minor unit per unit of account.
const gpuHourDecimals = 3

// VarianceRow is one cost centre of one month in one unit.
type VarianceRow struct {
	Month      string `json:"month"`
	CostCenter string `json:"cost_center"`
	Unit       string `json:"unit"`
	Decimals   int    `json:"decimals"`

	BudgetedMinor  int64 `json:"budgeted_minor"`
	EstimatedMinor int64 `json:"estimated_minor"`
	ActualMinor    int64 `json:"actual_minor"`
	HasBudget      bool  `json:"has_budget"`
	HasEstimate    bool  `json:"has_estimate"`

	// Variance against the budget and against the estimate.
	BudgetVarMinor   int64    `json:"budget_variance_minor"`
	BudgetVarPct     *float64 `json:"budget_variance_pct"`
	EstimateVarMinor int64    `json:"estimate_variance_minor"`
	EstimateVarPct   *float64 `json:"estimate_variance_pct"`
	// OverThreshold: a variance beyond the configured threshold in absolute
	// value (variance_threshold_pct, default 15, FinOps' Walk).
	OverThreshold bool `json:"over_threshold"`
}

// Amounts renders minor units as a decimal string.
func (r VarianceRow) Amounts() (budgeted, estimated, actual, budgetVar, estimateVar string) {
	f := func(m int64) string { return formatMinor(m, r.Decimals) }
	return f(r.BudgetedMinor), f(r.EstimatedMinor), f(r.ActualMinor), f(r.BudgetVarMinor), f(r.EstimateVarMinor)
}

// varianceLine is a budget or an allowance, ready to prorate.
type varianceLine struct {
	CostCenter string
	Unit       string
	// Amount per period, and the period's length in days (a calendar month:
	// 0, taken from the month being priced).
	Amount     float64
	PeriodDays float64
	// Team marks a team-scope budget (the most specific wins over the org's).
	Team bool
}

type varianceKey struct{ month, cc, unit string }

// varianceInputs are the facts a variance report is computed from.
type varianceInputs struct {
	// Months are the calendar-month spans of the period, as chargeback's.
	Months [][2]time.Time
	// Budgets and Allowances are the declared and the profile lines.
	Budgets, Allowances []varianceLine
	// Actual is the amount actually spent, by month, cost centre and unit
	// ("" GPU-hours, else a currency).
	Actual map[varianceKey]float64
	// Threshold is the percentage beyond which a variance is flagged.
	Threshold float64
}

func unitName(u string) string { return store.UnitName(u) }

func decimalsOf(unit string) int {
	if unit == "" {
		return gpuHourDecimals
	}
	return 2
}

// prorate is a line's amount for the month span.
func (l varianceLine) prorate(span [2]time.Time) float64 {
	days := span[1].Sub(span[0]).Hours() / 24
	p := l.PeriodDays
	if p == 0 { // a calendar month
		a := span[0].UTC()
		p = float64(time.Date(a.Year(), a.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
	}
	return l.Amount * days / p
}

// buildVariance computes the rows, sorted by month, cost centre, unit.
func buildVariance(in varianceInputs) []VarianceRow {
	type acc struct {
		team, org, est          float64
		hasTeam, hasOrg, hasEst bool
	}
	cells := map[varianceKey]*acc{}
	get := func(k varianceKey) *acc {
		a := cells[k]
		if a == nil {
			a = &acc{}
			cells[k] = a
		}
		return a
	}
	for _, sp := range in.Months {
		month := sp[0].UTC().Format("2006-01")
		for _, l := range in.Budgets {
			a := get(varianceKey{month, l.CostCenter, l.Unit})
			if l.Team {
				a.team, a.hasTeam = a.team+l.prorate(sp), true
			} else {
				a.org, a.hasOrg = a.org+l.prorate(sp), true
			}
		}
		for _, l := range in.Allowances {
			a := get(varianceKey{month, l.CostCenter, l.Unit})
			a.est, a.hasEst = a.est+l.prorate(sp), true
		}
	}
	for k := range in.Actual {
		get(k)
	}
	rows := make([]VarianceRow, 0, len(cells))
	for k, a := range cells {
		dec := decimalsOf(k.unit)
		r := VarianceRow{Month: k.month, CostCenter: k.cc, Unit: unitName(k.unit), Decimals: dec}
		r.ActualMinor = minorUnits(in.Actual[k], dec)
		switch {
		case a.hasTeam:
			r.BudgetedMinor, r.HasBudget = minorUnits(a.team, dec), true
		case a.hasOrg:
			r.BudgetedMinor, r.HasBudget = minorUnits(a.org, dec), true
		}
		if a.hasEst {
			r.EstimatedMinor, r.HasEstimate = minorUnits(a.est, dec), true
		}
		pct := func(v, base int64) *float64 {
			if base <= 0 {
				return nil
			}
			p := math.RoundToEven(1000*float64(v)/float64(base)) / 10 // tenths of a percent
			return &p
		}
		if r.HasBudget {
			r.BudgetVarMinor = r.ActualMinor - r.BudgetedMinor
			r.BudgetVarPct = pct(r.BudgetVarMinor, r.BudgetedMinor)
		}
		if r.HasEstimate {
			r.EstimateVarMinor = r.ActualMinor - r.EstimatedMinor
			r.EstimateVarPct = pct(r.EstimateVarMinor, r.EstimatedMinor)
		}
		for _, p := range []*float64{r.BudgetVarPct, r.EstimateVarPct} {
			if p != nil && math.Abs(*p) > in.Threshold {
				r.OverThreshold = true
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.Month+"\x00"+a.CostCenter+"\x00"+a.Unit < b.Month+"\x00"+b.CostCenter+"\x00"+b.Unit
	})
	return rows
}

// ---- the gateway's part ----

// MaxVarianceRows bounds the variance table (#111).
const MaxVarianceRows = 3000

func (s *Server) varianceThreshold() float64 {
	if s.cfg.VarianceThresholdPct > 0 {
		return s.cfg.VarianceThresholdPct
	}
	return 15
}

// varianceReport builds the rows for the period from the chargeback's actuals,
// the declared budgets and the profile allowances of the partners p may see.
func (s *Server) varianceReport(ctx context.Context, p *principal, chargeback []ChargebackRow, from, to time.Time) ([]VarianceRow, error) {
	visible := func(org string) bool { return p.reportView() || org != "" && p.orgView(org) }
	teams, err := s.st.ListTeams(ctx, "")
	if err != nil {
		return nil, err
	}
	orgOfTeam := map[string]string{}
	for _, t := range teams {
		orgOfTeam[t.Name] = t.Org
	}
	ccOf := func(org, team string) string {
		if cc := s.cfg.CostCenterOf(org, s.cfg.LabOf(team), team, ""); cc != "" {
			return cc
		}
		return CostCenterUnmapped
	}
	budgets, err := s.st.Budgets(ctx)
	if err != nil {
		return nil, err
	}
	in := varianceInputs{Months: monthSpans(from, to), Actual: map[varianceKey]float64{}, Threshold: s.varianceThreshold()}
	for _, c := range chargeback {
		m := c.From.UTC().Format("2006-01")
		in.Actual[varianceKey{m, c.CostCenter, ""}] += c.GPUHours
		if c.ProviderCurrency != "" {
			in.Actual[varianceKey{m, c.CostCenter, c.ProviderCurrency}] += float64(c.ProviderCostMinor) / math.Pow10(ProviderDecimals)
		}
	}
	// A person's team, for the profile allowances.
	binds, err := s.st.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	homeTeam := map[string]string{}
	for _, b := range binds {
		if b.Team != "" {
			if cur, ok := homeTeam[b.User]; !ok || b.Team < cur {
				homeTeam[b.User] = b.Team
			}
		}
	}
	for _, b := range budgets {
		if b.Model != "" {
			continue
		}
		var days float64
		switch b.Period {
		case store.Day:
			days = 1
		case store.Week:
			days = 7
		case store.Month:
			days = 0
		default:
			if b.Kind != store.ScopeProfileWeek {
				continue // another rolling window: not a periodic budget
			}
			days = 7
		}
		switch b.Kind {
		case store.ScopeOrg:
			if visible(b.Name) {
				in.Budgets = append(in.Budgets, varianceLine{CostCenter: ccOf(b.Name, ""), Unit: b.Unit, Amount: b.MaxCost, PeriodDays: days})
			}
		case store.ScopeTeam:
			if org := orgOfTeam[b.Name]; visible(org) {
				in.Budgets = append(in.Budgets, varianceLine{CostCenter: ccOf(org, b.Name), Unit: b.Unit, Amount: b.MaxCost, PeriodDays: days, Team: true})
			}
		case store.ScopeProfileWeek:
			team := homeTeam[b.Name]
			if org := orgOfTeam[team]; team != "" && visible(org) {
				in.Allowances = append(in.Allowances, varianceLine{CostCenter: ccOf(org, team), Unit: b.Unit, Amount: b.MaxCost, PeriodDays: 7})
			}
		}
	}
	rows := buildVariance(in)
	if len(rows) > MaxVarianceRows {
		return nil, store.ErrTooMany
	}
	return rows, nil
}

func pctText(p *float64) string {
	if p == nil {
		return "n/a"
	}
	v := strconv.FormatFloat(*p, 'f', 1, 64)
	if *p > 0 {
		v = "+" + v
	}
	return v + " %"
}
