package gateway

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-llmflow/gateway/store"
)

// GET /admin/adoption?from=&to=&format=json|csv (#97): active people per day
// and per week, requests, and the models used, as counts. No name, key or
// content leaves it. The proxy roles and finance_viewer read every
// organisation, org_admin and org-level viewer their own.

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
	a, err := as.Adoption(r.Context(), from, to, orgs)
	if err != nil {
		storeError(w, err)
		return
	}
	if format == FormatCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"series", "period_or_model", "active_users", "requests"})
		emit := func(series string, ps []store.AdoptionPoint) {
			for _, x := range ps {
				_ = cw.Write([]string{series, csvSafe(x.Day), strconv.FormatInt(x.ActiveUsers, 10), strconv.FormatInt(x.Requests, 10)})
			}
		}
		emit("day", a.Daily)
		emit("week", a.Weekly)
		for _, m := range a.Models {
			_ = cw.Write([]string{"model", csvSafe(m.Model), strconv.FormatInt(m.Users, 10), strconv.FormatInt(m.Requests, 10)})
		}
		cw.Flush()
		return
	}
	nz := func(v []store.AdoptionPoint) []store.AdoptionPoint {
		if v == nil {
			return []store.AdoptionPoint{}
		}
		return v
	}
	models := a.Models
	if models == nil {
		models = []store.AdoptionModel{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"from": from.UTC(), "to": to.UTC(), "active_users": a.Users, "daily": nz(a.Daily), "weekly": nz(a.Weekly), "models": models,
	})
}
