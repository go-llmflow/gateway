package gateway

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// adminHandler is an admin route handler with its authenticated principal.
type adminHandler func(w http.ResponseWriter, r *http.Request, p *principal)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	h := func(pattern string, f adminHandler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			p, ok := s.adminPrincipal(w, r)
			if !ok {
				return
			}
			f(w, r, p)
		})
	}
	h("GET /admin/whoami", s.whoami)
	h("GET /admin/orgs", s.listOrgs)
	h("POST /admin/orgs", s.createOrg)
	h("DELETE /admin/orgs/{name}", s.disableOrg)
	h("GET /admin/teams", s.listTeams)
	h("POST /admin/teams", s.createTeam)
	h("DELETE /admin/teams/{name}", s.disableTeam)
	h("GET /admin/teams/{team}/projects", s.listProjects)
	h("POST /admin/teams/{team}/projects", s.createProject)
	h("DELETE /admin/teams/{team}/projects/{name}", s.disableProject)
	h("GET /admin/keys", s.listKeys)
	h("POST /admin/keys", s.createKey)
	h("DELETE /admin/keys/{id}", s.revokeKey)
	h("GET /admin/users", s.listUsers)
	h("POST /admin/users", s.createUser)
	h("DELETE /admin/users/{name}", s.disableUser)
	h("GET /admin/users/{name}/tokens", s.listTokens)
	h("POST /admin/users/{name}/tokens", s.createToken)
	h("DELETE /admin/users/{name}/tokens/{id}", s.revokeToken)
	h("GET /admin/users/{name}/roles", s.listRoles)
	h("PUT /admin/users/{name}/roles", s.grantRole)
	h("DELETE /admin/users/{name}/roles", s.ungrantRole)
	h("GET /admin/budgets", s.listBudgets)
	h("PUT /admin/budgets/{team}", s.setBudget)
	h("DELETE /admin/budgets/{team}", s.deleteBudget)
	h("GET /admin/usage.csv", s.usageCSV)
	h("GET /admin/fairshare", s.fairshareState)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrExists):
		apiError(w, http.StatusConflict, "conflict", "already exists")
	case errors.Is(err, store.ErrConflict):
		apiError(w, http.StatusConflict, "conflict", "conflicts with an existing object")
	default:
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "store error")
	}
}

func badName(w http.ResponseWriter, what string) {
	apiError(w, http.StatusBadRequest, "invalid_request_error", what+" name: want [A-Za-z0-9_.-]{1,64}")
}

// team loads a team for a permission check, writing the error itself.
func (s *Server) team(w http.ResponseWriter, r *http.Request, name string) (store.Team, bool) {
	t, err := s.st.GetTeam(r.Context(), name)
	if err != nil {
		storeError(w, err)
		return store.Team{}, false
	}
	return t, true
}

// teamsByName returns every team, for filtering listings.
func (s *Server) teamsByName(r *http.Request) (map[string]store.Team, error) {
	ts, err := s.st.ListTeams(r.Context(), "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.Team, len(ts))
	for _, t := range ts {
		out[t.Name] = t
	}
	return out, nil
}

func (s *Server) whoami(w http.ResponseWriter, _ *http.Request, p *principal) {
	bs := p.bindings
	if bs == nil {
		bs = []store.Binding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": p.name, "master": p.master, "roles": bs})
}

// ---- organisations ----

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request, p *principal) {
	os, err := s.st.ListOrgs(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	type org struct {
		store.Org
		// Share is the configured share of every pool (0 for an
		// organisation created through the API: it only borrows).
		Share float64 `json:"share"`
	}
	out := []org{}
	for _, o := range os {
		if p.orgView(o.Name) {
			out = append(out, org{o, s.cfg.OrgShare(o.Name)})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyAdmin() {
		forbidden(w)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !config.ValidName(in.Name) {
		badName(w, "organisation")
		return
	}
	if err := s.st.CreateOrg(r.Context(), in.Name); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": in.Name, "share": s.cfg.OrgShare(in.Name)})
}

func (s *Server) disableOrg(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyAdmin() {
		forbidden(w)
		return
	}
	name := r.PathValue("name")
	if err := s.st.DisableOrg(r.Context(), name); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(k store.Key) bool { return k.Org == name })
	w.WriteHeader(http.StatusNoContent)
}

// ---- teams and projects ----

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request, p *principal) {
	ts, err := s.st.ListTeams(r.Context(), r.URL.Query().Get("org"))
	if err != nil {
		storeError(w, err)
		return
	}
	out := []store.Team{}
	for _, t := range ts {
		if p.teamView(t) {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Name string `json:"name"`
		Org  string `json:"org"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !config.ValidName(in.Name) {
		badName(w, "team")
		return
	}
	if !config.ValidName(in.Org) {
		badName(w, "organisation")
		return
	}
	if !p.orgAdmin(in.Org) {
		forbidden(w)
		return
	}
	if err := s.st.CreateTeam(r.Context(), in.Name, in.Org); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": in.Name, "org": in.Org})
}

func (s *Server) disableTeam(w http.ResponseWriter, r *http.Request, p *principal) {
	t, ok := s.team(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if !p.orgAdmin(t.Org) {
		forbidden(w)
		return
	}
	if err := s.st.DisableTeam(r.Context(), t.Name); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(k store.Key) bool { return k.Team == t.Name })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, p *principal) {
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.teamView(t) {
		forbidden(w)
		return
	}
	ps, err := s.st.ListProjects(r.Context(), t.Name)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !config.ValidName(in.Name) {
		badName(w, "project")
		return
	}
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.teamAdmin(t) {
		forbidden(w)
		return
	}
	if err := s.st.CreateProject(r.Context(), t.Name, in.Name); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"team": t.Name, "name": in.Name})
}

func (s *Server) disableProject(w http.ResponseWriter, r *http.Request, p *principal) {
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.teamAdmin(t) {
		forbidden(w)
		return
	}
	name := r.PathValue("name")
	if err := s.st.DisableProject(r.Context(), t.Name, name); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(k store.Key) bool { return k.Team == t.Name && k.Project == name })
	w.WriteHeader(http.StatusNoContent)
}

// ---- keys ----

// keyVisible: administrators and viewers of the team see all its keys; a
// member sees only the keys they own.
func keyVisible(p *principal, t store.Team, k store.Key) bool {
	if p.teamAdmin(t) || p.orgView(t.Org) || p.has(store.RoleViewer, "", t.Name) {
		return true
	}
	return p.teamMember(t) && k.Owner != "" && k.Owner == p.name
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p *principal) {
	ks, err := s.st.ListKeys(r.Context(), r.URL.Query().Get("team"))
	if err != nil {
		storeError(w, err)
		return
	}
	teams, err := s.teamsByName(r)
	if err != nil {
		storeError(w, err)
		return
	}
	out := []store.Key{}
	for _, k := range ks {
		if keyVisible(p, teams[k.Team], k) {
			out = append(out, k)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Team        string `json:"team"`
		Project     string `json:"project"`
		Name        string `json:"name"`
		RPM         int    `json:"rpm"`
		TPM         int    `json:"tpm"`
		MaxInFlight int    `json:"max_inflight"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.RPM < 0 || in.TPM < 0 || in.MaxInFlight < 0 || len(in.Name) > 128 {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "limits must be non-negative and the name at most 128 bytes")
		return
	}
	t, ok := s.team(w, r, in.Team)
	if !ok {
		return
	}
	owner := ""
	switch {
	case p.teamAdmin(t):
	case p.teamMember(t):
		owner = p.name // a member's key is theirs
	default:
		forbidden(w)
		return
	}
	secret, id, hash := store.GenerateKey()
	k := store.Key{ID: id, Hash: hash, Team: in.Team, Project: in.Project, Owner: owner, Name: in.Name,
		RPM: in.RPM, TPM: in.TPM, MaxInFlight: in.MaxInFlight}
	if err := s.st.CreateKey(r.Context(), k); err != nil {
		storeError(w, err)
		return
	}
	s.log.Info("key created", "key", id, "team", in.Team, "project", in.Project, "by", p.name)
	// The secret is returned once and never logged or stored.
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "team": in.Team, "project": in.Project, "owner": owner, "key": secret})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, p *principal) {
	k, err := s.st.GetKey(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	t, ok := s.team(w, r, k.Team)
	if !ok {
		return
	}
	if !p.teamAdmin(t) && !(p.teamMember(t) && k.Owner != "" && k.Owner == p.name) {
		forbidden(w)
		return
	}
	if err := s.st.RevokeKey(r.Context(), k.ID); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(c store.Key) bool { return c.ID == k.ID })
	w.WriteHeader(http.StatusNoContent)
}

// ---- users, tokens, roles ----

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, p *principal) {
	us, err := s.st.ListUsers(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	if p.proxyView() {
		writeJSON(w, http.StatusOK, us)
		return
	}
	// An organisation admin sees the users holding a role in their
	// organisation or one of its teams.
	bs, err := s.st.Bindings(r.Context(), "")
	if err != nil {
		storeError(w, err)
		return
	}
	teams, err := s.teamsByName(r)
	if err != nil {
		storeError(w, err)
		return
	}
	visible := map[string]bool{p.name: true}
	for _, b := range bs {
		org := b.Org
		if b.Team != "" {
			org = teams[b.Team].Org
		}
		if org != "" && p.orgAdmin(org) {
			visible[b.User] = true
		}
	}
	out := []store.User{}
	for _, u := range us {
		if visible[u.Name] {
			out = append(out, u)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.anyOrgAdmin() {
		forbidden(w)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !config.ValidName(in.Name) {
		badName(w, "user")
		return
	}
	if err := s.st.CreateUser(r.Context(), in.Name); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": in.Name})
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyAdmin() {
		forbidden(w)
		return
	}
	if err := s.st.DisableUser(r.Context(), r.PathValue("name")); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// selfOrProxyAdmin guards a user's own tokens.
func selfOrProxyAdmin(p *principal, user string) bool {
	return p.proxyAdmin() || p.name == user && !p.master
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p *principal) {
	user := r.PathValue("name")
	if !selfOrProxyAdmin(p, user) {
		forbidden(w)
		return
	}
	ts, err := s.st.ListUserTokens(r.Context(), user)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p *principal) {
	user := r.PathValue("name")
	if !selfOrProxyAdmin(p, user) {
		forbidden(w)
		return
	}
	secret, id, hash := store.GenerateAdminToken()
	if err := s.st.CreateUserToken(r.Context(), store.UserToken{ID: id, Hash: hash, User: user}); err != nil {
		storeError(w, err)
		return
	}
	s.log.Info("admin token created", "user", user, "token", id, "by", p.name)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "user": user, "token": secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p *principal) {
	user := r.PathValue("name")
	if !selfOrProxyAdmin(p, user) {
		forbidden(w)
		return
	}
	if err := s.st.RevokeUserToken(r.Context(), user, r.PathValue("id")); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request, p *principal) {
	user := r.PathValue("name")
	bs, err := s.st.Bindings(r.Context(), user)
	if err != nil {
		storeError(w, err)
		return
	}
	if p.proxyView() || p.name == user && !p.master {
		writeJSON(w, http.StatusOK, bs)
		return
	}
	out := []store.Binding{}
	for _, b := range bs {
		if ok, err := s.canGrant(r.Context(), p, b); err == nil && ok {
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) bindingFrom(w http.ResponseWriter, r *http.Request, p *principal, b store.Binding) bool {
	if err := b.Validate(); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return false
	}
	ok, err := s.canGrant(r.Context(), p, b)
	if err != nil {
		storeError(w, err)
		return false
	}
	if !ok {
		forbidden(w)
		return false
	}
	return true
}

func (s *Server) grantRole(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Role store.Role `json:"role"`
		Org  string     `json:"org"`
		Team string     `json:"team"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b := store.Binding{User: r.PathValue("name"), Role: in.Role, Org: in.Org, Team: in.Team}
	if !s.bindingFrom(w, r, p, b) {
		return
	}
	if err := s.st.Grant(r.Context(), b); err != nil {
		storeError(w, err)
		return
	}
	s.log.Info("role granted", "user", b.User, "role", b.Role, "org", b.Org, "team", b.Team, "by", p.name)
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) ungrantRole(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	b := store.Binding{User: r.PathValue("name"), Role: store.Role(q.Get("role")), Org: q.Get("org"), Team: q.Get("team")}
	if !s.bindingFrom(w, r, p, b) {
		return
	}
	if err := s.st.Ungrant(r.Context(), b); err != nil {
		storeError(w, err)
		return
	}
	s.log.Info("role removed", "user", b.User, "role", b.Role, "org", b.Org, "team", b.Team, "by", p.name)
	w.WriteHeader(http.StatusNoContent)
}

// ---- budgets ----

func (s *Server) listBudgets(w http.ResponseWriter, r *http.Request, p *principal) {
	bs, err := s.st.Budgets(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	teams, err := s.teamsByName(r)
	if err != nil {
		storeError(w, err)
		return
	}
	out := []store.Budget{}
	for _, b := range bs {
		if p.teamView(teams[b.Team]) {
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setBudget(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Period  store.Period `json:"period"`
		MaxCost float64      `json:"max_cost"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b := store.Budget{Team: r.PathValue("team"), Period: in.Period, MaxCost: in.MaxCost}
	if err := store.ValidateBudget(b); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	t, ok := s.team(w, r, b.Team)
	if !ok {
		return
	}
	if !p.orgAdmin(t.Org) {
		forbidden(w)
		return
	}
	if err := s.st.SetBudget(r.Context(), b); err != nil {
		storeError(w, err)
		return
	}
	_ = s.budget.refresh(r.Context())
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteBudget(w http.ResponseWriter, r *http.Request, p *principal) {
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.orgAdmin(t.Org) {
		forbidden(w)
		return
	}
	if err := s.st.DeleteBudget(r.Context(), t.Name); err != nil {
		storeError(w, err)
		return
	}
	_ = s.budget.refresh(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ---- reports ----

// ParseDay accepts RFC 3339 or YYYY-MM-DD (UTC midnight).
func ParseDay(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

func (s *Server) usageCSV(w http.ResponseWriter, r *http.Request, p *principal) {
	from, err1 := ParseDay(r.URL.Query().Get("from"))
	to, err2 := ParseDay(r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	us, err := s.st.Usage(r.Context(), from, to)
	if err != nil {
		storeError(w, err)
		return
	}
	if !p.proxyView() {
		teams, err := s.teamsByName(r)
		if err != nil {
			storeError(w, err)
			return
		}
		kept := us[:0]
		for _, u := range us {
			if t, ok := teams[u.Team]; ok && p.teamView(t) {
				kept = append(kept, u)
			}
		}
		us = kept
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	_ = WriteUsageCSV(w, from, to, us)
}

// WriteUsageCSV writes the per-team GPU-hour export. Fields are written by
// encoding/csv; a team name cannot start a spreadsheet formula because
// names are restricted to [A-Za-z0-9_.-].
func WriteUsageCSV(w io.Writer, from, to time.Time, us []store.TeamUsage) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"from", "to", "team", "pool", "requests", "prompt_tokens", "completion_tokens", "cached_tokens", "gpu_hours", "estimated_requests"})
	for _, u := range us {
		_ = cw.Write([]string{
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), u.Team, u.Pool,
			strconv.FormatInt(u.Requests, 10), strconv.FormatInt(u.PromptTokens, 10),
			strconv.FormatInt(u.CompletionTokens, 10), strconv.FormatInt(u.CachedTokens, 10),
			strconv.FormatFloat(u.GPUHours, 'f', 6, 64), strconv.FormatInt(u.EstimatedRequests, 10),
		})
	}
	cw.Flush()
	return cw.Error()
}

func (s *Server) fairshareState(w http.ResponseWriter, _ *http.Request, p *principal) {
	type orgOut struct {
		Org            string  `json:"org"`
		Share          float64 `json:"share"`
		Reserved       int     `json:"reserved"`
		InFlight       int     `json:"inflight"`
		Borrowing      int     `json:"borrowing"`
		InFlightTokens int64   `json:"inflight_tokens"`
		Usage          float64 `json:"usage"`
		LevelFairshare string  `json:"level_fairshare"`
		Rank           int     `json:"rank"`
	}
	out := map[string][]orgOut{}
	for _, pool := range s.porder {
		out[pool] = []orgOut{}
		for _, t := range s.pools[pool].Snapshot() {
			if !p.orgView(t.Team) {
				continue
			}
			out[pool] = append(out[pool], orgOut{t.Team, t.Share, t.Reserved, t.InFlight, t.Borrowing, t.InFlightTokens,
				t.Usage, formatFloat(t.LevelFairshare), t.Rank})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
