package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

// Admission under hard budgets (#87): before a request is sent, its
// worst-case cost W (bound.go) is held on every budget it falls under
// (escrow.go). The gateway always sends an explicit max_tokens under a
// budget: the client's (refused, never lowered, when W does not fit), or,
// when the client sent none, the largest that fits, down to
// budgets.min_max_tokens, said in X-Llmflow-Max-Tokens.

// admission is a request's budget standing.
type admission struct {
	hold *hold // nil: no budget applies, or the request is free
	m    int64 // the output bound written into the body
	set  bool  // m is written into the body
	// picked: the client sent no bound and the gateway chose m.
	picked bool
	at     time.Time
	// reset is when the tightest budget the request falls under resets
	// (X-Llmflow-Budget-Reset); zero without a budget.
	reset time.Time
	// id is the request's record id (the arming tracker's key); recorded
	// is set once its record is handed to the recorder, which untracks it
	// when stored.
	id       string
	recorded bool
	// profile is the person's workload profile in force ("" for none);
	// overflow: admitted beyond it, on its overflow cap (#90).
	profile  string
	overflow bool
}

// finish untracks a request that leaves no record (refused, or ended
// before reaching vLLM).
func (s *Server) finishUnrecorded(a *admission) {
	if a != nil && !a.recorded {
		s.tracker.untrack(a.id)
	}
}

// admitError is an admission refusal with its HTTP answer.
type admitError struct {
	status  int
	typ     string
	msg     string
	denyTag string
	// reset: when the budget that refused resets.
	reset time.Time
	// key is the budget that refused (zero for another refusal).
	key store.BudgetKey
}

// budgetResetHeader carries, on a refusal, when the budget that refused
// resets, and on success when the tightest one does (RFC 3339, UTC). It is
// a time only: it names no scope, the caller's or another's.
const budgetResetHeader = "X-Llmflow-Budget-Reset"

func (e *admitError) Error() string { return e.msg }

// budgetRefusal is a request that does not fit a budget of its scope. The
// message names only the caller's own scope (the key's org, team, project,
// key, tags, the person), its room left and the request's maximum cost; a
// workload profile's budget is spelled in workdays (profiles.go).
func (s *Server) budgetRefusal(k store.BudgetKey, room, w int64, unit string, reset time.Time) *admitError {
	if k.Kind.Profile() && s.prof != nil {
		if ae, ok := s.profileRefusal(k, room, w, reset); ok {
			return ae
		}
	}
	m := fmt.Sprintf("budget exhausted for this period (%s %s", k.Kind, k.Name)
	if k.Model != "" {
		m += ", model " + k.Model
	}
	m += fmt.Sprintf("): %s %s left, and this request may cost up to %s; send a smaller max_tokens",
		store.FormatNano(room), store.UnitName(unit), store.FormatNano(w))
	return &admitError{http.StatusTooManyRequests, "insufficient_quota", m, "budget", reset, k}
}

// armingRefusal answers a request under a budget that is still arming
// (arming.go): it names the caller's own scope only.
func armingRefusal(k store.BudgetKey) *admitError {
	s := fmt.Sprintf("the budget of this period (%s %s", k.Kind, k.Name)
	if k.Model != "" {
		s += ", model " + k.Model
	}
	s += ") was just created and is arming: requests in its scope admitted before it are still running or being recorded; retry in a moment"
	return &admitError{http.StatusTooManyRequests, "insufficient_quota", s, "budget_arming", time.Time{}, k}
}

var (
	errNoContextLen = errors.New("the context length of this model is unknown (set max_model_len, or wait for its upstreams to report it on /v1/models): requests are refused while a budget applies")
)

// admitBudget holds a request's worst-case cost and returns the body to
// send (with its max_tokens). out is the rewritten body for g.
//
// id names the request's record: the request is tracked under it until the
// record is stored (arming.go), whether a budget applies or not.
//
// Under a workload profile (#90) it first makes sure the person's profile
// budgets exist, and, when the profile's weekly or session allowance
// refuses the request and the person may overflow, admits it on the
// overflow cap instead (the request is then marked overflow).
func (s *Server) admitBudget(ctx context.Context, ep openai.Endpoint, g *group, scope reqScope, out []byte, betas []string, id string) (*admission, []byte, error) {
	if s.budget.stale() {
		return nil, nil, &admitError{http.StatusServiceUnavailable, "service_unavailable", errStateStale.Error(), "store_unavailable", time.Time{}, store.BudgetKey{}}
	}
	if s.prof == nil || scope.user == "" || s.softBudgets {
		return s.admitScope(ctx, ep, g, scope, out, betas, id)
	}
	scope.overflow = false
	ps, err := s.profileAdmission(ctx, g, scope)
	if err != nil {
		return nil, nil, err
	}
	admit := func(sc reqScope) (*admission, []byte, error) {
		adm, body, err := s.admitScope(ctx, ep, g, sc, out, betas, id)
		// Budgets just created arm within a moment (every replica reports
		// on the change notification): a first request waits for them.
		deadline := s.now().Add(profileArmWait)
		for ps.created && err != nil && s.now().Before(deadline) {
			var ae *admitError
			if !errors.As(err, &ae) || ae.denyTag != "budget_arming" || !ae.key.Kind.Profile() {
				break
			}
			select {
			case <-ctx.Done():
				return nil, nil, err
			case <-time.After(50 * time.Millisecond):
			}
			adm, body, err = s.admitScope(ctx, ep, g, sc, out, betas, id)
		}
		if adm != nil {
			adm.profile, adm.overflow = ps.id, sc.overflow
		}
		return adm, body, err
	}
	adm, body, err := admit(scope)
	var ae *admitError
	if err == nil || !ps.overflow || !errors.As(err, &ae) || ae.denyTag != "profile" ||
		ae.key.Kind != store.ScopeProfileWeek && ae.key.Kind != store.ScopeProfileSession {
		return adm, body, err
	}
	// Beyond the allowance, on the overflow cap (its budget exists: the
	// assignment that allowed it created it).
	over := scope
	over.overflow = true
	if !s.budget.has(store.BudgetKey{Kind: store.ScopeProfileOverflow, Name: scope.user, Unit: scope.unit}) {
		if eerr := s.ensureProfile(ctx, scope.user); eerr != nil {
			return nil, nil, err
		}
		ps.created = true
	}
	oadm, obody, oerr := admit(over)
	var oe *admitError
	if oerr != nil && !(errors.As(oerr, &oe) && oe.key.Kind == store.ScopeProfileOverflow) {
		// Refused for another budget than the overflow cap (the team's,
		// the org's…): that refusal is the one to report.
		return nil, nil, oerr
	}
	if oerr != nil {
		// Both refuse: say both, and the earlier of their resets.
		both := *oe
		both.msg = ae.msg + "; and the " + oe.msg
		if !ae.reset.IsZero() && (both.reset.IsZero() || ae.reset.Before(both.reset)) {
			both.reset = ae.reset
		}
		return nil, nil, &both
	}
	return oadm, obody, nil
}

// admitScope is admitBudget for one scope.
func (s *Server) admitScope(ctx context.Context, ep openai.Endpoint, g *group, scope reqScope, out []byte, betas []string, id string) (*admission, []byte, error) {
	adm := &admission{at: s.now(), id: id}
	if s.softBudgets {
		// The pre-#87 check (spent < limit), kept for the planted control
		// of the property test only.
		if err := s.budget.check(scope); err != nil {
			return nil, nil, &admitError{http.StatusTooManyRequests, "insufficient_quota", err.Error(), "budget", time.Time{}, store.BudgetKey{}}
		}
		return adm, out, nil
	}
	capTok := g.cfg.MaxOutputTokens
	sh, shapeErr := openai.ShapeOf(ep, out)
	// Tracked before the budgets are read (arming.go): at its largest
	// possible output bound, or without bound when there is none.
	s.tracker.track(id, scope.keys(), s.trackedW(g, sh, shapeErr, betas), adm.at)
	refs := s.budget.refs(scope)
	if len(refs) == 0 && capTok == 0 {
		return adm, out, nil
	}
	sh, err := sh, shapeErr
	if err != nil {
		return nil, nil, &admitError{http.StatusBadRequest, "invalid_request_error", err.Error(), "", time.Time{}, store.BudgetKey{}}
	}
	if sh.Generates && sh.HasMaxTokens && capTok > 0 && sh.MaxTokens > capTok {
		return nil, nil, &admitError{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("max_tokens %d exceeds this model's max_output_tokens (%d)", sh.MaxTokens, capTok), "", time.Time{}, store.BudgetKey{}}
	}
	writable := sh.Generates && ep != openai.Transcriptions
	if len(refs) == 0 || sh.Free || g.prices.zero {
		if writable && !sh.HasMaxTokens && capTok > 0 {
			adm.m, adm.set, adm.picked = capTok, true, true
		}
		return adm.write(ep, out)
	}
	if sh.Unbounded == "" && sh.Generates {
		sh.Unbounded = betaUnbounded(g, betas)
	}
	if sh.Unbounded != "" {
		return nil, nil, &admitError{http.StatusBadRequest, "invalid_request_error",
			"this request's cost cannot be bounded before it runs (" + sh.Unbounded + "), so it is refused while a budget applies to its scope", "budget_unbounded", time.Time{}, store.BudgetKey{}}
	}
	adm.reset = s.escrow.tightest(refs)
	l := g.contextLen()
	if l <= 0 {
		return nil, nil, &admitError{http.StatusServiceUnavailable, "service_unavailable", errNoContextLen.Error(), "context_unknown", time.Time{}, store.BudgetKey{}}
	}
	provider := g.ext != nil
	try := func(m int64) (*hold, int64, error) {
		w, ok := g.prices.worstCase(sh, l, m, provider)
		if !ok {
			return nil, w, errNoContextLen
		}
		h, err := s.escrow.acquire(ctx, refs, w)
		return h, w, err
	}
	refuse := func(err error, w int64) error {
		var nr *noRoom
		if errors.As(err, &nr) && nr.arming {
			return armingRefusal(nr.key)
		}
		if errors.As(err, &nr) {
			return s.budgetRefusal(nr.key, nr.room, w, scope.unit, nr.resetIn(refs))
		}
		return &admitError{http.StatusServiceUnavailable, "service_unavailable", "the budget ledger cannot be reached; requests under a budget are refused", "store_unavailable", time.Time{}, store.BudgetKey{}}
	}
	if !writable || sh.HasMaxTokens {
		m := sh.MaxTokens
		h, w, err := try(m)
		if err != nil {
			return nil, nil, refuse(err, w)
		}
		adm.hold, adm.m, adm.set = h, m, writable
		adm.reset = s.escrow.tightest(refs)
		return adm.write(ep, out)
	}
	// The client sent no bound: the largest that fits, from the cap (or the
	// whole context) down to the floor.
	hi := l
	if capTok > 0 {
		hi = min(capTok, l)
	}
	lo := min(s.cfg.Budgets.MinMaxTokens, hi)
	m := hi
	for attempt := 0; ; attempt++ {
		h, w, err := try(m)
		if err == nil {
			adm.hold, adm.m, adm.set, adm.picked = h, m, true, true
			adm.reset = s.escrow.tightest(refs)
			return adm.write(ep, out)
		}
		var nr *noRoom
		if !errors.As(err, &nr) || nr.arming || attempt >= 3 {
			return nil, nil, refuse(err, w)
		}
		next := g.prices.largestFit(sh, l, lo, m, nr.room, provider)
		if next < 0 {
			wlo, _ := g.prices.worstCase(sh, l, lo, provider)
			return nil, nil, s.budgetRefusal(nr.key, nr.room, wlo, scope.unit, nr.resetIn(refs))
		}
		if next == m {
			next = m - 1 // the room shrank meanwhile
			if next < lo {
				return nil, nil, refuse(err, w)
			}
		}
		m = next
	}
}

// trackedW is the worst case a request is tracked at for arming budgets:
// its W at the largest output bound it can be sent with; no bound
// (maxLedgerNano) when it has none; 0 when it is never charged.
func (s *Server) trackedW(g *group, sh openai.Shape, shapeErr error, betas []string) int64 {
	if g.prices.zero || shapeErr == nil && sh.Free {
		return 0
	}
	if shapeErr != nil || sh.Unbounded != "" || sh.Generates && betaUnbounded(g, betas) != "" {
		return maxLedgerNano
	}
	l := g.contextLen()
	m := sh.MaxTokens
	if sh.Generates && !sh.HasMaxTokens {
		m = l
		if g.cfg.MaxOutputTokens > 0 {
			m = min(g.cfg.MaxOutputTokens, l)
		}
	}
	w, ok := g.prices.worstCase(sh, l, m, g.ext != nil)
	if !ok {
		return maxLedgerNano
	}
	return min(w, maxLedgerNano)
}

// write sets the admission's output bound into the body.
func (a *admission) write(ep openai.Endpoint, out []byte) (*admission, []byte, error) {
	b, err := a.apply(ep, out)
	if err != nil {
		a.hold.settle(0)
		return nil, nil, &admitError{http.StatusBadRequest, "invalid_request_error", err.Error(), "", time.Time{}, store.BudgetKey{}}
	}
	return a, b, nil
}

// apply writes the bound into a body rewritten again (after masking, or
// for a fallback).
func (a *admission) apply(ep openai.Endpoint, out []byte) ([]byte, error) {
	if a == nil || !a.set {
		return out, nil
	}
	return openai.SetMaxTokens(ep, out, a.m)
}

// header tells the client the bound the gateway chose, and when the
// tightest budget it falls under resets.
func (a *admission) header(h http.Header) {
	if a != nil && a.picked {
		h.Set("X-Llmflow-Max-Tokens", strconv.FormatInt(a.m, 10))
	}
	if a != nil && !a.reset.IsZero() {
		h.Set(budgetResetHeader, a.reset.UTC().Format(time.RFC3339))
	}
}

// release ends an admission whose request cost actual nano-units.
func (a *admission) release(actual int64) bool {
	if a == nil {
		return false
	}
	return a.hold.settle(actual)
}

// recordHolds fills a record's hard-budget fields; the record must then be
// handed to the recorder, which untracks it once stored.
func (s *Server) recordHolds(rec *store.Record, a *admission, costNano int64) {
	rec.CostNano = costNano
	if a == nil {
		return
	}
	rec.Profile, rec.Overflow = a.profile, a.overflow
	a.recorded = true
	rec.ID = a.id
	rec.AdmittedAt = a.at.UTC()
	if h := a.hold; h != nil && len(h.refs) > 0 {
		rec.HoldReplica, rec.HoldIncarnation, rec.HoldNano = s.escrow.holder.Replica, s.escrow.holder.Incarnation, h.w
		for _, r := range h.refs {
			rec.Holds = append(rec.Holds, store.HoldRef{Key: r.key, Start: r.start.UTC()})
		}
	}
}

// writeAdmitError answers a refused admission.
func (s *Server) writeAdmitError(w http.ResponseWriter, team string, err error) {
	var ae *admitError
	if !errors.As(err, &ae) {
		ae = &admitError{http.StatusServiceUnavailable, "service_unavailable", err.Error(), "store_unavailable", time.Time{}, store.BudgetKey{}}
	}
	if ae.denyTag != "" {
		s.met.denied.add(1, team, ae.denyTag)
	}
	if !ae.reset.IsZero() {
		w.Header().Set(budgetResetHeader, ae.reset.UTC().Format(time.RFC3339))
	}
	apiError(w, ae.status, ae.typ, ae.msg)
}
