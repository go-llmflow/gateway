package gateway

import (
	"net/http"
	"sort"
	"strings"

	"github.com/go-llmflow/gateway/internal/warnat"
	"github.com/go-llmflow/gateway/store"
)

// The admin API of soft quotas (#96): the thresholds in force at each level,
// their runtime overrides, and who is above one now.
//
// Who may change what: the gateway default and a profile's thresholds, the
// proxy admins; an organisation's, that organisation's admins (and the proxy
// admins). Reading follows the organisation view rule. Every change is audited
// (the admin API's audit wrapper) and applies on this replica at once and on
// the others at their next evaluation (warn.every).

func (s *Server) softQuotaRoutes(h func(string, adminHandler)) {
	h("GET /admin/soft-quotas", s.listSoftQuotas)
	h("GET /admin/soft-quotas/standing", s.softQuotaStanding)
	h("PUT /admin/soft-quotas/gateway", s.setSoftQuota)
	h("DELETE /admin/soft-quotas/gateway", s.deleteSoftQuota)
	h("PUT /admin/soft-quotas/orgs/{name}", s.setSoftQuota)
	h("DELETE /admin/soft-quotas/orgs/{name}", s.deleteSoftQuota)
	h("PUT /admin/soft-quotas/profiles/{name}", s.setSoftQuota)
	h("DELETE /admin/soft-quotas/profiles/{name}", s.deleteSoftQuota)
}

// levelOut is one level's thresholds: configured, overridden, in force.
type levelOut struct {
	Name      string `json:"name,omitempty"`
	Config    []int  `json:"config"`
	Override  []int  `json:"override"`
	Effective []int  `json:"effective"`
	SetBy     string `json:"set_by,omitempty"`
}

func (s *Server) listSoftQuotas(w http.ResponseWriter, r *http.Request, p *principal) {
	ls, err := s.st.WarnLevels(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	over := map[[2]string]store.WarnLevel{}
	for _, l := range ls {
		over[[2]string{l.Scope, l.Name}] = l
	}
	mk := func(scope, name string, cfg []int) levelOut {
		o := levelOut{Name: name, Config: cfg}
		eff := cfg
		if l, ok := over[[2]string{scope, name}]; ok {
			o.Override, o.SetBy = l.WarnAt, l.SetBy
			eff = l.WarnAt
		}
		o.Effective = eff
		return o
	}
	gw := mk(store.WarnScopeGateway, "", s.cfg.SoftQuotas.WarnAt)
	if gw.Effective == nil {
		gw.Effective = warnat.Default()
	}
	orgs, profs := []levelOut{}, []levelOut{}
	for _, o := range s.cfg.Orgs {
		if p.orgView(o.Name) {
			orgs = append(orgs, mk(store.WarnScopeOrg, o.Name, o.WarnAt))
		}
	}
	for _, pr := range s.cfg.Profiles.List {
		profs = append(profs, mk(store.WarnScopeProfile, pr.ID, pr.WarnAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": warnat.Default(), "gateway": gw, "orgs": orgs, "profiles": profs,
		"can_set_gateway": p.proxyAdmin()})
}

// softQuotaTarget resolves a PUT or DELETE route to its level, checking the
// principal may change it; it writes the error itself.
func (s *Server) softQuotaTarget(w http.ResponseWriter, r *http.Request, p *principal) (scope, name string, ok bool) {
	switch {
	case r.PathValue("name") == "":
		scope = store.WarnScopeGateway
	case strings.HasPrefix(r.URL.Path, "/admin/soft-quotas/orgs/"):
		scope, name = store.WarnScopeOrg, r.PathValue("name")
	default:
		scope, name = store.WarnScopeProfile, r.PathValue("name")
	}
	if err := store.ValidWarnLevel(scope, name, []int{}); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return "", "", false
	}
	known := false
	switch scope {
	case store.WarnScopeGateway:
		known = true
	case store.WarnScopeOrg:
		for _, o := range s.cfg.Orgs {
			known = known || o.Name == name
		}
		if !known {
			if orgs, err := s.st.ListOrgs(r.Context()); err == nil {
				for _, o := range orgs {
					known = known || o.Name == name
				}
			}
		}
	case store.WarnScopeProfile:
		_, known = s.cfg.Profiles.Get(name)
	}
	// Authorised before existence is revealed.
	allowed := p.proxyAdmin() || scope == store.WarnScopeOrg && p.orgAdmin(name)
	if !allowed {
		forbidden(w)
		return "", "", false
	}
	if !known {
		apiError(w, http.StatusNotFound, "not_found", scope+" "+name+": unknown")
		return "", "", false
	}
	return scope, name, true
}

func (s *Server) setSoftQuota(w http.ResponseWriter, r *http.Request, p *principal) {
	scope, name, ok := s.softQuotaTarget(w, r, p)
	if !ok {
		return
	}
	var in struct {
		WarnAt *[]int `json:"warn_at"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.WarnAt == nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "warn_at: required (a list of 1 to 5 increasing percentages from 1 to 99, or [] for none)")
		return
	}
	levels := *in.WarnAt
	if levels == nil {
		levels = []int{}
	}
	if err := store.ValidWarnLevel(scope, name, levels); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.st.SetWarnLevels(r.Context(), scope, name, levels, p.name); err != nil {
		storeError(w, err)
		return
	}
	_ = s.loadWarnLevels(r.Context())
	s.audit("soft_quota.levels", "by", p.name, "scope", scope, "name", name, "warn_at", warnat.Format(levels))
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "name": name, "warn_at": levels})
}

func (s *Server) deleteSoftQuota(w http.ResponseWriter, r *http.Request, p *principal) {
	scope, name, ok := s.softQuotaTarget(w, r, p)
	if !ok {
		return
	}
	if err := s.st.SetWarnLevels(r.Context(), scope, name, nil, p.name); err != nil {
		storeError(w, err)
		return
	}
	_ = s.loadWarnLevels(r.Context())
	s.audit("soft_quota.levels", "by", p.name, "scope", scope, "name", name, "warn_at", "inherit")
	w.WriteHeader(http.StatusNoContent)
}

// softQuotaStanding lists who is above a threshold in the current window,
// among the people the principal may read (the profiles page's rule).
func (s *Server) softQuotaStanding(w http.ResponseWriter, r *http.Request, p *principal) {
	ctx := r.Context()
	binds, err := s.st.Bindings(ctx, "")
	if err != nil {
		storeError(w, err)
		return
	}
	teams, err := s.teamsByName(r)
	if err != nil {
		storeError(w, err)
		return
	}
	byUser := map[string][]store.Binding{}
	for _, b := range binds {
		byUser[b.User] = append(byUser[b.User], b)
	}
	var cands []string
	for _, u := range s.warnUsers() {
		cands = append(cands, u)
	}
	sort.Strings(cands)
	q := r.URL.Query()
	org, team := q.Get("org"), q.Get("team")
	var users []string
	for _, u := range cands {
		read, _ := personAccess(p, u, byUser[u], teams)
		if !read || !inScope(byUser[u], teams, org, team) {
			continue
		}
		if len(users) == maxPeople {
			break
		}
		users = append(users, u)
	}
	out := s.standings(users)
	if out == nil {
		out = []standing{}
	}
	writeJSON(w, http.StatusOK, out)
}
