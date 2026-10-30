package gateway

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// The chargeback file (#97): one row per cost centre and calendar month of
// the requested period, with GPU-hours, measured energy and its cost, ready
// for a finance system. FinOps separates showback from chargeback: this is
// the second, the "official chargebacks to the organization's Finance
// systems" (finops.org, Invoicing & Chargeback). Units are never summed:
// GPU-hours, kWh, the fluids currency and the currency of an external
// provider's spend are four columns; a provider's currency has rows of its
// own.
//
// A consumer's cost centre is the project's, else the lab's, else the
// organisation's (config cost_center). What has none is not dropped: it is the
// cost centre UNMAPPED, and energy no organisation is charged for is
// UNALLOCATED, so the file always adds up to the reports.

// Cost centres the file invents for what has none.
const (
	CostCenterUnmapped    = "UNMAPPED"
	CostCenterUnallocated = "UNALLOCATED"
)

// ChargebackRow is one line of the file.
type ChargebackRow struct {
	From       time.Time `json:"period_from"`
	To         time.Time `json:"period_to"`
	CostCenter string    `json:"cost_center"`
	GPUHours   float64   `json:"gpu_hours"`
	KWh        float64   `json:"kwh"`
	// Money is an integer count of the currency's minor unit (cents),
	// rounded once per row, half to even; FluidsCost and ProviderCost are the
	// same amounts as decimal strings. The rows of a file therefore add up to
	// its total exactly, in integers (#110).
	FluidsCostMinor   int64  `json:"fluids_cost_minor"`
	FluidsCost        string `json:"fluids_cost"`
	FluidsCurrency    string `json:"fluids_currency"`
	ProviderCostMinor int64  `json:"provider_cost_minor"`
	ProviderCost      string `json:"provider_cost"`
	ProviderCurrency  string `json:"provider_currency"`

	fluids, provider float64 // the amounts before rounding
}

// ProviderDecimals is the minor unit of an external provider's currency, in
// decimal digits: provider costs are in USD or EUR, two digits.
const ProviderDecimals = 2

// minorUnits rounds an amount to the currency's minor unit, half to even.
func minorUnits(v float64, decimals int) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.RoundToEven(v * math.Pow10(decimals)))
}

// formatMinor writes minor units as a decimal string with no float involved.
func formatMinor(m int64, decimals int) string {
	neg := m < 0
	if neg {
		m = -m
	}
	s := strconv.FormatInt(m, 10)
	if decimals > 0 {
		for len(s) <= decimals {
			s = "0" + s
		}
		s = s[:len(s)-decimals] + "." + s[len(s)-decimals:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// monthSpans splits [from, to) into calendar months (UTC), clipped to it.
func monthSpans(from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	for s := from.UTC(); s.Before(to); {
		next := time.Date(s.Year(), s.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		e := next
		if e.After(to) {
			e = to.UTC()
		}
		out = append(out, [2]time.Time{s, e})
		s = next
	}
	return out
}

type chargeKey struct {
	month    string
	cc, unit string
}

// chargeback builds the rows of a period for the organisations visible
// reports; kept sorted by period, cost centre, unit.
func (s *Server) chargeback(r *http.Request, p *principal, from, to time.Time) ([]ChargebackRow, error) {
	visible := func(org string) bool { return p.reportView() || p.orgView(org) }
	spans := monthSpans(from, to)
	agg := map[chargeKey]*ChargebackRow{}
	row := func(sp [2]time.Time, cc, unit string) *ChargebackRow {
		k := chargeKey{sp[0].Format("2006-01"), cc, unit}
		if a := agg[k]; a != nil {
			return a
		}
		a := &ChargebackRow{From: sp[0], To: sp[1], CostCenter: cc}
		agg[k] = a
		return a
	}
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	ccOf := func(org, lab, team, project string) string {
		if org == Unallocated {
			return CostCenterUnallocated
		}
		if cc := s.cfg.CostCenterOf(org, lab, team, project); cc != "" {
			return cc
		}
		return CostCenterUnmapped
	}
	// GPU-hours and provider spend.
	for _, sp := range spans {
		rows, err := s.visibleReport(r, p, sp[0], sp[1], []string{"org", "lab", "team", "project"})
		if err != nil {
			return nil, err
		}
		for _, x := range rows {
			if !visible(x.Org) {
				continue
			}
			a := row(sp, ccOf(x.Org, x.Lab, x.Team, x.Project), x.Unit)
			if x.Unit == "" {
				a.GPUHours += x.Cost
			} else {
				a.provider += x.Cost
				a.ProviderCurrency = x.Unit
			}
		}
	}
	// Measured energy and its cost.
	if f := s.cfg.Fluids; f != nil {
		if es, ok := s.st.(store.EnergyStore); ok {
			// One read per span, on its instants: an interval belongs to the
			// span its start falls in, exactly as in /admin/fluids, whatever
			// time of day the period ends (#110).
			for _, sp := range spans {
				all, err := s.allocatedEnergy(ctx, es, sp[0], sp[1])
				if err != nil {
					return nil, err
				}
				for _, c := range all {
					if !p.reportView() && (c.Org == Unallocated || !visible(c.Org)) {
						continue
					}
					a := row(sp, ccOf(c.Org, c.Lab, c.Team, c.Project), "")
					a.KWh += energy.KWh(c.ActiveMJ + c.IdleMJ)
					a.fluids += f.Params(sp[0].Format("2006-01")).Cost(c.ActiveMJ + c.IdleMJ)
					a.FluidsCurrency = f.Currency
				}
			}
		}
	}
	dec := 2
	if s.cfg.Fluids != nil {
		dec = s.cfg.Fluids.CurrencyDecimals
	}
	out := make([]ChargebackRow, 0, len(agg))
	for _, a := range agg {
		a.FluidsCostMinor, a.ProviderCostMinor = minorUnits(a.fluids, dec), minorUnits(a.provider, ProviderDecimals)
		a.FluidsCost, a.ProviderCost = formatMinor(a.FluidsCostMinor, dec), formatMinor(a.ProviderCostMinor, ProviderDecimals)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.From.Equal(b.From) {
			return a.From.Before(b.From)
		}
		if a.CostCenter != b.CostCenter {
			return a.CostCenter < b.CostCenter
		}
		return a.ProviderCurrency < b.ProviderCurrency
	})
	return out, nil
}

var chargebackHeader = []string{"period_from", "period_to", "cost_center", "gpu_hours", "kwh", "fluids_cost", "fluids_currency", "provider_cost", "provider_currency"}

func (c ChargebackRow) record() []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	return []string{c.From.UTC().Format("2006-01-02"), c.To.UTC().Format("2006-01-02"), csvSafe(c.CostCenter), f(c.GPUHours), f(c.KWh), c.FluidsCost,
		c.FluidsCurrency, c.ProviderCost, c.ProviderCurrency}
}

// chargebackReport answers GET /admin/chargeback?from=&to=&format=csv|json.
func (s *Server) chargebackReport(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.anyOrgView() {
		forbidden(w)
		return
	}
	q := r.URL.Query()
	from, err1 := ParseDay(q.Get("from"))
	to, err2 := ParseDay(q.Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	if to.Sub(from) > store.MaxReportSpan {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "the period is longer than 366 days")
		return
	}
	format := strings.TrimSpace(q.Get("format"))
	if format == "" {
		format = FormatCSV
	}
	if format != FormatCSV && format != FormatJSON {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want csv or json")
		return
	}
	release, ok := heavy(w)
	if !ok {
		return
	}
	defer release()
	rows, err := s.chargeback(r, p, from, to)
	if errors.Is(err, context.DeadlineExceeded) {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "the report took too long; shorten the period")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	if format == FormatJSON {
		if rows == nil {
			rows = []ChargebackRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"from": from.UTC(), "to": to.UTC(), "rows": rows})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	cw := csv.NewWriter(w)
	_ = cw.Write(chargebackHeader)
	for _, x := range rows {
		_ = cw.Write(x.record())
	}
	cw.Flush()
}
