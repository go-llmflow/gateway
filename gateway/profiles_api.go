package gateway

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// The admin API of workload profiles (#90).
//
// Who may assign: a proxy admin anyone; an organisation admin a person all
// of whose role bindings are in their organisation; a team admin a person
// all of whose bindings are in teams they administer. A person with a
// binding elsewhere (another organisation, a proxy role) or with none (a
// JWT's user) is the proxy admins' only: a profile limits the person
// everywhere, so one organisation must not decide it for another
// (security audit #90: no cross-org assignment). Reading follows the user
// budgets' rule (userBudgetAccess). Every change is audited.

func (s *Server) profileRoutes(h func(string, adminHandler)) {
	h("GET /admin/profiles", s.listProfiles)
	h("GET /admin/profiles/people", s.profilePeople)
	h("GET /admin/profiles/capacity", s.profileCapacity)
	h("GET /admin/profiles/calibrate", s.profileCalibration)
	h("POST /admin/profiles/calibrate", s.acceptCalibration)
	h("GET /admin/users/{name}/profile", s.getUserProfile)
	h("PUT /admin/users/{name}/profile", s.setUserProfile)
	h("DELETE /admin/users/{name}/profile", s.deleteUserProfile)
}

func noProfiles(w http.ResponseWriter) {
	apiError(w, http.StatusNotFound, "not_found", "no workload profiles are configured (profile blocks)")
}

type profileOut struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	Days         float64 `json:"days"`
	SessionShare float64 `json:"session_share"`
	OverflowCap  float64 `json:"overflow_cap,omitempty"`
}

func (s *Server) listProfiles(w http.ResponseWriter, _ *http.Request, _ *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
	ps := []profileOut{}
	for _, p := range s.cfg.Profiles.List {
		ps = append(ps, profileOut{p.ID, p.Label, p.Days, p.SessionShare, p.OverflowCap})
	}
	wd := map[string]workdayValue{}
	for u, v := range s.prof.workdays() {
		wd[store.UnitName(u)] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps, "default_profile": s.cfg.Profiles.Default,
		"session_window": fmtWindow(s.cfg.Profiles.SessionWindow), "workday": wd})
}

// personAccess decides from a person's bindings whether p reads and
// assigns their profile (teams: every team, by name).
//
// Who a person belongs to is decided by their TEAM bindings only (a team
// membership, which SSO and SCIM also create). An organisation-level binding
// (viewer or org_admin of an organisation) is authority granted to a person,
// not membership: an organisation admin can grant one to any existing user
// (canGrant), so counting it would let them claim anybody by granting their
// own organisation's viewer role first (#99). Such bindings neither give
// access nor are required for it; a person with no team binding, or with a
// proxy role, is the proxy admins' only. Writing needs every team of the
// person to be the caller's (a person in two organisations is decided by
// neither alone).
func personAccess(p *principal, user string, binds []store.Binding, teams map[string]store.Team) (read, write bool) {
	read, write = p.proxyView() || p.name == user && !p.master, p.proxyAdmin()
	if write {
		return read, write
	}
	all, member := true, false
	for _, b := range binds {
		switch {
		case b.Team != "":
			member = true
			t, ok := teams[b.Team]
			if !ok {
				all = false
				continue
			}
			read = read || p.teamAdmin(t) || p.orgView(t.Org) || p.has(store.RoleViewer, "", t.Name)
			all = all && p.teamAdmin(t)
		case b.Org != "":
			// not membership: see above
		default: // a proxy role: the proxy admins' only
			all = false
		}
	}
	return read, all && member
}

// validPerson accepts a name that can be a person's: a gateway user name,
// or a JWT's synthetic id.
func validPerson(name string) bool {
	if config.ValidUserName(name) {
		return true
	}
	if !strings.HasPrefix(name, "jwt-") || len(name) > 140 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x21 || name[i] > 0x7e {
			return false
		}
	}
	return true
}

// userProfileAccess loads a person's bindings and decides.
func (s *Server) userProfileAccess(ctx context.Context, p *principal, user string) (read, write bool, err error) {
	binds, err := s.st.Bindings(ctx, user)
	if err != nil {
		return false, false, err
	}
	teams := map[string]store.Team{}
	for _, b := range binds {
		if b.Team == "" {
			continue
		}
		t, err := s.st.GetTeam(ctx, b.Team)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, false, err
		}
		if err == nil {
			teams[t.Name] = t
		}
	}
	read, write = personAccess(p, user, binds, teams)
	return read, write, nil
}

// unitUse is a person's standing in one unit, in workdays.
type unitUse struct {
	Unit         string    `json:"unit"`
	Workday      float64   `json:"workday"`
	WeekDays     float64   `json:"week_days"`
	WeekUsedDays float64   `json:"week_used_days"`
	WeekResets   time.Time `json:"week_resets"`
	// WeekStart identifies the weekly window instance (the ISO week's
	// Monday 00:00 UTC); WeekPct and SessionPct are the shares used.
	WeekStart      time.Time `json:"week_start"`
	WeekPct        float64   `json:"week_used_pct"`
	SessionPct     float64   `json:"session_used_pct"`
	SessionDays    float64   `json:"session_days"`
	SessionUsed    float64   `json:"session_used_days"`
	SessionWindow  string    `json:"session_window"`
	SessionWhole   time.Time `json:"session_whole_at"`
	OverflowDays   float64   `json:"overflow_days,omitempty"`
	OverflowUsed   float64   `json:"overflow_used_days,omitempty"`
	OverflowResets time.Time `json:"overflow_resets,omitzero"`
}

// person is a person's profile and use.
type person struct {
	User     string    `json:"user"`
	Profile  string    `json:"profile"`
	Label    string    `json:"label"`
	Days     float64   `json:"days"`
	Assigned bool      `json:"assigned"`
	Overflow bool      `json:"overflow"`
	SetBy    string    `json:"set_by,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Units    []unitUse `json:"units"`
	Write    bool      `json:"can_assign"`
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// personOf is a person's standing, from the budgets of the last refresh.
func (s *Server) personOf(user string) person {
	prof, overflow, ok := s.prof.forUser(user)
	out := person{User: user, Units: []unitUse{}}
	if a, assigned := s.prof.assignment(user); assigned {
		out.Assigned, out.SetBy, out.Since = true, a.SetBy, a.From
	}
	if !ok {
		return out
	}
	out.Profile, out.Label, out.Days, out.Overflow = prof.ID, prof.Label, prof.Days, overflow
	now := s.now()
	session := store.RollingPeriod(s.cfg.Profiles.SessionWindow)
	wds := s.prof.workdays()
	units := make([]string, 0, len(wds))
	for u := range wds {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, u := range units {
		wd := wds[u].Amount
		used := func(kind store.ScopeKind) float64 {
			v, _ := s.budget.spent(store.BudgetKey{Kind: kind, Name: user, Unit: u})
			return round2(v / wd)
		}
		uu := unitUse{Unit: store.UnitName(u), Workday: wd, WeekDays: prof.Days, WeekUsedDays: used(store.ScopeProfileWeek),
			WeekResets: store.Week.ResetAt(now), SessionDays: prof.SessionShare, SessionUsed: used(store.ScopeProfileSession),
			SessionWindow: fmtWindow(s.cfg.Profiles.SessionWindow), SessionWhole: session.ResetAt(now)}
		uu.WeekStart = store.Week.Start(now)
		if uu.WeekDays > 0 {
			uu.WeekPct = round2(100 * uu.WeekUsedDays / uu.WeekDays)
		}
		if uu.SessionDays > 0 {
			uu.SessionPct = round2(100 * uu.SessionUsed / uu.SessionDays)
		}
		if overflow {
			uu.OverflowDays, uu.OverflowUsed, uu.OverflowResets = prof.OverflowCap, used(store.ScopeProfileOverflow), store.Month.ResetAt(now)
		}
		out.Units = append(out.Units, uu)
	}
	return out
}

// maxPeople bounds a listing.
const maxPeople = 2000

// profilePeople lists the people the principal may read: those with a
// role binding in its scope, those with an assignment, and those whose
// profile budgets exist (the default profile's people seen so far).
func (s *Server) profilePeople(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
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
	users := map[string]bool{}
	for u := range byUser {
		users[u] = true
	}
	for _, a := range s.mustAssignments() {
		users[a.User] = true
	}
	for u := range s.profileBudgets() {
		users[u] = true
	}
	q := r.URL.Query()
	org, team := q.Get("org"), q.Get("team")
	names := make([]string, 0, len(users))
	for u := range users {
		names = append(names, u)
	}
	sort.Strings(names)
	out := []person{}
	for _, u := range names {
		read, write := personAccess(p, u, byUser[u], teams)
		if !read || !inScope(byUser[u], teams, org, team) {
			continue
		}
		if len(out) == maxPeople {
			break
		}
		pe := s.personOf(u)
		pe.Write = write
		out = append(out, pe)
	}
	writeJSON(w, http.StatusOK, out)
}

// inScope filters people by an org or a team they hold a binding in.
func inScope(binds []store.Binding, teams map[string]store.Team, org, team string) bool {
	if org == "" && team == "" {
		return true
	}
	for _, b := range binds {
		o := b.Org
		if b.Team != "" {
			o = teams[b.Team].Org
		}
		if (team == "" || b.Team == team) && (org == "" || o == org) {
			return true
		}
	}
	return false
}

func (s *Server) mustAssignments() []store.Assignment {
	s.prof.mu.Lock()
	defer s.prof.mu.Unlock()
	out := make([]store.Assignment, 0, len(s.prof.assign))
	for _, a := range s.prof.assign {
		out = append(out, a)
	}
	return out
}

func (s *Server) getUserProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
	user := r.PathValue("name")
	if !validPerson(user) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "user name: want [A-Za-z0-9_.@+-]{1,128} or a JWT user id")
		return
	}
	read, write, err := s.userProfileAccess(r.Context(), p, user)
	if err != nil {
		storeError(w, err)
		return
	}
	if !read {
		forbidden(w)
		return
	}
	hist, err := s.st.AssignmentHistory(r.Context(), user)
	if err != nil {
		storeError(w, err)
		return
	}
	pe := s.personOf(user)
	pe.Write = write
	writeJSON(w, http.StatusOK, map[string]any{"person": pe, "history": hist})
}

func (s *Server) setUserProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Profile  string `json:"profile"`
		Overflow bool   `json:"overflow"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.assignProfile(w, r, p, in.Profile, in.Overflow)
}

func (s *Server) deleteUserProfile(w http.ResponseWriter, r *http.Request, p *principal) {
	s.assignProfile(w, r, p, "", false)
}

// assignProfile sets (profile "": removes) a person's assignment and
// applies it at once.
func (s *Server) assignProfile(w http.ResponseWriter, r *http.Request, p *principal, id string, overflow bool) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
	user := r.PathValue("name")
	if !validPerson(user) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "user name: want [A-Za-z0-9_.@+-]{1,128} or a JWT user id")
		return
	}
	if id != "" {
		prof, ok := s.cfg.Profiles.Get(id)
		if !ok {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "profile "+strconv.Quote(id)+": no such profile")
			return
		}
		if overflow && prof.OverflowCap == 0 {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "profile "+strconv.Quote(id)+" has no overflow_cap: overflow cannot be allowed on it")
			return
		}
	} else if overflow {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "overflow needs a profile")
		return
	}
	ctx := r.Context()
	_, write, err := s.userProfileAccess(ctx, p, user)
	if err != nil {
		storeError(w, err)
		return
	}
	if !write {
		forbidden(w)
		return
	}
	if err := s.st.AssignProfile(ctx, store.Assignment{User: user, Profile: id, Overflow: overflow, SetBy: p.name, From: s.now()}); err != nil {
		storeError(w, err)
		return
	}
	if err := s.prof.load(ctx, s.st); err != nil {
		storeError(w, err)
		return
	}
	changed, err := s.applyProfile(ctx, user, s.profileBudgets()[user])
	if changed {
		s.budgetsChanged(ctx, "profile/"+user)
	}
	if err != nil {
		// Stored: the reconciliation loop applies it.
		s.log.Error("profile assigned but its budgets not yet applied", "user", user, "err", err)
	}
	s.log.Info("profile assigned", "user", user, "profile", id, "overflow", overflow, "by", p.name)
	pe := s.personOf(user)
	pe.Write = true
	writeJSON(w, http.StatusOK, pe)
}

// ---- capacity ----

// orgCapacity is an organisation's assigned days against what its share
// of the pools serves, in one unit.
type orgCapacity struct {
	Org          string   `json:"org"`
	People       int      `json:"people"`
	AssignedDays float64  `json:"assigned_days_per_week"`
	Unit         string   `json:"unit"`
	Capacity     *float64 `json:"capacity_days_per_week"`
	Ratio        *float64 `json:"oversubscription"`
	Note         string   `json:"note,omitempty"`
}

// capacityWindow is how much measured load the capacity check reads.
const capacityWindow = 7 * 24 * time.Hour

// capacityOf computes, per unit with a workday, the workdays a week an
// organisation's share of the pools serves: share × C × the measured cost
// per slot-hour × 168 h ÷ workday. A pool without measured load counts as
// unmeasured: the result is then nil with the reason, never a guess.
func capacityOf(cfg *config.Config, org string, loads []store.PoolLoad, wds map[string]workdayValue) map[string]struct {
	days *float64
	note string
} {
	share := cfg.OrgShare(org)
	cap := map[string]int{}
	for _, p := range cfg.Pools {
		cap[p.Name] = p.Capacity
	}
	out := map[string]struct {
		days *float64
		note string
	}{}
	for unit, wd := range wds {
		total, measured, unmeasured := 0.0, 0, []string{}
		for _, p := range cfg.Pools {
			inUnit := false
			for _, g := range cfg.ModelGroups {
				if g.Pool == p.Name && g.Cost.Unit == unit {
					inUnit = true
				}
			}
			if !inUnit {
				continue
			}
			var l *store.PoolLoad
			for i := range loads {
				if loads[i].Pool == p.Name && loads[i].Unit == unit {
					l = &loads[i]
				}
			}
			if l == nil || l.BusySeconds <= 0 || l.Cost <= 0 {
				unmeasured = append(unmeasured, p.Name)
				continue
			}
			perSlotHour := l.Cost / (l.BusySeconds / 3600)
			total += share * float64(cap[p.Name]) * perSlotHour * 168 / wd.Amount
			measured++
		}
		v := struct {
			days *float64
			note string
		}{}
		switch {
		case len(unmeasured) > 0:
			v.note = "throughput not measured yet for pool " + strings.Join(unmeasured, ", ") + " (no request in the last 7 days): no capacity is inferred"
		case measured == 0:
			v.note = "no pool serves this unit (an external provider is billed per request, without a capacity)"
		default:
			d := round2(total)
			v.days = &d
		}
		out[unit] = v
	}
	return out
}

func (s *Server) profileCapacity(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
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
	now := s.now()
	loads, err := s.st.PoolLoad(ctx, now.Add(-capacityWindow), now)
	if err != nil {
		storeError(w, err)
		return
	}
	// The people of each organisation: a binding in it or in one of its
	// teams. A person in two organisations counts in both.
	people := map[string]map[string]bool{}
	for _, b := range binds {
		o := b.Org
		if b.Team != "" {
			o = teams[b.Team].Org
		}
		if o == "" {
			continue
		}
		if people[o] == nil {
			people[o] = map[string]bool{}
		}
		people[o][b.User] = true
	}
	orgs, err := s.st.ListOrgs(ctx)
	if err != nil {
		storeError(w, err)
		return
	}
	wds := s.prof.workdays()
	out := []orgCapacity{}
	for _, o := range orgs {
		if o.Disabled || !p.orgView(o.Name) {
			continue
		}
		days, n := 0.0, 0
		for u := range people[o.Name] {
			if prof, _, ok := s.prof.forUser(u); ok {
				days += prof.Days
				n++
			}
		}
		caps := capacityOf(s.cfg, o.Name, loads, wds)
		units := make([]string, 0, len(caps))
		for u := range caps {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			c := caps[u]
			oc := orgCapacity{Org: o.Name, People: n, AssignedDays: round2(days), Unit: store.UnitName(u), Capacity: c.days, Note: c.note}
			if c.days != nil && *c.days > 0 {
				ratio := round2(days / *c.days)
				oc.Ratio = &ratio
			}
			out = append(out, oc)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- calibration ----

// calibration is the measured distribution of active person-days in one
// unit, and the workday it proposes (its 75th percentile).
type calibration struct {
	Unit     string    `json:"unit"`
	Weeks    int       `json:"weeks"`
	Days     int       `json:"active_person_days"`
	P50      float64   `json:"p50"`
	P75      float64   `json:"p75"`
	P90      float64   `json:"p90"`
	Max      float64   `json:"max"`
	Proposal float64   `json:"proposal"`
	Current  *float64  `json:"current,omitempty"`
	Source   string    `json:"current_source,omitempty"`
	Buckets  []float64 `json:"histogram_upper_bounds"`
	Counts   []int     `json:"histogram_counts"`
}

// percentile is the nearest-rank percentile of sorted values.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// calibrate builds the distribution of a unit's cost per active
// person-day (a person with a cost that day) from report rows by user and
// day.
func calibrate(rows []store.ReportRow, unit string, weeks int) calibration {
	var xs []float64
	for _, r := range rows {
		if r.Unit == unit && r.User != "" && r.Cost > 0 {
			xs = append(xs, r.Cost)
		}
	}
	sort.Float64s(xs)
	c := calibration{Unit: store.UnitName(unit), Weeks: weeks, Days: len(xs), Buckets: []float64{}, Counts: []int{}}
	if len(xs) == 0 {
		return c
	}
	c.P50, c.P75, c.P90, c.Max = percentile(xs, 0.5), percentile(xs, 0.75), percentile(xs, 0.9), xs[len(xs)-1]
	c.Proposal = c.P75
	const bins = 10
	for i := 1; i <= bins; i++ {
		c.Buckets = append(c.Buckets, c.Max*float64(i)/bins)
		c.Counts = append(c.Counts, 0)
	}
	for _, x := range xs {
		i := int(math.Ceil(x/c.Max*bins)) - 1
		c.Counts[min(max(i, 0), bins-1)]++
	}
	return c
}

func (s *Server) profileCalibration(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
	if !p.proxyView() {
		forbidden(w) // it reads every person's usage
		return
	}
	weeks := 4
	if v := r.URL.Query().Get("weeks"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 26 {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "weeks: want 1..26")
			return
		}
		weeks = n
	}
	now := s.now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC) // whole days only
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	rows, err := s.st.Report(ctx, to.AddDate(0, 0, -7*weeks), to, []string{"user", "day"})
	if err != nil {
		storeError(w, err)
		return
	}
	wds := s.prof.workdays()
	out := []calibration{}
	for _, u := range s.cfg.Units() {
		c := calibrate(rows, u, weeks)
		if v, ok := wds[u]; ok {
			cur := v.Amount
			c.Current, c.Source = &cur, v.Source
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// acceptCalibration records a workday a proxy admin accepted (usually a
// proposal of profileCalibration) and applies it to every profile budget.
func (s *Server) acceptCalibration(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.prof == nil {
		noProfiles(w)
		return
	}
	if !p.proxyAdmin() {
		forbidden(w)
		return
	}
	var in struct {
		Unit   string  `json:"unit"`
		Amount float64 `json:"amount"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	unit, err := s.budgetUnit(in.Unit)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.st.SetWorkday(r.Context(), store.Workday{Unit: unit, Amount: in.Amount, SetBy: p.name, SetAt: s.now()}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			storeError(w, err)
			return
		}
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.reconcileProfiles(r.Context()); err != nil {
		s.log.Error("calibration stored; profile budgets not all applied yet", "err", err)
	}
	s.log.Info("workday calibrated", "unit", store.UnitName(unit), "amount", in.Amount, "by", p.name)
	writeJSON(w, http.StatusOK, map[string]any{"unit": store.UnitName(unit), "amount": in.Amount})
}
