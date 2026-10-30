package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/internal/warnat"
	"github.com/go-llmflow/gateway/store"
)

// Soft quotas (#96): people are told before a window runs out, the way
// Anthropic warns at 75% and 95% of its session and weekly limits
// (code.claude.com/docs/en/errors.md "Usage limits"; claude-apps-gateway-
// spend-limits.md "Usage warnings in Claude Code").
//
// Claude Code's own warnings come from the anthropic-ratelimit-unified-*
// headers, whose exact format is documented only in a Claude apps gateway's
// own /protocol reference, which is not public: they are NOT emitted here
// (nothing is guessed). The warnings reach people through channels the
// gateway controls: response headers (X-Llmflow-Usage-*), GET /v1/me/usage
// (read by cmd/llmflow-status for Claude Code's statusLine), notifications,
// and the admin UI.
//
// What a person's windows are, everything from memory (the budgets of the last
// refresh plus what this process finished since):
//
//	session  the profile's rolling window (5 h of 5-minute buckets)
//	week     the ISO week, Monday 00:00 UTC
//	budget   a money budget with a user scope, in its own period
//
// each as a percentage of its limit, the largest over the units the person
// has (the most-consumed cap, as Claude Code's own warnings use).

const (
	windowSession = "session"
	windowWeek    = "week"
	// meUsageRPM bounds GET /v1/me/usage per key.
	meUsageRPM = 60
	// defaultWarnEvery is how often the thresholds are evaluated.
	defaultWarnEvery = 15 * time.Second
)

// Response headers (they name the caller's own windows only).
const (
	hdrUsageSession      = "X-Llmflow-Usage-Session"
	hdrUsageSessionReset = "X-Llmflow-Usage-Session-Reset"
	hdrUsageWeek         = "X-Llmflow-Usage-Week"
	hdrUsageWeekReset    = "X-Llmflow-Usage-Week-Reset"
	hdrUsageWarning      = "X-Llmflow-Usage-Warning"
)

// winUse is one window of one person.
type winUse struct {
	Window string
	Pct    float64
	Start  time.Time // the window instance (week start; zero for the rolling session)
	Reset  time.Time
	Unit   string
	// Rolling: the window slides (no instance id; see store/softquotas.go).
	Rolling bool
	// Period names a budget's period (windows of kind budget).
	Period store.Period
}

// warnLevels are the runtime overrides in force (loaded by the evaluator).
type warnLevels struct {
	gateway    []int
	org, prof  map[string][]int
	hasGateway bool
}

// warnState is the evaluator's state.
type warnState struct {
	levels atomic.Pointer[warnLevels]
	orgOf  atomic.Pointer[map[string]string]
	every  time.Duration

	mu sync.Mutex
	// local remembers the thresholds this process has already settled
	// (fired by it, or found fired by another replica), by person, window and
	// threshold, so that a threshold costs the store one call per crossing,
	// not one per round. The value is the window instance for a calendar
	// window, zero for a rolling one (cleared when it re-arms).
	local map[string]time.Time
	// checked: rolling windows whose re-arming this process has verified
	// with the store since usage last was high.
	checked map[string]bool

	// noDedup is the planted control of the property test: every replica
	// that sees a crossing fires it, without asking the store.
	noDedup bool
	// fired is told every notification this process fires (tests).
	fired func(user, window string, threshold int)
}

func newWarnState() *warnState {
	return &warnState{every: defaultWarnEvery, local: map[string]time.Time{}, checked: map[string]bool{}}
}

// WithWarnEvery sets how often soft-quota thresholds are evaluated (tests).
func WithWarnEvery(d time.Duration) Option { return func(s *Server) { s.warn.every = d } }

// ---- language, reset times, texts ----

// langOf is the language of a request's warnings: its Accept-Language (the
// first tag that is French or English), else the configured one.
func (s *Server) langOf(r *http.Request) string {
	if r != nil {
		for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
			tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
			switch {
			case tag == "fr" || strings.HasPrefix(tag, "fr-"):
				return config.LangFR
			case tag == "en" || strings.HasPrefix(tag, "en-"):
				return config.LangEN
			}
		}
	}
	if s.cfg.SoftQuotas.Language == config.LangFR {
		return config.LangFR
	}
	return config.LangEN
}

var (
	weekdaysEN = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	weekdaysFR = [...]string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}
)

// resetText spells when a window resets, in the configured zone, as
// Anthropic does: a time for the session ("15:45"), a day and a time for a
// longer window ("Mon 00:00", "lun. 00:00").
func (s *Server) resetText(t time.Time, window, lang string) string {
	loc := s.softLoc
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	hm := t.Format("15:04")
	if window == windowSession {
		return hm
	}
	if lang == config.LangFR {
		return weekdaysFR[t.Weekday()] + " " + hm
	}
	return weekdaysEN[t.Weekday()] + " " + hm
}

func windowNoun(window, lang string) string {
	switch {
	case window == windowSession && lang == config.LangFR:
		return "quota de session"
	case window == windowSession:
		return "session allowance"
	case window == windowWeek && lang == config.LangFR:
		return "quota hebdomadaire"
	case window == windowWeek:
		return "weekly allowance"
	case lang == config.LangFR:
		return "budget"
	}
	return "budget"
}

// warnText is the warning of a window past a threshold: "You've used 85% of
// your session allowance · resets 15:45".
func (s *Server) warnText(window string, pct float64, reset time.Time, lang string) string {
	n := int(math.Floor(pct))
	when := s.resetText(reset, window, lang)
	if lang == config.LangFR {
		return fmt.Sprintf("Vous avez utilisé %d %% de votre %s · réinitialisation %s", n, windowNoun(window, lang), frAt(window, when))
	}
	return fmt.Sprintf("You've used %d%% of your %s · resets %s", n, windowNoun(window, lang), when)
}

// frAt: "à 15:45" for a time, "lun. 00:00" for a day and a time.
func frAt(window, when string) string {
	if window == windowSession {
		return "à " + when
	}
	return when
}

// hitText is the refusal of a window that has run out, in Anthropic's
// words: "You've hit your session allowance · resets 15:45".
func (s *Server) hitText(what string, reset time.Time, lang string) string {
	window := what
	if what != windowSession && what != windowWeek {
		window = "other"
	}
	when := s.resetText(reset, window, lang)
	if lang == config.LangFR {
		return fmt.Sprintf("Vous avez atteint votre %s · réinitialisation %s", s.hitNoun(what, lang), frAt(window, when))
	}
	return fmt.Sprintf("You've hit your %s · resets %s", s.hitNoun(what, lang), when)
}

func (s *Server) hitNoun(what, lang string) string {
	switch what {
	case windowSession, windowWeek:
		return windowNoun(what, lang)
	case "overflow":
		if lang == config.LangFR {
			return "plafond de dépassement mensuel"
		}
		return "monthly overflow allowance"
	}
	return windowNoun("budget", lang)
}

// ---- the windows of a person ----

// eachWorkday calls f for every unit with a workday (under the lock).
func (pr *profiler) eachWorkday(f func(unit string, workday float64)) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for u, w := range pr.workday {
		f(u, w.Amount)
	}
}

// userWindows is a person's profile windows: the largest share used over
// their units. ok is false without a profile.
func (s *Server) userWindows(user string, now time.Time) (prof config.Profile, sess, week winUse, ok bool) {
	if s.prof == nil || user == "" {
		return
	}
	prof, _, ok = s.prof.forUser(user)
	if !ok {
		return
	}
	period := store.RollingPeriod(s.cfg.Profiles.SessionWindow)
	sess = winUse{Window: windowSession, Reset: period.ResetAt(now), Rolling: true}
	week = winUse{Window: windowWeek, Start: store.Week.Start(now), Reset: store.Week.ResetAt(now)}
	s.prof.eachWorkday(func(unit string, wd float64) {
		if wd <= 0 {
			return
		}
		if prof.Days > 0 {
			v, _ := s.budget.spent(store.BudgetKey{Kind: store.ScopeProfileWeek, Name: user, Unit: unit})
			if pct := 100 * v / wd / prof.Days; pct > week.Pct {
				week.Pct, week.Unit = pct, unit
			}
		}
		if prof.SessionShare > 0 {
			v, _ := s.budget.spent(store.BudgetKey{Kind: store.ScopeProfileSession, Name: user, Unit: unit})
			if pct := 100 * v / wd / prof.SessionShare; pct > sess.Pct {
				sess.Pct, sess.Unit = pct, unit
			}
		}
	})
	return prof, sess, week, true
}

// userBudgetWindows are a person's user-scope money budgets as windows.
func (s *Server) userBudgetWindows(user string, now time.Time) (ws []winUse, warn map[string][]int) {
	for _, b := range s.budget.list() {
		if b.Kind != store.ScopeUser || b.Name != user || b.MaxCost <= 0 {
			continue
		}
		spent, _ := s.budget.spent(b.BudgetKey)
		limit := b.Limit(now)
		if limit <= 0 {
			continue
		}
		_, rolling := b.Period.Rolling()
		w := winUse{Window: "budget:" + b.BudgetKey.String(), Pct: 100 * spent / limit, Unit: b.Unit,
			Start: b.Period.Start(now), Reset: b.Period.ResetAt(now), Rolling: rolling, Period: b.Period}
		if rolling {
			w.Start = time.Time{}
		}
		ws = append(ws, w)
		if warn == nil {
			warn = map[string][]int{}
		}
		warn[w.Window] = b.WarnAt
	}
	return ws, warn
}

// ---- thresholds ----

// warnAt is the list in force: the most specific level that is set wins
// (budget, profile, organisation, gateway); a runtime override replaces its
// level's configured value; Anthropic's [75, 95] when none is set.
func (s *Server) warnAt(budget []int, profile, org string) []int {
	lv := s.warn.levels.Load()
	if lv == nil {
		return s.cfg.WarnAtOf(budget, profile, org)
	}
	var p, o []int
	if v, ok := lv.prof[profile]; ok {
		p = v
	} else if pr, ok := s.cfg.Profiles.Get(profile); ok {
		p = pr.WarnAt
	}
	if v, ok := lv.org[org]; ok {
		o = v
	} else {
		for _, x := range s.cfg.Orgs {
			if x.Name == org {
				o = x.WarnAt
			}
		}
	}
	g := s.cfg.SoftQuotas.WarnAt
	if lv.hasGateway {
		g = lv.gateway
	}
	return warnat.Resolve(budget, p, o, g)
}

// userOrg is the organisation to apply thresholds of: the key's, when the
// request names one, else the first (by name) of the person's organisations.
func (s *Server) userOrg(user, keyOrg string) string {
	if keyOrg != "" {
		return keyOrg
	}
	if m := s.warn.orgOf.Load(); m != nil {
		return (*m)[user]
	}
	return ""
}

// highest is the largest threshold of levels that pct reached (0: none).
func highest(levels []int, pct float64) int {
	c := warnat.Crossed(levels, pct)
	if len(c) == 0 {
		return 0
	}
	return c[len(c)-1]
}

// ---- response headers ----

// usageHeaders puts a person's windows on a successful response: the
// session and the week, as a percentage and when they reset, and, once a
// threshold is passed, the warning in the request's language. They name the
// caller's own windows only; nothing is set without a profile.
func (s *Server) usageHeaders(h http.Header, r *http.Request, user, org string) {
	if s.prof == nil || user == "" {
		return
	}
	now := s.now()
	prof, sess, week, ok := s.userWindows(user, now)
	if !ok {
		return
	}
	h.Set(hdrUsageSession, strconv.Itoa(int(math.Round(sess.Pct))))
	h.Set(hdrUsageSessionReset, sess.Reset.UTC().Format(time.RFC3339))
	h.Set(hdrUsageWeek, strconv.Itoa(int(math.Round(week.Pct))))
	h.Set(hdrUsageWeekReset, week.Reset.UTC().Format(time.RFC3339))
	levels := s.warnAt(nil, prof.ID, s.userOrg(user, org))
	ts, tw := highest(levels, sess.Pct), highest(levels, week.Pct)
	if ts == 0 && tw == 0 {
		return
	}
	w := sess
	if tw > ts || tw == ts && week.Pct > sess.Pct {
		w = week
	}
	h.Set(hdrUsageWarning, headerSafe(s.warnText(w.Window, w.Pct, w.Reset, s.langOf(r))))
}

// ---- GET /v1/me/usage ----

type meWindow struct {
	UsedPct  float64   `json:"used_pct"`
	ResetsAt time.Time `json:"resets_at"`
	Start    time.Time `json:"window_start,omitzero"`
	Window   string    `json:"window,omitempty"`
	Level    int       `json:"warn_level,omitempty"`
	Unit     string    `json:"unit,omitempty"`
}

type meUsage struct {
	User     string     `json:"user"`
	Profile  *meProfile `json:"profile"`
	WarnAt   []int      `json:"warn_at"`
	Session  *meWindow  `json:"session"`
	Week     *meWindow  `json:"week"`
	Budgets  []meBudget `json:"budgets"`
	Warning  string     `json:"warning,omitempty"`
	Language string     `json:"language"`
	Now      time.Time  `json:"now"`
}

type meProfile struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Days  float64 `json:"days"`
}

type meBudget struct {
	Period   store.Period `json:"period"`
	Unit     string       `json:"unit"`
	UsedPct  float64      `json:"used_pct"`
	ResetsAt time.Time    `json:"resets_at"`
	WarnAt   []int        `json:"warn_at"`
}

// meUsageHandler answers a key's or a token's own windows: percentages,
// reset times, the profile in force and the thresholds. It is served from
// memory, rate limited per key, authenticated like every request, and never
// cached (the answer changes with every request the person makes).
func (s *Server) meUsageHandler(w http.ResponseWriter, r *http.Request) {
	// Whatever the outcome, even a refusal: no cache keeps one person's usage.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Authorization, X-Api-Key")
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	id := "usage|" + key.ID
	if err := s.limiter.admit(id, meUsageRPM, 0, 0); err != nil {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error())
		return
	}
	s.limiter.release(id, 0)
	user := recordUser(key)
	now := s.now()
	lang := s.langOf(r)
	out := meUsage{User: user, Language: lang, Now: now.UTC(), Budgets: []meBudget{}, WarnAt: []int{}}
	org := s.userOrg(user, key.Org)
	if prof, sess, week, ok := s.userWindows(user, now); ok {
		levels := s.warnAt(nil, prof.ID, org)
		out.Profile = &meProfile{ID: prof.ID, Label: prof.Label, Days: prof.Days}
		out.WarnAt = levels
		mk := func(u winUse) *meWindow {
			return &meWindow{UsedPct: round2(u.Pct), ResetsAt: u.Reset.UTC(), Start: u.Start, Window: u.Window, Level: highest(levels, u.Pct), Unit: store.UnitName(u.Unit)}
		}
		out.Session, out.Week = mk(sess), mk(week)
		ts, tw := out.Session.Level, out.Week.Level
		if ts != 0 || tw != 0 {
			win := sess
			if tw > ts || tw == ts && week.Pct > sess.Pct {
				win = week
			}
			out.Warning = s.warnText(win.Window, win.Pct, win.Reset, lang)
		}
	}
	if user != "" {
		bws, warn := s.userBudgetWindows(user, now)
		for _, b := range bws {
			levels := s.warnAt(warn[b.Window], "", org)
			out.Budgets = append(out.Budgets, meBudget{Period: b.Period, Unit: store.UnitName(b.Unit), UsedPct: round2(b.Pct), ResetsAt: b.Reset.UTC(), WarnAt: levels})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- who is above a threshold (admin) ----

// standing is a person above a threshold of a window.
type standing struct {
	User     string    `json:"user"`
	Profile  string    `json:"profile,omitempty"`
	Window   string    `json:"window"`
	UsedPct  float64   `json:"used_pct"`
	Level    int       `json:"warn_level"`
	WarnAt   []int     `json:"warn_at"`
	ResetsAt time.Time `json:"resets_at"`
}

// standings lists, for the people in users, each window past a threshold in
// the current window instance, the most used first.
func (s *Server) standings(users []string) []standing {
	now := s.now()
	var out []standing
	for _, u := range users {
		org := s.userOrg(u, "")
		if prof, sess, week, ok := s.userWindows(u, now); ok {
			levels := s.warnAt(nil, prof.ID, org)
			for _, w := range []winUse{sess, week} {
				if l := highest(levels, w.Pct); l > 0 {
					out = append(out, standing{User: u, Profile: prof.Label, Window: w.Window, UsedPct: round2(w.Pct), Level: l, WarnAt: levels, ResetsAt: w.Reset.UTC()})
				}
			}
		}
		bws, warn := s.userBudgetWindows(u, now)
		for _, b := range bws {
			levels := s.warnAt(warn[b.Window], "", org)
			if l := highest(levels, b.Pct); l > 0 {
				out = append(out, standing{User: u, Window: b.Window, UsedPct: round2(b.Pct), Level: l, WarnAt: levels, ResetsAt: b.Reset.UTC()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UsedPct != out[j].UsedPct {
			return out[i].UsedPct > out[j].UsedPct
		}
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		return out[i].Window < out[j].Window
	})
	return out
}

// ---- evaluation and notification ----

// warnLoop evaluates the thresholds of everyone with a window, every
// warn.every, on every replica: the store decides who fires (MarkWarning).
func (s *Server) warnLoop(ctx context.Context) {
	t := time.NewTicker(s.warn.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		s.warnRound(rctx)
		cancel()
	}
}

// loadWarnLevels reads the runtime overrides and the people's organisations.
func (s *Server) loadWarnLevels(ctx context.Context) error {
	ls, err := s.st.WarnLevels(ctx)
	if err != nil {
		return err
	}
	lv := &warnLevels{org: map[string][]int{}, prof: map[string][]int{}}
	for _, l := range ls {
		switch l.Scope {
		case store.WarnScopeGateway:
			lv.gateway, lv.hasGateway = l.WarnAt, true
		case store.WarnScopeOrg:
			lv.org[l.Name] = l.WarnAt
		case store.WarnScopeProfile:
			lv.prof[l.Name] = l.WarnAt
		}
	}
	s.warn.levels.Store(lv)
	binds, err := s.st.Bindings(ctx, "")
	if err != nil {
		return err
	}
	teams, err := s.st.ListTeams(ctx, "")
	if err != nil {
		return err
	}
	orgOfTeam := map[string]string{}
	for _, t := range teams {
		orgOfTeam[t.Name] = t.Org
	}
	m := map[string]string{}
	for _, b := range binds {
		o := b.Org
		if b.Team != "" {
			o = orgOfTeam[b.Team]
		}
		if o != "" && (m[b.User] == "" || o < m[b.User]) {
			m[b.User] = o
		}
	}
	s.warn.orgOf.Store(&m)
	return nil
}

// warnUsers lists the people with a window: those with profile budgets and
// those with a user budget.
func (s *Server) warnUsers() []string {
	seen := map[string]bool{}
	for u := range s.profileBudgets() {
		seen[u] = true
	}
	for _, b := range s.budget.list() {
		if b.Kind == store.ScopeUser {
			seen[b.Name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// warnRound is one evaluation.
func (s *Server) warnRound(ctx context.Context) {
	if err := s.loadWarnLevels(ctx); err != nil {
		s.log.Warn("soft quotas: levels not loaded; the previous ones apply", "err", err)
	}
	now := s.now()
	for _, u := range s.warnUsers() {
		org := s.userOrg(u, "")
		if prof, sess, week, ok := s.userWindows(u, now); ok {
			levels := s.warnAt(nil, prof.ID, org)
			s.warnWindow(ctx, u, sess, levels)
			s.warnWindow(ctx, u, week, levels)
		}
		bws, warn := s.userBudgetWindows(u, now)
		for _, b := range bws {
			s.warnWindow(ctx, u, b, s.warnAt(warn[b.Window], "", org))
		}
	}
}

// warnWindow settles every threshold of one window of one person.
func (s *Server) warnWindow(ctx context.Context, user string, w winUse, levels []int) {
	ws := s.warn
	crossed := warnat.Crossed(levels, w.Pct)
	instance := w.Start
	if w.Rolling {
		instance = store.BucketOf(s.now())
	}
	// A rolling window re-arms what it fired once usage has fallen enough
	// (the local memory and, for the other replicas, the store).
	if w.Rolling {
		var drop []string
		ws.mu.Lock()
		for k := range ws.local {
			if !strings.HasPrefix(k, user+"\x00"+w.Window+"\x00") {
				continue
			}
			th, _ := strconv.Atoi(k[strings.LastIndexByte(k, 0)+1:])
			if w.Pct < float64(warnat.RearmBelow(th)) {
				drop = append(drop, k)
			}
		}
		ws.mu.Unlock()
		// A process that has just started (or whose memory was lost: it was
		// killed) remembers nothing it fired, so a descent would never re-arm
		// what an earlier process fired. Once per person and window, while
		// usage is low, it asks the store to: a cheap UPDATE that changes
		// nothing when nothing was fired (found by the three-replica test).
		ck := user + "\x00" + w.Window
		low := len(levels) > 0 && w.Pct < float64(warnat.RearmBelow(levels[0]))
		ws.mu.Lock()
		verify := low && !ws.checked[ck]
		if !low {
			delete(ws.checked, ck)
		}
		ws.mu.Unlock()
		if len(drop) > 0 || verify {
			if _, err := s.st.RearmWarnings(ctx, user, w.Window, w.Pct); err != nil {
				s.log.Warn("soft quotas: re-arm failed", "err", err)
			} else {
				ws.mu.Lock()
				for _, k := range drop {
					delete(ws.local, k)
				}
				if low {
					ws.checked[ck] = true
				}
				ws.mu.Unlock()
			}
		}
	}
	for i, th := range crossed {
		k := user + "\x00" + w.Window + "\x00" + strconv.Itoa(th)
		ws.mu.Lock()
		at, known := ws.local[k]
		ws.mu.Unlock()
		if known && (w.Rolling || at.Equal(instance)) {
			continue
		}
		top := i == len(crossed)-1
		var out []store.WarnOutbox
		var pl quotaPayload
		if top {
			pl = s.quotaPayloadFor(ctx, user, w, th)
			out = s.quotaOutbox(pl)
		}
		fired := true
		if !ws.noDedup {
			var err error
			fired, err = s.st.MarkWarning(ctx, store.WarnMark{User: user, Window: w.Window, Threshold: th, Instance: instance, Rolling: w.Rolling, At: s.now()}, s.outboxFor(out))
			if err != nil {
				s.log.Warn("soft quotas: threshold not recorded; retried next round", "err", err)
				continue
			}
		}
		ws.mu.Lock()
		if w.Rolling {
			ws.local[k] = time.Time{}
		} else {
			ws.local[k] = instance
		}
		ws.mu.Unlock()
		if !fired || !top {
			continue
		}
		s.warnFired(pl, out)
	}
}

// outboxFor: the rows go in the mark's transaction only with the shared
// outbox (several replicas); alone, the notification is sent directly.
func (s *Server) outboxFor(out []store.WarnOutbox) []store.WarnOutbox {
	if s.shared == nil {
		return nil
	}
	return out
}

// warnFired is what the replica that won a threshold does: count it, audit
// it and, without the shared outbox, send it (with the outbox, MarkWarning
// has queued it).
func (s *Server) warnFired(pl quotaPayload, out []store.WarnOutbox) {
	s.met.softAlerts.add(1, "soft_quota")
	s.audit("soft_quota.warned", "user", pl.User, "window", pl.Window, "threshold", pl.Threshold, "used_pct", pl.UsedPct)
	if f := s.warn.fired; f != nil {
		f(pl.User, pl.Window, pl.Threshold)
	}
	if s.alerts != nil && s.shared == nil {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			s.alerts.sendQuota(s.alertCtx, pl)
		}()
	}
}
