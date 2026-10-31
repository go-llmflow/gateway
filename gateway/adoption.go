package gateway

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// GET /admin/adoption?from=&to=&format=json|csv (#97): active people per day
// and per week, requests, and the models used, as counts. No name, key or
// content leaves it. The proxy roles and finance_viewer read every
// organisation, org_admin and org-level viewer their own.
//
// Small cells are not shown (adoption_min_users, default 5, at least 2): a day,
// a week or a model used by 1 to k-1 distinct people is suppressed (primary
// suppression), and the hidden models are merged into one "(other models)"
// row. A figure over one person is that person; a service key's requests
// count as requests and as no one.
//
// Requests add up: the days, the weeks and the models each partition the
// period's requests, and a week is the sum of its days. So a hidden cell is
// recovered by subtraction from the others unless enough is hidden with it
// (ESSnet Handbook on Statistical Disclosure Control, §5.3 p. 175: "to ensure
// these cannot be derived by subtractions from published marginal totals,
// additional cells are selected for secondary suppression", and "the same
// cells in any linked tables" must be). planAdoption applies that to each of
// the four sums: while the hidden cells of a sum stand for 1 to k-1 distinct
// people (a union, measured by the store), the smallest shown cell of that sum
// is hidden too.

// adoptionPoint is a day, a week or a model; Suppressed cells carry no count.
type adoptionPoint struct {
	Period      string `json:"period"`
	ActiveUsers *int64 `json:"active_users"`
	Requests    *int64 `json:"requests"`
	Suppressed  bool   `json:"suppressed,omitempty"`
}

type adoptionModel struct {
	Model       string `json:"model"`
	ActiveUsers *int64 `json:"active_users"`
	Requests    *int64 `json:"requests"`
	Suppressed  bool   `json:"suppressed,omitempty"`
}

type adoptionView struct {
	ActiveUsers   *int64          `json:"active_users"`
	Daily, Weekly []adoptionPoint `json:"-"`
	Models        []adoptionModel `json:"-"`
}

// OtherModels names the merged suppressed models.
const OtherModels = "(other models)"

func ptr(v int64) *int64 { return &v }

// hidden reports whether a count of distinct people is a small cell: some, but
// fewer than k.
func hidden(users int64, k int) bool { return users > 0 && users < int64(k) }

// suppressAdoption applies the small-cell rule and the plan planAdoption left
// in a.Hide.
func suppressAdoption(a *store.Adoption, k int) adoptionView {
	var v adoptionView
	if !hidden(a.Users, k) {
		v.ActiveUsers = ptr(a.Users)
	}
	pts := func(dim string, in []store.AdoptionPoint) []adoptionPoint {
		out := make([]adoptionPoint, 0, len(in))
		for _, x := range in {
			if hidden(x.ActiveUsers, k) || a.Hide[dim][x.Day] {
				out = append(out, adoptionPoint{Period: x.Day, Suppressed: true})
				continue
			}
			out = append(out, adoptionPoint{Period: x.Day, ActiveUsers: ptr(x.ActiveUsers), Requests: ptr(x.Requests)})
		}
		return out
	}
	v.Daily, v.Weekly = pts(store.DimDay, a.Daily), pts(store.DimWeek, a.Weekly)
	v.Models = []adoptionModel{}
	var merged, mergedN int64
	for _, m := range a.Models {
		if hidden(m.Users, k) || a.Hide[store.DimModel][m.Model] {
			merged += m.Requests
			mergedN++
			continue
		}
		v.Models = append(v.Models, adoptionModel{Model: m.Model, ActiveUsers: ptr(m.Users), Requests: ptr(m.Requests)})
	}
	// The merged row carries its counts only when the DISTINCT people over the
	// hidden models (a union, never a sum: two models of one person are one
	// person) are k or more; otherwise it carries no count at all.
	if mergedN >= 1 {
		row := adoptionModel{Model: OtherModels, Suppressed: true}
		if a.MergedUsers > 0 && !hidden(a.MergedUsers, k) {
			row.Suppressed, row.ActiveUsers, row.Requests = false, ptr(a.MergedUsers), ptr(merged)
		}
		v.Models = append(v.Models, row)
	}
	return v
}

// adoptionCell is a cell of one of the sums planAdoption protects.
type adoptionCell struct {
	key   string
	users int64
}

// unionFunc counts the distinct people over cells of a dimension.
type unionFunc func(dim string, keys []string) (int64, error)

// planAdoption computes the secondary suppression of a (see the comment at
// the top) into a.Hide and a.MergedUsers. union measures the distinct people
// over a set of cells. Every step hides one more cell, so it ends.
func planAdoption(a *store.Adoption, k int, union unionFunc) error {
	a.Hide = map[string]map[string]bool{store.DimDay: {}, store.DimWeek: {}, store.DimModel: {}}
	isHidden := func(dim string, c adoptionCell) bool { return hidden(c.users, k) || a.Hide[dim][c.key] }
	cellsOf := func(ps []store.AdoptionPoint) []adoptionCell {
		out := make([]adoptionCell, len(ps))
		for i, p := range ps {
			out[i] = adoptionCell{p.Day, p.ActiveUsers}
		}
		return out
	}
	days, weeks := cellsOf(a.Daily), cellsOf(a.Weekly)
	models := make([]adoptionCell, len(a.Models))
	for i, m := range a.Models {
		models[i] = adoptionCell{m.Model, m.Users}
	}
	// protect makes the hidden cells of one sum stand for k people or more,
	// or none; it reports whether it hid a cell.
	protect := func(dim string, cells []adoptionCell) (bool, error) {
		added := false
		for {
			var keys []string
			var smallest *adoptionCell
			for i := range cells {
				c := &cells[i]
				if isHidden(dim, *c) {
					keys = append(keys, c.key)
				} else if c.users > 0 && (smallest == nil || c.users < smallest.users || c.users == smallest.users && c.key < smallest.key) {
					smallest = c
				}
			}
			if len(keys) == 0 {
				return added, nil
			}
			u, err := union(dim, keys)
			if err != nil {
				return added, err
			}
			if !hidden(u, k) || smallest == nil {
				return added, nil
			}
			a.Hide[dim][smallest.key] = true
			added = true
		}
	}
	for {
		changed := false
		for _, s := range []struct {
			dim   string
			cells []adoptionCell
		}{{store.DimDay, days}, {store.DimWeek, weeks}, {store.DimModel, models}} {
			c, err := protect(s.dim, s.cells)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		// A shown week is the sum of its days.
		for _, w := range weeks {
			if isHidden(store.DimWeek, w) {
				continue
			}
			var in []adoptionCell
			for _, d := range days {
				if mondayOf(d.key) == w.key {
					in = append(in, d)
				}
			}
			c, err := protect(store.DimDay, in)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		if !changed {
			break
		}
	}
	var hm []string
	for _, m := range models {
		if isHidden(store.DimModel, m) {
			hm = append(hm, m.key)
		}
	}
	if len(hm) > 0 {
		u, err := union(store.DimModel, hm)
		if err != nil {
			return err
		}
		a.MergedUsers = u
	}
	return nil
}

// mondayOf is the Monday (UTC) of a YYYY-MM-DD day, as the store keys weeks.
func mondayOf(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)).Format("2006-01-02")
}

// adoptionOf reads the adoption of a period and plans its secondary
// suppression. A store that cannot count a union is answered by the largest
// cell among those hidden: a lower bound of the union, so the plan hides at
// least as much as with the exact count.
func (s *Server) adoptionOf(ctx context.Context, as store.AdoptionStore, from, to time.Time, orgs []string) (*store.Adoption, error) {
	a, err := as.Adoption(ctx, from, to, orgs)
	if err != nil {
		return nil, err
	}
	union := lowerBoundUnion(a)
	if us, ok := as.(store.AdoptionUnionStore); ok {
		union = func(dim string, keys []string) (int64, error) {
			return us.UsersOf(ctx, from, to, orgs, dim, keys)
		}
	}
	if err := planAdoption(a, s.adoptionMin(), union); err != nil {
		return nil, err
	}
	return a, nil
}

// lowerBoundUnion answers a union by its largest member.
func lowerBoundUnion(a *store.Adoption) unionFunc {
	users := map[string]map[string]int64{store.DimDay: {}, store.DimWeek: {}, store.DimModel: {}}
	for _, p := range a.Daily {
		users[store.DimDay][p.Day] = p.ActiveUsers
	}
	for _, p := range a.Weekly {
		users[store.DimWeek][p.Day] = p.ActiveUsers
	}
	for _, m := range a.Models {
		users[store.DimModel][m.Model] = m.Users
	}
	return func(dim string, keys []string) (int64, error) {
		var n int64
		for _, key := range keys {
			n = max(n, users[dim][key])
		}
		return n, nil
	}
}

func (s *Server) adoptionMin() int {
	if s.cfg.AdoptionMinUsers >= 2 {
		return s.cfg.AdoptionMinUsers
	}
	return 5
}

func (s *Server) adoptionReport(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.anyOrgView() {
		forbidden(w)
		return
	}
	as, ok := s.st.(store.AdoptionStore)
	if !ok {
		apiError(w, http.StatusNotImplemented, "not_implemented", "this store cannot report adoption")
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
	format := q.Get("format")
	if format == "" {
		format = FormatJSON
	}
	if format != FormatJSON && format != FormatCSV {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want json or csv")
		return
	}
	var orgs []string // nil: every organisation
	if !p.reportView() {
		os, err := s.st.ListOrgs(r.Context())
		if err != nil {
			storeError(w, err)
			return
		}
		orgs = []string{}
		for _, o := range os {
			if p.orgView(o.Name) {
				orgs = append(orgs, o.Name)
			}
		}
	}
	a, err := s.adoptionOf(r.Context(), as, from, to, orgs)
	if err != nil {
		storeError(w, err)
		return
	}
	k := s.adoptionMin()
	v := suppressAdoption(a, k)
	if format == FormatCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"series", "period_or_model", "active_users", "requests"})
		num := func(p *int64) string {
			if p == nil {
				return "suppressed"
			}
			return strconv.FormatInt(*p, 10)
		}
		emit := func(series string, ps []adoptionPoint) {
			for _, x := range ps {
				_ = cw.Write([]string{series, csvSafe(x.Period), num(x.ActiveUsers), num(x.Requests)})
			}
		}
		emit("day", v.Daily)
		emit("week", v.Weekly)
		for _, m := range v.Models {
			_ = cw.Write([]string{"model", csvSafe(m.Model), num(m.ActiveUsers), num(m.Requests)})
		}
		cw.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"from": from.UTC(), "to": to.UTC(), "active_users": v.ActiveUsers, "daily": v.Daily, "weekly": v.Weekly, "models": v.Models,
		"min_users": k, "note": "cells with fewer than min_users distinct people are suppressed",
	})
}
