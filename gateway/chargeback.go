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
	FluidsCostMinor int64  `json:"fluids_cost_minor"`
	FluidsCost      string `json:"fluids_cost"`
	// FixedCostMinor and FixedCost are the fixed costs (racks, fluids {
	// fixed_cost }) charged to the cost centre, in the fluids currency; exact
	// integers (the split is in minor units). Network and storage are the
	// metered part, Markup the organisations' markup, Credit what the owners
	// are credited for a cost they bore and a non-shareholder paid, Adjustment
	// the difference of a closed month booked on this one (a credit note when
	// negative). TotalCost is fluids + fixed + network + storage + markup -
	// credit + adjustment, in minor units, so rows add up.
	FixedCostMinor    int64   `json:"fixed_cost_minor"`
	FixedCost         string  `json:"fixed_cost"`
	NetworkGB         float64 `json:"network_gb"`
	StorageGBMonth    float64 `json:"storage_gb_month"`
	NetworkCostMinor  int64   `json:"network_cost_minor"`
	NetworkCost       string  `json:"network_cost"`
	StorageCostMinor  int64   `json:"storage_cost_minor"`
	StorageCost       string  `json:"storage_cost"`
	MarkupMinor       int64   `json:"markup_minor"`
	Markup            string  `json:"markup"`
	CreditMinor       int64   `json:"credit_minor"`
	Credit            string  `json:"credit"`
	AdjustmentMinor   int64   `json:"adjustment_minor"`
	Adjustment        string  `json:"adjustment"`
	TotalCostMinor    int64   `json:"total_cost_minor"`
	TotalCost         string  `json:"total_cost"`
	FluidsCurrency    string  `json:"fluids_currency"`
	ProviderCostMinor int64   `json:"provider_cost_minor"`
	ProviderCost      string  `json:"provider_cost"`
	ProviderCurrency  string  `json:"provider_currency"`

	fluids, provider, network, storage, markup float64 // the amounts before rounding
	netBytes                                   int64
	storByteMonths                             float64
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
	rows, _, err := s.chargebackView(r, p, from, to)
	return rows, err
}

// chargebackView is chargeback with the view of the period it was built from
// (the closed months it read).
func (s *Server) chargebackView(r *http.Request, p *principal, from, to time.Time) ([]ChargebackRow, *billingView, error) {
	visible := func(org string) bool { return p.reportView() || p.orgView(org) }
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	view, err := s.billing(ctx, from, to)
	if err != nil {
		return nil, nil, err
	}
	spanOf := map[string][2]time.Time{}
	for _, sp := range view.spans {
		spanOf[monthKey(sp[0])] = sp
	}
	agg := map[chargeKey]*ChargebackRow{}
	row := func(month, cc, unit string) *ChargebackRow {
		sp, ok := spanOf[month]
		if !ok {
			return nil
		}
		k := chargeKey{month, cc, unit}
		if a := agg[k]; a != nil {
			return a
		}
		a := &ChargebackRow{From: sp[0], To: sp[1], CostCenter: cc}
		agg[k] = a
		return a
	}
	// GPU-hours and provider spend.
	for _, x := range view.usage {
		if !visible(x.Org) || len(x.Day) < 7 {
			continue
		}
		a := row(x.Day[:7], s.costCenterOf(x.Org, x.Lab, x.Team, x.Project), x.Unit)
		if a == nil {
			continue
		}
		if x.Unit == "" {
			a.GPUHours += x.Cost
		} else {
			a.provider += x.Cost
			a.ProviderCurrency = x.Unit
		}
	}
	// Measured energy, network and storage, the fixed costs, and their cost.
	f := s.cfg.Fluids
	if f != nil {
		for _, c := range view.cells {
			if !p.reportView() && (c.Org == Unallocated || !visible(c.Org)) {
				continue
			}
			a := row(c.month(), s.costCenterOf(c.Org, c.Lab, c.Team, c.Project), "")
			if a == nil {
				continue
			}
			m := priceCell(c, f, s.cfg.MarkupOf)
			a.KWh += energy.KWh(c.ActiveMJ + c.IdleMJ)
			a.fluids += m.fluids
			a.network += m.network
			a.markup += m.markup
			a.FixedCostMinor += c.FixedMinor
			a.CreditMinor += c.CreditMinor
			a.netBytes += c.NetIn + c.NetOut
			a.storByteMonths += float64(c.StorageByteDays) / daysIn(c.month())
			a.storage += m.storage
			a.FluidsCurrency = f.Currency
		}
		// Adjustments of closed months, on the month that follows them (a whole
		// month only: a part of a month would book them more than once).
		for _, sp := range view.spans {
			if !wholeMonth(sp) {
				continue
			}
			month := monthKey(sp[0])
			var adj []adjLine
			if snap := view.snaps[month]; snap != nil {
				adj = snap.Adjustments
			} else if adj, err = s.adjustmentsFor(ctx, month); err != nil {
				return nil, nil, err
			}
			for _, x := range adj {
				if !p.reportView() && !visible(x.Org) {
					continue
				}
				if a := row(month, x.CostCenter, ""); a != nil {
					a.AdjustmentMinor += x.total()
					a.FluidsCurrency = f.Currency
				}
			}
		}
	}
	dec := 2
	if f != nil {
		dec = f.CurrencyDecimals
	}
	out := make([]ChargebackRow, 0, len(agg))
	for _, a := range agg {
		a.FluidsCostMinor, a.ProviderCostMinor = minorUnits(a.fluids, dec), minorUnits(a.provider, ProviderDecimals)
		a.NetworkCostMinor, a.StorageCostMinor, a.MarkupMinor = minorUnits(a.network, dec), minorUnits(a.storage, dec), minorUnits(a.markup, dec)
		a.TotalCostMinor = a.FluidsCostMinor + a.FixedCostMinor + a.NetworkCostMinor + a.StorageCostMinor + a.MarkupMinor - a.CreditMinor + a.AdjustmentMinor
		a.NetworkGB, a.StorageGBMonth = float64(a.netBytes)/1e9, a.storByteMonths/1e9
		a.FluidsCost, a.ProviderCost = formatMinor(a.FluidsCostMinor, dec), formatMinor(a.ProviderCostMinor, ProviderDecimals)
		a.FixedCost, a.NetworkCost, a.StorageCost = formatMinor(a.FixedCostMinor, dec), formatMinor(a.NetworkCostMinor, dec), formatMinor(a.StorageCostMinor, dec)
		a.Markup, a.Credit, a.Adjustment, a.TotalCost = formatMinor(a.MarkupMinor, dec), formatMinor(a.CreditMinor, dec), formatMinor(a.AdjustmentMinor, dec), formatMinor(a.TotalCostMinor, dec)
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
	return out, view, nil
}

var chargebackHeader = []string{"period_from", "period_to", "cost_center", "gpu_hours", "kwh", "network_gb", "storage_gb_month",
	"fluids_cost", "fixed_cost", "network_cost", "storage_cost", "markup", "credit", "adjustment", "total_cost",
	"fluids_currency", "provider_cost", "provider_currency"}

func (c ChargebackRow) record() []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	return []string{c.From.UTC().Format("2006-01-02"), c.To.UTC().Format("2006-01-02"), csvSafe(c.CostCenter), f(c.GPUHours), f(c.KWh),
		f(c.NetworkGB), f(c.StorageGBMonth), c.FluidsCost, c.FixedCost, c.NetworkCost, c.StorageCost, c.Markup, c.Credit, c.Adjustment, c.TotalCost,
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
	rows, view, err := s.chargebackView(r, p, from, to)
	if errors.Is(err, context.DeadlineExceeded) {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "the report took too long; shorten the period")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	s.stampMethod(w, view)
	if format == FormatJSON {
		if rows == nil {
			rows = []ChargebackRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"from": from.UTC(), "to": to.UTC(), "rows": rows, "method": s.billingMethod(), "closed_months": view.closed})
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
