package gateway

import (
	"encoding/csv"
	"encoding/json"
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
	From             time.Time `json:"period_from"`
	To               time.Time `json:"period_to"`
	CostCenter       string    `json:"cost_center"`
	GPUHours         float64   `json:"gpu_hours"`
	KWh              float64   `json:"kwh"`
	FluidsCost       float64   `json:"fluids_cost"`
	FluidsCurrency   string    `json:"fluids_currency"`
	ProviderCost     float64   `json:"provider_cost"`
	ProviderCurrency string    `json:"provider_currency"`
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
	spanOf := func(day string) (int, bool) {
		for i, sp := range spans {
			if day >= sp[0].Format("2006-01-02") && day < sp[1].Format("2006-01-02") {
				return i, true
			}
		}
		return 0, false
	}
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
				a.ProviderCost += x.Cost
				a.ProviderCurrency = x.Unit
			}
		}
	}
	// Measured energy and its cost.
	if f := s.cfg.Fluids; f != nil {
		if es, ok := s.st.(store.EnergyStore); ok {
			cells, idle, err := es.EnergyBetween(r.Context(), from, to)
			if err != nil {
				return nil, err
			}
			all, err := allocateFluids(cells, idle, f, s.orgShares())
			if err != nil {
				return nil, err
			}
			for _, c := range all {
				if !p.reportView() && (c.Org == Unallocated || !visible(c.Org)) {
					continue
				}
				i, ok := spanOf(c.Day)
				if !ok {
					continue
				}
				a := row(spans[i], ccOf(c.Org, c.Lab, c.Team, c.Project), "")
				a.KWh += energy.KWh(c.ActiveMJ + c.IdleMJ)
				a.FluidsCost += f.Params(c.month()).Cost(c.ActiveMJ + c.IdleMJ)
				a.FluidsCurrency = f.Currency
			}
		}
	}
	out := make([]ChargebackRow, 0, len(agg))
	for _, a := range agg {
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
	return []string{c.From.UTC().Format("2006-01-02"), c.To.UTC().Format("2006-01-02"), csvSafe(c.CostCenter), f(c.GPUHours), f(c.KWh), f(c.FluidsCost),
		c.FluidsCurrency, f(c.ProviderCost), c.ProviderCurrency}
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
	rows, err := s.chargeback(r, p, from, to)
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
