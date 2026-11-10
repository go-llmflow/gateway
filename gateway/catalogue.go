package gateway

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-llmflow/gateway/config"
)

// The model catalogue ("AI Hub"): what the gateway serves, for the people who
// administer it, as JSON (GET /admin/models) and as a page (/ui/models). It
// reads the configuration and the live health of the upstreams, and nothing of
// a request. What a caller is allowed to use stays GET /v1/models; the
// catalogue says what each model is, what it costs and where it runs.
//
// Guard models (class "guard") are the gateway's own checks, not something a
// team calls: only a proxy admin or viewer sees them. The allow-lists of an
// external provider's model (which organisations and teams may send data off
// the site) are shown to them too, and to nobody else.

type catalogueEntry struct {
	ID            string   `json:"id"`
	UpstreamModel string   `json:"upstream_model"`
	Class         string   `json:"class"`
	Pool          string   `json:"pool"`
	ContextLength int64    `json:"context_length"`
	InputPerMTok  float64  `json:"input_per_mtok"`
	OutputPerMTok float64  `json:"output_per_mtok"`
	CachedPerMTok float64  `json:"cached_per_mtok,omitempty"`
	Unit          string   `json:"unit"`
	Upstreams     int      `json:"upstreams"`
	Healthy       int      `json:"healthy"`
	Regions       []string `json:"regions,omitempty"`
	RegionPolicy  string   `json:"region_policy,omitempty"`
	External      string   `json:"external_provider,omitempty"`
	AllowedOrgs   []string `json:"allowed_orgs,omitempty"`
	AllowedTeams  []string `json:"allowed_teams,omitempty"`
	Fallbacks     []string `json:"fallbacks,omitempty"`
}

// className names a model's class for the catalogue.
func className(c config.Class) string {
	switch c {
	case config.Guard:
		return "guard"
	case config.Batch:
		return "batch"
	}
	return "interactive"
}

// catalogue lists the models p may see.
func (s *Server) catalogue(p *principal) []catalogueEntry {
	now := s.now()
	out := make([]catalogueEntry, 0, len(s.gorder))
	for _, n := range s.gorder {
		g := s.groups[n]
		if g.cfg.Class == config.Guard && !p.proxyView() {
			continue
		}
		e := catalogueEntry{ID: n, UpstreamModel: g.cfg.UpstreamModel, Class: className(g.cfg.Class), Pool: g.cfg.Pool,
			ContextLength: g.contextLen(), InputPerMTok: g.cfg.Cost.InputPerMTok, OutputPerMTok: g.cfg.Cost.OutputPerMTok,
			CachedPerMTok: g.cfg.Cost.CachedPerMTok, Unit: g.cfg.Cost.Unit, Fallbacks: g.cfg.Fallbacks}
		if e.Unit == "" {
			e.Unit = "gpu_hours"
		}
		for _, u := range g.members() {
			e.Upstreams++
			if u.available(now) {
				e.Healthy++
			}
			if u.region != "" && !slices.Contains(e.Regions, u.region) {
				e.Regions = append(e.Regions, u.region)
			}
		}
		sort.Strings(e.Regions)
		if s.cfg.Region != "" {
			e.RegionPolicy = g.cfg.RegionPolicy
		}
		if g.ext != nil {
			e.External = g.ext.cfg.Provider
			if p.proxyView() {
				e.AllowedOrgs, e.AllowedTeams = g.ext.cfg.Orgs, g.ext.cfg.Teams
			}
		}
		out = append(out, e)
	}
	return out
}

// listModels is GET /admin/models.
func (s *Server) listModels(w http.ResponseWriter, _ *http.Request, p *principal) {
	writeJSON(w, http.StatusOK, s.catalogue(p))
}

// uiOrgs is the read-only page of organisations and teams, from the admin
// API as the caller's own role sees it (the API decides what is listed).
func (s *Server) uiOrgs(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	_, orgs := s.callAdmin(r, p, "GET /admin/orgs", "/admin/orgs", nil, nil, nil)
	_, teams := s.callAdmin(r, p, "GET /admin/teams", "/admin/teams", nil, nil, nil)
	data := struct{ Orgs, Teams table }{
		objectsTable(orgs, "name", "created_at", "disabled"),
		objectsTable(teams, "name", "org", "created_at", "disabled"),
	}
	s.render(w, "orgs", view{Title: "Organisations and teams", User: p.name, CSRF: csrf, Data: data})
}

// uiModels is the catalogue page.
func (s *Server) uiModels(w http.ResponseWriter, _ *http.Request, p *principal, csrf string) {
	t := table{Cols: []string{"model", "class", "context", "input /Mtok", "output /Mtok", "unit", "upstreams up", "regions", "notes"}}
	for _, e := range s.catalogue(p) {
		var notes []string
		if e.External != "" {
			notes = append(notes, "external provider "+e.External+": prompts leave the site")
		}
		if len(e.Fallbacks) > 0 {
			notes = append(notes, "falls back to "+strings.Join(e.Fallbacks, ", "))
		}
		if e.RegionPolicy == "require" {
			notes = append(notes, "stays in the gateway's region")
		}
		ctxLen := ""
		if e.ContextLength > 0 {
			ctxLen = strconv.FormatInt(e.ContextLength, 10)
		}
		t.Rows = append(t.Rows, []string{e.ID, e.Class, ctxLen, strconv.FormatFloat(e.InputPerMTok, 'g', 8, 64),
			strconv.FormatFloat(e.OutputPerMTok, 'g', 8, 64), e.Unit, strconv.Itoa(e.Healthy) + "/" + strconv.Itoa(e.Upstreams),
			strings.Join(e.Regions, ", "), strings.Join(notes, "; ")})
		t.IDs = append(t.IDs, e.ID)
	}
	s.render(w, "models", view{Title: "Models", User: p.name, CSRF: csrf, Data: t})
}
