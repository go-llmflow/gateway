package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-llmflow/gateway/internal/warnat"
)

// The admin UI of soft quotas (#96): who is above a threshold in the current
// window, and the thresholds of each level with their overrides. Like every
// page it calls the admin API in-process (authorisation, validation and audit
// are the API's).

func (s *Server) softQuotaUIRoutes(mux *http.ServeMux, page func(func(http.ResponseWriter, *http.Request, *principal, string)) http.Handler) {
	mux.Handle("GET /ui/softquotas", page(s.uiSoftQuotas))
	mux.Handle("POST /ui/softquotas", page(s.uiSetSoftQuota))
}

type sqLevelRow struct {
	Scope, Name, Label          string
	Config, Override, Effective string
	SetBy, Input                string
	CanSet                      bool
}

type sqStandingRow struct {
	standing
	Thresholds string
	Resets     string
	Bar        float64
}

func (s *Server) uiSoftQuotas(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.softQuotasPage(w, r, p, csrf, "", "")
}

func (s *Server) softQuotasPage(w http.ResponseWriter, r *http.Request, p *principal, csrf, msg, errMsg string) {
	q := url.Values{}
	for _, k := range []string{"org", "team"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	st, raw := s.callAdmin(r, p, "GET /admin/soft-quotas/standing", "/admin/soft-quotas/standing", q, nil, nil)
	var stand []standing
	if st != http.StatusOK {
		if errMsg == "" {
			errMsg = apiMessage(raw)
		}
	} else {
		_ = json.Unmarshal(raw, &stand)
	}
	rows := make([]sqStandingRow, 0, len(stand))
	for _, x := range stand {
		rows = append(rows, sqStandingRow{x, warnat.Format(x.WarnAt), s.resetText(x.ResetsAt, x.Window, "en"), min(x.UsedPct, 100)})
	}
	var levels struct {
		Gateway  levelOut   `json:"gateway"`
		Orgs     []levelOut `json:"orgs"`
		Profiles []levelOut `json:"profiles"`
	}
	st, raw = s.callAdmin(r, p, "GET /admin/soft-quotas", "/admin/soft-quotas", nil, nil, nil)
	if st != http.StatusOK {
		if errMsg == "" {
			errMsg = apiMessage(raw)
		}
	} else {
		_ = json.Unmarshal(raw, &levels)
	}
	row := func(scope, name, label string, l levelOut, can bool) sqLevelRow {
		return sqLevelRow{Scope: scope, Name: name, Label: label, Config: spell(l.Config, "not set"), Override: spell(l.Override, "—"),
			Effective: warnat.Format(l.Effective), SetBy: l.SetBy, Input: warnat.Format(l.Override), CanSet: can}
	}
	lr := []sqLevelRow{row("gateway", "", "Gateway default", levels.Gateway, p.proxyAdmin())}
	for _, o := range levels.Orgs {
		lr = append(lr, row("orgs", o.Name, "Organisation "+o.Name, o, p.orgAdmin(o.Name)))
	}
	for _, o := range levels.Profiles {
		lr = append(lr, row("profiles", o.Name, "Profile "+o.Name, o, p.proxyAdmin()))
	}
	data := struct {
		Standing []sqStandingRow
		Levels   []sqLevelRow
		Org      string
		Team     string
	}{rows, lr, q.Get("org"), q.Get("team")}
	s.render(w, "softquotas", view{Title: "Soft quotas", User: p.name, CSRF: csrf, Message: msg, Error: errMsg, Data: data})
}

// spell shows a list for a table cell: "off" for none, the fallback when
// the level sets nothing.
func spell(a []int, unset string) string {
	if a == nil {
		return unset
	}
	return warnat.Format(a)
}

func (s *Server) uiSetSoftQuota(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	scope, name := r.PostForm.Get("scope"), r.PostForm.Get("name")
	text := r.PostForm.Get("warn_at")
	var pattern, path string
	var vals map[string]string
	switch scope {
	case "gateway":
		pattern, path = "/admin/soft-quotas/gateway", "/admin/soft-quotas/gateway"
	case "orgs", "profiles":
		pattern, path, vals = "/admin/soft-quotas/"+scope+"/{name}", "/admin/soft-quotas/"+scope+"/"+url.PathEscape(name), map[string]string{"name": name}
	default:
		s.softQuotasPage(w, r, p, csrf, "", "scope: want gateway, orgs or profiles")
		return
	}
	levels, set, err := warnat.Parse(text)
	if err != nil {
		s.softQuotasPage(w, r, p, csrf, "", err.Error())
		return
	}
	var st int
	var raw []byte
	if !set {
		st, raw = s.callAdmin(r, p, "DELETE "+pattern, path, nil, nil, vals)
	} else {
		st, raw = s.callAdmin(r, p, "PUT "+pattern, path, nil, map[string]any{"warn_at": levels}, vals)
	}
	if st >= 300 {
		s.softQuotasPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	what := strings.TrimSpace(scope + " " + name)
	if !set {
		s.softQuotasPage(w, r, p, csrf, what+": back to the level above ("+strconv.Itoa(len(levels))+" thresholds set here)", "")
		return
	}
	s.softQuotasPage(w, r, p, csrf, what+": warn at "+warnat.Format(levels), "")
}
