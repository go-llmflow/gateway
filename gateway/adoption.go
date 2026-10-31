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
// a week or a model used by 1 to k-1 distinct people is suppressed, and models
// suppressed together (two or more) are merged into one "(other models)" row
// with their requests only. A figure over one person is that person; a
// service key's requests count as requests and as no one.

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

// suppressAdoption applies the small-cell rule.
func suppressAdoption(a *store.Adoption, k int) adoptionView {
	var v adoptionView
	if !hidden(a.Users, k) {
		v.ActiveUsers = ptr(a.Users)
	}
	pts := func(in []store.AdoptionPoint) []adoptionPoint {
		out := make([]adoptionPoint, 0, len(in))
		for _, x := range in {
			if hidden(x.ActiveUsers, k) {
				out = append(out, adoptionPoint{Period: x.Day, Suppressed: true})
				continue
			}
			out = append(out, adoptionPoint{Period: x.Day, ActiveUsers: ptr(x.ActiveUsers), Requests: ptr(x.Requests)})
		}
		return out
	}
	v.Daily, v.Weekly = pts(a.Daily), pts(a.Weekly)
	v.Models = []adoptionModel{}
	var merged, mergedN int64
	for _, m := range a.Models {
		if hidden(m.Users, k) {
			merged += m.Requests
			mergedN++
			continue
		}
		v.Models = append(v.Models, adoptionModel{Model: m.Model, ActiveUsers: ptr(m.Users), Requests: ptr(m.Requests)})
	}
	// Two suppressed models can be the same person: the merged row carries its
	// requests only if the DISTINCT people over the union (a.OtherUsers, a
	// union, never a sum) are k or more; otherwise it carries no count at all.
	if mergedN >= 2 {
		row := adoptionModel{Model: OtherModels, Suppressed: true}
		if a.OtherKnown && !hidden(a.OtherUsers, k) && a.OtherUsers > 0 {
			row.Suppressed, row.ActiveUsers, row.Requests = false, ptr(a.OtherUsers), ptr(merged)
		}
		v.Models = append(v.Models, row)
	}
	return v
}

// adoptionOf reads the adoption of a period and measures the distinct people
// over the models the small-cell rule will merge.
func (s *Server) adoptionOf(ctx context.Context, as store.AdoptionStore, from, to time.Time, orgs []string) (*store.Adoption, error) {
	a, err := as.Adoption(ctx, from, to, orgs)
	if err != nil {
		return nil, err
	}
	k := s.adoptionMin()
	var hiddenModels []string
	for _, m := range a.Models {
		if hidden(m.Users, k) {
			hiddenModels = append(hiddenModels, m.Model)
		}
	}
	if us, ok := as.(store.AdoptionUnionStore); ok && len(hiddenModels) >= 2 {
		if a.OtherUsers, err = us.UsersOfModels(ctx, from, to, orgs, hiddenModels); err != nil {
			return nil, err
		}
		a.OtherKnown = true
	}
	return a, nil
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
