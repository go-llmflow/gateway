package gateway

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// The fluids report (#97): measured energy per dimension, side by side with
// GPU-hours, and its cost kWh_IT × PUE × price. Units are never summed:
// GPU-hours, kWh and the currency are three columns.
//
// Allocation of idle energy follows the agreement (fluids.idle_energy) and
// is decided per month, on the whole month's energy: pro rata to the
// guaranteed shares (by_share) or to what each organisation used (by_usage).
// An organisation's idle energy is then spread over its rows pro rata to
// their active energy, so that every grouping sums to the measured total.
// Energy nobody can be charged for (no share configured, no usage) is the
// organisation "(unallocated)", only visible to the proxy roles.

// Unallocated names the energy no organisation is charged for.
const Unallocated = "(unallocated)"

// FluidsDims are the dimensions energy can be grouped by (a request's key,
// tags and profile are not part of an energy record).
var FluidsDims = []string{"org", "lab", "team", "project", "user", "model", "pool", "day"}

// ParseFluidsDims checks and orders the dimensions of a fluids report.
func ParseFluidsDims(v string) ([]string, error) {
	seen := map[string]bool{}
	for _, d := range strings.Split(v, ",") {
		if d = strings.TrimSpace(d); d != "" {
			if !contains(FluidsDims, d) {
				return nil, fmt.Errorf("dimension %q: energy is grouped by %s", d, strings.Join(FluidsDims, ", "))
			}
			seen[d] = true
		}
	}
	out := []string{}
	for _, d := range FluidsDims {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// fluidsCell is the finest grain of the report: a consumer on a day (Day ""
// for idle energy not placed on a day).
type fluidsCell struct {
	Day string
	energy.Consumer
	ActiveMJ, IdleMJ int64
}

func (c fluidsCell) month() string {
	if len(c.Day) >= 7 {
		return c.Day[:7]
	}
	return ""
}

func (c fluidsCell) dim(d string) string {
	switch d {
	case "org":
		return c.Org
	case "lab":
		return c.Lab
	case "team":
		return c.Team
	case "project":
		return c.Project
	case "user":
		return c.User
	case "model":
		return c.Model
	case "pool":
		return c.Pool
	case "day":
		return c.Day
	}
	return ""
}

// allocateFluids places the idle energy of each month on organisations and
// spreads it over their cells. The sum of ActiveMJ+IdleMJ over the result is
// exactly the measured energy of the input.
func allocateFluids(cells []store.EnergyCell, idle []store.EnergyIdle, f *config.Fluids, shares map[string]float64) ([]fluidsCell, error) {
	var out []fluidsCell
	byMonth := map[string][]int{} // month → indices into out
	for _, c := range cells {
		out = append(out, fluidsCell{Day: c.Day, Consumer: c.Consumer, ActiveMJ: c.MJ})
		if len(c.Day) >= 7 {
			byMonth[c.Day[:7]] = append(byMonth[c.Day[:7]], len(out)-1)
		}
	}
	idleOf := map[string]int64{}
	for _, i := range idle {
		if len(i.Day) >= 7 {
			idleOf[i.Day[:7]] += i.MJ
		}
	}
	months := make([]string, 0, len(idleOf))
	for m := range idleOf {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, m := range months {
		usage := map[string]float64{}
		for _, i := range byMonth[m] {
			usage[out[i].Org] += float64(out[i].ActiveMJ)
		}
		alloc, err := energy.AllocateIdle(idleOf[m], f.IdleEnergy, shares, usage)
		if err != nil {
			return nil, err
		}
		orgs := make([]string, 0, len(alloc))
		for o := range alloc {
			orgs = append(orgs, o)
		}
		sort.Strings(orgs)
		for _, o := range orgs {
			var idx []int
			var ws []float64
			for _, i := range byMonth[m] {
				if out[i].Org == o {
					idx = append(idx, i)
					ws = append(ws, float64(out[i].ActiveMJ))
				}
			}
			parts := energy.Apportion(alloc[o], ws)
			if parts == nil { // the organisation did nothing this month: its idle energy has no row yet
				name := o
				if o == "" {
					name = Unallocated
				}
				out = append(out, fluidsCell{Day: m + "-01", Consumer: energy.Consumer{Org: name}, IdleMJ: alloc[o]})
				continue
			}
			for k, i := range idx {
				out[i].IdleMJ += parts[k]
			}
		}
	}
	return out, nil
}

// FluidsRow is one group of the fluids report.
type FluidsRow struct {
	Org      string  `json:"org,omitempty"`
	Lab      string  `json:"lab,omitempty"`
	Team     string  `json:"team,omitempty"`
	Project  string  `json:"project,omitempty"`
	User     string  `json:"user,omitempty"`
	Model    string  `json:"model,omitempty"`
	Pool     string  `json:"pool,omitempty"`
	Day      string  `json:"day,omitempty"`
	GPUHours float64 `json:"gpu_hours"`
	// KWhActive and KWhIdle are IT energy, as the GPUs measured it; KWh is
	// their sum.
	KWhActive float64 `json:"kwh_active"`
	KWhIdle   float64 `json:"kwh_idle"`
	KWh       float64 `json:"kwh"`
	// Cost is kWh × PUE × price in Currency ("" when no price is set).
	Cost     float64 `json:"fluids_cost"`
	Currency string  `json:"currency"`

	activeMJ, idleMJ int64
}

func (r FluidsRow) dim(d string) string {
	return fluidsCell{Day: r.Day, Consumer: energy.Consumer{Org: r.Org, Lab: r.Lab, Team: r.Team, Project: r.Project, User: r.User, Model: r.Model, Pool: r.Pool}}.dim(d)
}

func (r *FluidsRow) set(d, v string) {
	switch d {
	case "org":
		r.Org = v
	case "lab":
		r.Lab = v
	case "team":
		r.Team = v
	case "project":
		r.Project = v
	case "user":
		r.User = v
	case "model":
		r.Model = v
	case "pool":
		r.Pool = v
	case "day":
		r.Day = v
	}
}

func fluidsKey(dims []string, get func(string) string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = get(d)
	}
	return strings.Join(parts, "\x00")
}

// groupFluids regroups cells onto dims, pricing each cell at its month's
// terms before they are added.
func groupFluids(cells []fluidsCell, dims []string, f *config.Fluids) []FluidsRow {
	agg := map[string]*FluidsRow{}
	var keys []string
	for _, c := range cells {
		k := fluidsKey(dims, c.dim)
		r := agg[k]
		if r == nil {
			r = &FluidsRow{Currency: f.Currency}
			for _, d := range dims {
				r.set(d, c.dim(d))
			}
			agg[k] = r
			keys = append(keys, k)
		}
		r.activeMJ += c.ActiveMJ
		r.idleMJ += c.IdleMJ
		r.Cost += f.Params(c.month()).Cost(c.ActiveMJ + c.IdleMJ)
	}
	sort.Strings(keys)
	out := make([]FluidsRow, 0, len(keys))
	for _, k := range keys {
		r := *agg[k]
		r.KWhActive, r.KWhIdle = energy.KWh(r.activeMJ), energy.KWh(r.idleMJ)
		r.KWh = energy.KWh(r.activeMJ + r.idleMJ)
		out = append(out, r)
	}
	return out
}

// anyOrgView reports whether the principal may read something at the
// organisation level: a proxy role, or org_admin or viewer of an organisation.
func (p *principal) anyOrgView() bool {
	if p.reportView() {
		return true
	}
	for _, b := range p.bindings {
		if b.Org != "" && (b.Role == store.RoleOrgAdmin || b.Role == store.RoleViewer) {
			return true
		}
	}
	return false
}

func (s *Server) orgShares() map[string]float64 {
	m := map[string]float64{}
	for _, o := range s.cfg.Orgs {
		m[o.Name] = o.Share
	}
	return m
}

// fluidsData computes the visible rows of the fluids report, with GPU-hours
// joined by dimension.
func (s *Server) fluidsData(r *http.Request, p *principal, from, to time.Time, dims []string) ([]FluidsRow, error) {
	es, ok := s.st.(store.EnergyStore)
	if !ok {
		return nil, store.ErrNotFound
	}
	f := s.cfg.Fluids
	cells, idle, err := es.EnergyBetween(r.Context(), from, to)
	if err != nil {
		return nil, err
	}
	all, err := allocateFluids(cells, idle, f, s.orgShares())
	if err != nil {
		return nil, err
	}
	// Visibility is decided on the finest cells, before grouping: a row of a
	// grouping that does not name the organisation must still hold nothing of
	// another one.
	kept := all[:0:0]
	for _, c := range all {
		if p.reportView() || c.Org != Unallocated && p.orgView(c.Org) {
			kept = append(kept, c)
		}
	}
	rows := groupFluids(kept, dims, f)
	gpu, err := s.visibleReport(r, p, from, to, dims)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, row := range rows {
		idx[fluidsKey(dims, row.dim)] = i
	}
	for _, g := range gpu {
		if g.Unit != "" {
			continue
		}
		k := fluidsKey(dims, g.Dim)
		if i, ok := idx[k]; ok {
			rows[i].GPUHours += g.Cost
			continue
		}
		row := FluidsRow{Currency: f.Currency, GPUHours: g.Cost}
		for _, d := range dims {
			row.set(d, g.Dim(d))
		}
		idx[k] = len(rows)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return fluidsKey(dims, rows[i].dim) < fluidsKey(dims, rows[j].dim)
	})
	return rows, nil
}

func fluidsHeader(dims []string) []string {
	return append(append([]string{"from", "to"}, dims...), "gpu_hours", "kwh_active", "kwh_idle", "kwh", "fluids_cost", "currency")
}

func fluidsRecord(from, to time.Time, dims []string, r FluidsRow) []string {
	rec := []string{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}
	for _, d := range dims {
		rec = append(rec, csvSafe(r.dim(d)))
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	return append(rec, f(r.GPUHours), f(r.KWhActive), f(r.KWhIdle), f(r.KWh), f(r.Cost), r.Currency)
}

// fluidsReport answers GET /admin/fluids?from=&to=&group_by=&format=json|csv.
// Proxy roles see every organisation; org_admin and viewer of an
// organisation see that organisation; team-level roles have no energy view
// (a team's share of an organisation's electricity is its administrator's
// to give).
func (s *Server) fluidsReport(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.cfg.Fluids == nil {
		apiError(w, http.StatusNotFound, "not_found", "fluids are not configured")
		return
	}
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
		apiError(w, http.StatusBadRequest, "invalid_request_error", "the period is longer than "+strconv.Itoa(int(store.MaxReportSpan.Hours()/24))+" days")
		return
	}
	gb := q.Get("group_by")
	if gb == "" {
		gb = "org"
	}
	dims, err := ParseFluidsDims(gb)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	format := q.Get("format")
	if format == "" {
		format = FormatJSON
	}
	if format != FormatJSON && format != FormatCSV {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want json or csv")
		return
	}
	rows, err := s.fluidsData(r, p, from, to, dims)
	if err != nil {
		storeError(w, err)
		return
	}
	if format == FormatCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		_ = cw.Write(fluidsHeader(dims))
		for _, row := range rows {
			_ = cw.Write(fluidsRecord(from, to, dims, row))
		}
		cw.Flush()
		return
	}
	if rows == nil {
		rows = []FluidsRow{}
	}
	f := s.cfg.Fluids
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"from": from.UTC(), "to": to.UTC(), "group_by": dims, "idle_energy": f.IdleEnergy,
		"pue": f.PUE, "price_per_kwh": f.PricePerKWh, "currency": f.Currency, "rows": rows,
	})
}
