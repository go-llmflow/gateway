package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Workload profiles (#90). A person's profile (assigned in the store, or
// the configuration's default_profile) becomes hard budgets of the user
// scope (#87), in every cost unit that has a workday value:
//
//	profile_week     week (ISO 8601: Monday 00:00 UTC)  days × workday
//	profile_session  rolling session_window (5h)        session_share × workday
//	profile_overflow month, when allowed for the person overflow_cap × workday
//
// The escrow enforces them like any budget: a profile is never exceeded.
// Beyond the weekly or session allowance, a person whose administrator
// allowed it continues on the overflow cap (the analogue of Anthropic's
// usage credits, capped per person per month); the requests admitted so
// are marked overflow and count for the overflow budget only.
//
// The gateway writes these budgets itself (never the budgets API): when an
// assignment changes, when the reconciliation loop finds them out of date
// (a new workday calibration, a changed profile block), and, for a person
// with the default profile seen for the first time, at their first request
// (which then waits for the new budgets to arm, at most profileArmWait).

// profileArmWait bounds how long a first request waits for its profile
// budgets, just created, to arm (every replica reports at once on the
// change notification; one replica alone at once).
const profileArmWait = 3 * time.Second

// workdayValue is a unit's workday in force and where it comes from.
type workdayValue struct {
	Amount float64   `json:"amount"`
	Source string    `json:"source"` // "calibration" or "configuration"
	SetBy  string    `json:"set_by,omitempty"`
	SetAt  time.Time `json:"set_at,omitzero"`
}

type profiler struct {
	cfg config.Profiles

	mu      sync.Mutex
	assign  map[string]store.Assignment
	workday map[string]workdayValue
	loaded  bool

	// ensuring: one creation of a person's budgets at a time.
	ensureMu sync.Mutex
	ensuring map[string]chan struct{}
}

func newProfiler(cfg config.Profiles) *profiler {
	pr := &profiler{cfg: cfg, assign: map[string]store.Assignment{}, workday: map[string]workdayValue{}, ensuring: map[string]chan struct{}{}}
	for u, w := range cfg.Workday {
		pr.workday[u] = workdayValue{Amount: w, Source: "configuration"}
	}
	return pr
}

// load reads the assignments and the accepted calibrations.
func (pr *profiler) load(ctx context.Context, st store.Store) error {
	as, err := st.Assignments(ctx)
	if err != nil {
		return err
	}
	ws, err := st.Workdays(ctx)
	if err != nil {
		return err
	}
	assign := make(map[string]store.Assignment, len(as))
	for _, a := range as {
		assign[a.User] = a
	}
	wd := map[string]workdayValue{}
	for u, w := range pr.cfg.Workday {
		wd[u] = workdayValue{Amount: w, Source: "configuration"}
	}
	// An accepted calibration takes precedence over the configured value.
	for _, w := range ws {
		wd[w.Unit] = workdayValue{Amount: w.Amount, Source: "calibration", SetBy: w.SetBy, SetAt: w.SetAt}
	}
	pr.mu.Lock()
	pr.assign, pr.workday, pr.loaded = assign, wd, true
	pr.mu.Unlock()
	return nil
}

// forUser is the profile in force for a person, whether they may overflow,
// and whether any profile applies to them.
func (pr *profiler) forUser(user string) (config.Profile, bool, bool) {
	if pr == nil || user == "" {
		return config.Profile{}, false, false
	}
	pr.mu.Lock()
	a, assigned := pr.assign[user]
	pr.mu.Unlock()
	id := pr.cfg.Default
	if assigned {
		id = a.Profile
	}
	p, ok := pr.cfg.Get(id)
	if !ok {
		return config.Profile{}, false, false
	}
	return p, assigned && a.Overflow && p.OverflowCap > 0, true
}

// assignment is a person's stored assignment, if any.
func (pr *profiler) assignment(user string) (store.Assignment, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	a, ok := pr.assign[user]
	return a, ok
}

func (pr *profiler) workdayOf(unit string) (float64, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	w, ok := pr.workday[unit]
	return w.Amount, ok
}

func (pr *profiler) workdays() map[string]workdayValue {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	out := make(map[string]workdayValue, len(pr.workday))
	for u, w := range pr.workday {
		out[u] = w
	}
	return out
}

// allowance is a profile's allowance in one unit, in that unit.
func allowance(kind store.ScopeKind, p config.Profile, workday float64) float64 {
	switch kind {
	case store.ScopeProfileWeek:
		return p.Days * workday
	case store.ScopeProfileSession:
		return p.SessionShare * workday
	case store.ScopeProfileOverflow:
		return p.OverflowCap * workday
	}
	return 0
}

// desired lists the budgets a person's profile becomes.
func (pr *profiler) desired(user string) []store.Budget {
	p, overflow, ok := pr.forUser(user)
	if !ok {
		return nil
	}
	session := store.RollingPeriod(pr.cfg.SessionWindow)
	var out []store.Budget
	for unit, w := range pr.workdays() {
		mk := func(kind store.ScopeKind, per store.Period) store.Budget {
			return store.Budget{BudgetKey: store.BudgetKey{Kind: kind, Name: user, Unit: unit}, Period: per, MaxCost: allowance(kind, p, w.Amount)}
		}
		out = append(out, mk(store.ScopeProfileWeek, store.Week), mk(store.ScopeProfileSession, session))
		if overflow {
			out = append(out, mk(store.ScopeProfileOverflow, store.Month))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BudgetKey.String() < out[j].BudgetKey.String() })
	return out
}

// sameBudget: a stored budget already says what is desired.
func sameBudget(a, b store.Budget) bool {
	return a.Period == b.Period && math.Abs(a.MaxCost-b.MaxCost) <= 1e-12*math.Max(1, math.Abs(b.MaxCost))
}

// applyProfile makes a person's profile budgets what their profile says:
// it creates, changes and deletes them (the limit forced: an administrator
// who lowers a profile stops new admissions at once, what runs finishes).
// It reports whether it changed anything.
func (s *Server) applyProfile(ctx context.Context, user string, have map[store.BudgetKey]store.Budget) (bool, error) {
	want := s.prof.desired(user)
	keep := map[store.BudgetKey]bool{}
	changed := false
	for _, b := range want {
		keep[b.BudgetKey] = true
		if old, ok := have[b.BudgetKey]; ok && sameBudget(old, b) {
			continue
		}
		if err := s.st.SetBudgetChecked(ctx, b, s.now(), s.cfg.Limits.MaxRequestDuration, true); err != nil {
			return changed, err
		}
		changed = true
	}
	for k := range have {
		if k.Kind.Profile() && k.Name == user && !keep[k] {
			if err := s.st.DeleteBudget(ctx, k); err != nil && !errors.Is(err, store.ErrNotFound) {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// profileBudgets is the profile budgets of the last refresh, by user.
func (s *Server) profileBudgets() map[string]map[store.BudgetKey]store.Budget {
	out := map[string]map[store.BudgetKey]store.Budget{}
	for _, b := range s.budget.list() {
		if !b.Kind.Profile() {
			continue
		}
		if out[b.Name] == nil {
			out[b.Name] = map[store.BudgetKey]store.Budget{}
		}
		out[b.Name][b.BudgetKey] = b
	}
	return out
}

// budgetsChanged makes this process and the others load changed budgets at
// once (setBudget does the same).
func (s *Server) budgetsChanged(ctx context.Context, what string) {
	if err := s.budget.refresh(ctx); err != nil {
		s.log.Error("budget refresh after a profile change failed", "err", err)
	}
	s.sendReports(ctx)
	s.escrow.forgetRoom()
	if s.shared != nil {
		if err := s.shared.NotifyKeys(ctx, "budget:changed/"+what); err != nil {
			s.log.Error("profile change not broadcast; the other replicas load it at their next refresh", "err", err)
		}
	}
}

// reconcileProfiles brings every person's profile budgets up to date: those
// with an assignment, and those who already have profile budgets (the
// default profile's people, someone whose assignment was removed).
func (s *Server) reconcileProfiles(ctx context.Context) error {
	if s.prof == nil {
		return nil
	}
	if err := s.prof.load(ctx, s.st); err != nil {
		return err
	}
	have := s.profileBudgets()
	users := map[string]bool{}
	for u := range have {
		users[u] = true
	}
	s.prof.mu.Lock()
	for u := range s.prof.assign {
		users[u] = true
	}
	s.prof.mu.Unlock()
	names := make([]string, 0, len(users))
	for u := range users {
		names = append(names, u)
	}
	sort.Strings(names)
	changed := 0
	var firstErr error
	for _, u := range names {
		c, err := s.applyProfile(ctx, u, have[u])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if c {
			changed++
		}
	}
	if changed > 0 {
		s.log.Info("workload profiles reconciled", "people", changed)
		s.budgetsChanged(ctx, "profiles")
	}
	return firstErr
}

func (s *Server) profilesLoop(ctx context.Context) {
	every := max(s.cfg.BudgetRefresh, 30*time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		if err := s.reconcileProfiles(rctx); err != nil {
			s.log.Error("workload profiles: reconciliation failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ensureProfile creates a person's profile budgets at their first request
// (one creation per person at a time; the others wait for it).
func (s *Server) ensureProfile(ctx context.Context, user string) error {
	s.prof.ensureMu.Lock()
	if ch, ok := s.prof.ensuring[user]; ok {
		s.prof.ensureMu.Unlock()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ch := make(chan struct{})
	s.prof.ensuring[user] = ch
	s.prof.ensureMu.Unlock()
	defer func() {
		s.prof.ensureMu.Lock()
		delete(s.prof.ensuring, user)
		s.prof.ensureMu.Unlock()
		close(ch)
	}()
	changed, err := s.applyProfile(ctx, user, s.profileBudgets()[user])
	if changed {
		s.audit("profile.applied", "user", user)
		s.budgetsChanged(ctx, "profile/"+user)
	}
	return err
}

// profileState is a request's standing under its person's profile.
type profileState struct {
	id       string
	overflow bool // the person may overflow
	created  bool // the budgets were just created: wait for them to arm
}

// profileAdmission finds a request's profile and makes sure its budgets
// exist in the request's unit. A priced request in a unit without a
// workday is refused: its cost could not be held against the profile.
func (s *Server) profileAdmission(ctx context.Context, g *group, scope reqScope) (profileState, error) {
	p, overflow, ok := s.prof.forUser(scope.user)
	if !ok {
		return profileState{}, nil
	}
	ps := profileState{id: p.ID, overflow: overflow}
	if g.prices.zero {
		return ps, nil
	}
	if _, ok := s.prof.workdayOf(scope.unit); !ok {
		return ps, &admitError{http.StatusServiceUnavailable, "service_unavailable",
			fmt.Sprintf("your workload profile (%s) has no workday value for %s yet: requests under a profile in this unit are refused until an administrator calibrates it",
				p.Label, store.UnitName(scope.unit)), "profile_uncalibrated", time.Time{}, store.BudgetKey{}}
	}
	if !s.budget.has(store.BudgetKey{Kind: store.ScopeProfileWeek, Name: scope.user, Unit: scope.unit}) {
		if err := s.ensureProfile(ctx, scope.user); err != nil {
			return ps, &admitError{http.StatusServiceUnavailable, "service_unavailable",
				"your workload profile's budgets could not be created; retry in a moment", "store_unavailable", time.Time{}, store.BudgetKey{}}
		}
		ps.created = true
	}
	return ps, nil
}

// fmtDays spells a number of workdays: "2 days", "0.5 day", "1.25 days".
func fmtDays(d float64) string {
	d = math.Round(d*100) / 100
	s := strconv.FormatFloat(d, 'f', -1, 64)
	if d > 0 && d <= 1 {
		return s + " day"
	}
	return s + " days"
}

// fmtWindow spells a session window: "5h", "90m".
func fmtWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m"
}

// profileRefusal answers a request that does not fit its person's profile,
// in workdays. It names the caller's own allowance only.
func (s *Server) profileRefusal(k store.BudgetKey, room, w int64, reset time.Time) (*admitError, bool) {
	wd, ok := s.prof.workdayOf(k.Unit)
	limit, have := s.budget.limitOf(k)
	if !ok || !have || wd <= 0 {
		return nil, false
	}
	days := func(nano int64) string { return fmtDays(float64(nano) / nanoPerUnit / wd) }
	when := reset.UTC().Format("Monday 2 Jan 15:04 UTC")
	var what, verb string
	switch k.Kind {
	case store.ScopeProfileWeek:
		what, verb = "weekly allowance of "+fmtDays(limit/wd), "resets"
	case store.ScopeProfileSession:
		what, verb = "session allowance of "+fmtDays(limit/wd)+" per "+fmtWindow(s.cfg.Profiles.SessionWindow), "room again from"
	case store.ScopeProfileOverflow:
		what, verb = "monthly overflow allowance of "+fmtDays(limit/wd), "resets"
	default:
		return nil, false
	}
	msg := fmt.Sprintf("%s used; %s %s", what, verb, when)
	if room > 0 {
		msg = fmt.Sprintf("%s: %s left, and this request may cost up to %s (send a smaller max_tokens, or wait: %s %s)",
			what, days(room), days(w), verb, when)
	}
	return &admitError{http.StatusTooManyRequests, "insufficient_quota", msg, "profile", reset, k}, true
}
