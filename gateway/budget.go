package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Budget errors.
var (
	errBudgetExceeded = errors.New("budget exhausted for this period")
	errStateStale     = errors.New("accounting store unreachable; requests are refused (fail-closed)")
)

// budgetError names the scope whose budget is exhausted.
type budgetError struct{ key store.BudgetKey }

func (e *budgetError) Error() string {
	s := fmt.Sprintf("budget exhausted for this period (%s %s", e.key.Kind, e.key.Name)
	if e.key.Model != "" {
		s += ", model " + e.key.Model
	}
	return s + ")"
}

func (e *budgetError) Unwrap() error { return errBudgetExceeded }

// reqScope is everything a request is charged to. unit is the cost unit
// of its model ("" for GPU-hours): only the budgets of that unit apply.
type reqScope struct {
	org, team, project, key, model string
	tags                           []string
	unit                           string
}

// keys lists the budget keys that may apply to a request: each scope it
// belongs to, for every model and for its own model, in its unit.
func (r reqScope) keys() []store.BudgetKey {
	names := make([]store.BudgetKey, 0, 4+len(r.tags))
	names = append(names, store.BudgetKey{Kind: store.ScopeOrg, Name: r.org, Unit: r.unit}, store.BudgetKey{Kind: store.ScopeTeam, Name: r.team, Unit: r.unit},
		store.BudgetKey{Kind: store.ScopeKey, Name: r.key, Unit: r.unit})
	if r.project != "" {
		names = append(names, store.BudgetKey{Kind: store.ScopeProject, Name: r.team + "/" + r.project, Unit: r.unit})
	}
	for _, t := range r.tags {
		names = append(names, store.BudgetKey{Kind: store.ScopeTag, Name: t, Unit: r.unit})
	}
	out := make([]store.BudgetKey, 0, 2*len(names))
	for _, k := range names {
		out = append(out, k)
		k.Model = r.model
		out = append(out, k)
	}
	return out
}

// rowMatches reports whether spend row belongs to budget key k.
func rowMatches(k store.BudgetKey, r store.SpendRow) bool {
	if k.Unit != r.Unit || k.Model != "" && k.Model != r.Model {
		return false
	}
	switch k.Kind {
	case store.ScopeOrg:
		return r.Org == k.Name
	case store.ScopeTeam:
		return r.Team == k.Name
	case store.ScopeProject:
		return r.Project != "" && r.Team+"/"+r.Project == k.Name
	case store.ScopeKey:
		return r.KeyID == k.Name
	case store.ScopeTag:
		for _, t := range r.Tags {
			if t == k.Name {
				return true
			}
		}
	}
	return false
}

type budgetState struct {
	b     store.Budget
	start time.Time
	db    float64
}

// softAlert is a budget whose spend reached its soft limit.
type softAlert struct {
	b     store.Budget
	spent float64
	start time.Time
}

// budgeter keeps the spend of every budget for its current period in
// memory, refreshed from the store periodically, so that the request path
// never waits on PostgreSQL. Between refreshes it adds the cost of the
// requests this process finished. It also holds the team model lists.
type budgeter struct {
	mu         sync.Mutex
	st         store.Store
	now        func() time.Time
	budgets    map[store.BudgetKey]*budgetState
	pending    map[store.BudgetKey]float64 // charged here, record not yet stored
	written    map[store.BudgetKey]float64 // stored since the last refresh query
	models     map[string]map[string]bool  // team → allowed model groups
	allow      map[store.AllowKey][]netip.Prefix
	lastOK     time.Time
	everOK     bool
	maxAge     time.Duration
	failClosed bool
	// onSoft is called (outside the lock) for every budget at or above its
	// soft limit after a refresh.
	onSoft func([]softAlert)
}

func newBudgeter(st store.Store, refresh time.Duration, failClosed bool, now func() time.Time) *budgeter {
	return &budgeter{
		st: st, now: now, budgets: map[store.BudgetKey]*budgetState{},
		pending: map[store.BudgetKey]float64{}, written: map[store.BudgetKey]float64{},
		maxAge: 3 * refresh, failClosed: failClosed,
	}
}

// refresh reloads budgets, spend and team model lists.
//
// A finished request is counted in pending until the accounting writer has
// stored its record, then in written until the next refresh, whose query
// includes it. The written counts are taken out before the query runs, so
// a record is never missing from the spend; one written between the
// snapshot and the query is counted twice until the following refresh,
// which errs towards refusing. (The first version reset all local counts
// at each refresh, so records still in the accounting queue were counted
// nowhere: a key could overspend by everything it did since the last
// write.)
func (b *budgeter) refresh(ctx context.Context) error {
	b.mu.Lock()
	snapshot := b.written
	b.written = map[store.BudgetKey]float64{}
	b.mu.Unlock()
	restore := func() {
		b.mu.Lock()
		for k, v := range snapshot {
			b.written[k] += v
		}
		b.mu.Unlock()
	}

	list, err := b.st.Budgets(ctx)
	if err != nil {
		restore()
		return err
	}
	now := b.now()
	rowsAt := map[time.Time][]store.SpendRow{}
	for _, bg := range list {
		start := bg.Period.Start(now)
		if _, ok := rowsAt[start]; ok {
			continue
		}
		rows, err := b.st.SpendRows(ctx, start)
		if err != nil {
			restore()
			return err
		}
		rowsAt[start] = rows
	}
	budgets := make(map[store.BudgetKey]*budgetState, len(list))
	var alerts []softAlert
	for _, bg := range list {
		st := &budgetState{b: bg, start: bg.Period.Start(now)}
		for _, r := range rowsAt[st.start] {
			if rowMatches(bg.BudgetKey, r) {
				st.db += r.Cost
			}
		}
		budgets[bg.BudgetKey] = st
		// Alerts use the stored spend only: it is what every gateway
		// sees, and it cannot double-count records written meanwhile.
		if bg.SoftLimit > 0 && st.db >= bg.SoftLimit {
			alerts = append(alerts, softAlert{b: bg, spent: st.db, start: st.start})
		}
	}
	tm, err := b.st.TeamModels(ctx)
	if err != nil {
		restore()
		return err
	}
	models := make(map[string]map[string]bool, len(tm))
	for team, ms := range tm {
		set := make(map[string]bool, len(ms))
		for _, m := range ms {
			set[m] = true
		}
		models[team] = set
	}
	lists, err := b.st.Allowlists(ctx)
	if err != nil {
		restore()
		return err
	}
	allow := make(map[store.AllowKey][]netip.Prefix, len(lists))
	for _, l := range lists {
		ps := make([]netip.Prefix, 0, len(l.CIDRs))
		for _, c := range l.CIDRs {
			if p, err := netip.ParsePrefix(c); err == nil {
				ps = append(ps, p)
			}
		}
		// A list whose every prefix is unreadable must still deny:
		// an empty non-nil slice matches nothing.
		allow[l.AllowKey] = ps
	}
	b.mu.Lock()
	b.budgets, b.models, b.allow, b.lastOK, b.everOK = budgets, models, allow, now, true
	onSoft := b.onSoft
	b.mu.Unlock()
	if onSoft != nil && len(alerts) > 0 {
		onSoft(alerts)
	}
	return nil
}

// refreshTimeout bounds one refresh of the budget state.
const refreshTimeout = 30 * time.Second

func (b *budgeter) loop(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Bounded: a refresh stuck on a partitioned store would stop
			// every later one (A6). Fail-closed is decided by the age of
			// the last success, not by this loop.
			rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
			err := b.refresh(rctx)
			cancel()
			if err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// check reports whether a request may start: every budget it falls under
// must have room left.
func (b *budgeter) check(r reqScope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.failClosed && (!b.everOK || now.Sub(b.lastOK) > b.maxAge) {
		return errStateStale
	}
	for _, k := range r.keys() {
		st := b.budgets[k]
		if st == nil {
			continue
		}
		if st.db+b.pending[k]+b.written[k] >= st.b.Limit(now) {
			return &budgetError{key: k}
		}
	}
	return nil
}

// add charges a finished request: pending until its record is stored.
// Every scope key is tracked, whether or not it has a budget yet, so that
// a budget created meanwhile still sees the request.
func (b *budgeter) add(r reqScope, cost float64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range r.keys() {
		b.pending[k] += cost
	}
}

// persisted moves stored records from pending to written. It is also
// called for records the writer gave up on: they are then counted until
// the next refresh only (see README, accounting can drop records).
func (b *budgeter) persisted(recs []store.Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range recs {
		if rec.Cost <= 0 {
			continue
		}
		sc := reqScope{org: rec.Org, team: rec.Team, project: rec.Project, key: rec.KeyID, model: rec.Model, tags: rec.Tags, unit: rec.Unit}
		for _, k := range sc.keys() {
			b.pending[k] -= rec.Cost
			if v := b.pending[k]; v < 1e-12 && v > -1e-12 {
				delete(b.pending, k)
			}
			b.written[k] += rec.Cost
		}
	}
}

// spent returns the current spend of a budget (for the admin API).
func (b *budgeter) spent(k store.BudgetKey) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.budgets[k]
	if !ok {
		return 0, false
	}
	return st.db + b.pending[k] + b.written[k], true
}

// modelAllowed reports whether a team may use a model group: every model
// when the team has no list, only the listed ones otherwise.
func (b *budgeter) modelAllowed(team, model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.models[team]
	return !ok || set[model]
}

// addrAllowed reports whether address a passes every IP allowlist set on
// the key's organisation, team and the key itself.
func (b *budgeter) addrAllowed(a netip.Addr, k store.Key) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ak := range []store.AllowKey{{Kind: store.ScopeOrg, Name: k.Org}, {Kind: store.ScopeTeam, Name: k.Team}, {Kind: store.ScopeKey, Name: k.ID}} {
		if ps, ok := b.allow[ak]; ok && !inAny(a, ps) {
			return false
		}
	}
	return true
}

// healthy reports whether the last refresh is recent.
func (b *budgeter) healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.everOK && b.now().Sub(b.lastOK) <= b.maxAge
}
