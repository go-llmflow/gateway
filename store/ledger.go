package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// The budget ledger (hard budgets, #87): per budget and period, the cost
// committed (spent) and, per holder (a replica's process), an escrow
// (granted). Every transaction keeps
//
//	spent + Σ granted ≤ limit
//
// A replica admits a request locally while its own grant covers the
// request's worst-case cost W, and grows the grant in one transaction when
// it does not; storing a request's record settles its hold (spent +=
// actual, granted -= min(actual, W)), which leaves the sum unchanged. As in
// Slurm's AccountingStorageEnforce=safe (slurm.schedmd.com/resource_limits.html),
// a request starts only when it can run to completion within the limit.
//
// Amounts are integers of nano-units (1e-9 of the budget's unit): no float
// drift in ≤. A limit above MaxLedgerNano is enforced as MaxLedgerNano.

// MaxLedgerNano bounds every amount of the ledger (2^62 nano-units).
const MaxLedgerNano = int64(1) << 62

// Holder is who holds a grant: a replica (ha.replica_id, or the host) and
// one process of it (its incarnation, the id of its accounting spool).
type Holder struct {
	Replica     string `json:"replica"`
	Incarnation string `json:"incarnation"`
}

// HoldRef is one budget period a request was held against.
type HoldRef struct {
	Key   BudgetKey `json:"key"`
	Start time.Time `json:"start"`
}

// GrantRequest asks to grow a holder's grant on one budget period: by the
// largest of Need, Want and Share of the room left, when it fits, else by
// as much as fits as long as that is at least Need.
type GrantRequest struct {
	Key   BudgetKey
	Start time.Time
	Need  int64
	Want  int64
	Share float64
}

// growth is how much a request grows a grant with room left.
func (r GrantRequest) growth(room int64) int64 {
	want := max(r.Need, r.Want)
	if r.Share > 0 && r.Share <= 1 {
		want = max(want, int64(float64(room)*r.Share))
	}
	return min(want, room)
}

// GrantResult is what a growth gave.
type GrantResult struct {
	Key   BudgetKey
	Start time.Time
	// NoBudget: the budget no longer exists; nothing was granted, nothing
	// needs to be.
	NoBudget bool
	Grown    int64
	// Room is what remains after the growth: limit - spent - Σ granted.
	Room  int64
	Epoch int64
	// Until, when not zero, is when the grant stops being lendable: it
	// relies on a temporary increase that ends at Until + the lending time.
	Until time.Time
}

// GrantReturn gives back part of a grant.
type GrantReturn struct {
	Key    BudgetKey
	Start  time.Time
	Amount int64
}

// GrantRow is one holder's grant, for the admin API.
type GrantRow struct {
	Key     BudgetKey `json:"key"`
	Start   time.Time `json:"period_start"`
	Holder  Holder    `json:"holder"`
	Granted int64     `json:"granted_nano"`
}

// LedgerState is a budget period's ledger.
type LedgerState struct {
	Spent, Granted int64
}

// NoRoomError refuses a growth that does not fit a budget: Room is what
// the budget period had left for it. Arming: the budget is new and some
// gateway process has not yet reported what it had in flight in its scope
// (v0.17.1, see ReportArming).
type NoRoomError struct {
	Key    BudgetKey
	Room   int64
	Arming bool
	// Reset, for a rolling window, is when enough of what it counts has
	// left it for the growth to fit (RollingReset); zero otherwise.
	Reset time.Time
}

// bucketSpend is one bucket of a rolling window.
type bucketSpend struct {
	start time.Time
	spent int64
}

// RollingReset is when a rolling window of d, whose buckets in the window
// are bs (oldest first), has given back short nano-units: the bucket that
// completes them leaves the window then. When the buckets cannot (the rest
// is held by grants), it is when the whole window has emptied.
func rollingReset(bs []bucketSpend, now time.Time, d time.Duration, short int64) time.Time {
	var freed int64
	for _, b := range bs {
		freed += b.spent
		if freed >= short {
			return b.start.Add(BucketWidth + d)
		}
	}
	return BucketOf(now).Add(BucketWidth + d)
}

func (e *NoRoomError) Error() string {
	if e.Arming {
		return fmt.Sprintf("budget %s is arming", e.Key)
	}
	return fmt.Sprintf("budget %s has %d nano-units left", e.Key, e.Room)
}

// Arming (v0.17.1). A budget created over a scope already in use starts
// arming: the requests in its scope that were admitted before any gateway
// process knew it hold nothing on it, yet their cost lands in it. Every
// registered gateway process (RegisterHolder) reports, once it has loaded
// the budget, the worst case of the requests it still has unsettled in the
// scope (ReportArming), and lowers it as they settle. No grant is made
// before every registered process has reported; meanwhile
//
//	spent + Σ grants + Σ pre-existing ≤ limit
//
// A process that died without a clean shutdown stays registered, so the
// budget stays arming, until its spool is replayed (ReleaseIncarnation) or
// an administrator releases its replica: never on a timer. When every
// report is back to 0 the budget is active and the reports are removed.

// BelowCommittedError refuses a limit lower than what the current period
// already committed (spent + Σ granted), unless forced.
type BelowCommittedError struct {
	Spent, Granted, Limit int64
}

func (e *BelowCommittedError) Error() string {
	return fmt.Sprintf("the new limit (%s) is below what this period has committed (spent %s + held %s); force it to stop new admissions only",
		FormatNano(e.Limit), FormatNano(e.Spent), FormatNano(e.Granted))
}

// ErrBelowCommitted matches a *BelowCommittedError with errors.Is.
var ErrBelowCommitted = errors.New("below committed")

func (e *BelowCommittedError) Is(target error) bool { return target == ErrBelowCommitted }

// BudgetLedger is the ledger's part of the store.
type BudgetLedger interface {
	// SetBudgetChecked is SetBudget, refused with *BelowCommittedError when
	// the new limit at now (lendFor ahead, see LimitNano) is below the
	// current period's spent + Σ granted, unless force. A lower limit, and
	// a new budget, take a new Epoch. SetBudget is SetBudgetChecked forced.
	SetBudgetChecked(ctx context.Context, b Budget, now time.Time, lendFor time.Duration, force bool) error
	// IncreaseBudgetChecked is IncreaseBudget, checked like
	// SetBudgetChecked; it always takes a new Epoch.
	IncreaseBudgetChecked(ctx context.Context, k BudgetKey, amount float64, expiry, now time.Time, lendFor time.Duration, force bool) error
	// GrowGrants grows h's grants on every requested budget period, all or
	// none (*NoRoomError names the first that does not fit). The ledger
	// rows are locked in a canonical order, so two growths never deadlock.
	// A period's spent starts as the cost of its usage records.
	GrowGrants(ctx context.Context, h Holder, now time.Time, lendFor time.Duration, reqs []GrantRequest) ([]GrantResult, error)
	// ReturnGrants gives back unused grant (never below 0).
	ReturnGrants(ctx context.Context, h Holder, rets []GrantReturn) error
	// ReleaseIncarnation drops every grant of a process that is gone and
	// whose accounting spool was replayed; it returns the amount released.
	ReleaseIncarnation(ctx context.Context, incarnation string) (int64, error)
	// Grants lists every grant held.
	Grants(ctx context.Context) ([]GrantRow, error)
	// ReleaseGrants drops the given grants (an administrator releasing a
	// replica that never came back); it returns the amount released.
	ReleaseGrants(ctx context.Context, rows []GrantRow) (int64, error)
	// Ledger returns a budget period's ledger (zero when it has none);
	// LedgerAt is Ledger at now, which a rolling window's spent depends on
	// (Ledger is LedgerAt at the current time).
	Ledger(ctx context.Context, k BudgetKey, start time.Time) (LedgerState, error)
	LedgerAt(ctx context.Context, k BudgetKey, start, now time.Time) (LedgerState, error)

	// RegisterHolder records a gateway process before it admits anything;
	// UnregisterHolder removes it (and its arming reports) at a clean
	// shutdown, once its records are stored.
	RegisterHolder(ctx context.Context, h Holder) error
	UnregisterHolder(ctx context.Context, h Holder) error
	// ReportArming records what h still has unsettled in an arming
	// budget's scope, and activates the budget once every registered
	// process has reported 0. It reports whether the budget is active.
	ReportArming(ctx context.Context, h Holder, k BudgetKey, pre int64) (bool, error)
	// ReleaseReplica drops the registration and the arming reports of every
	// process of a replica that will not come back (an administrator).
	ReleaseReplica(ctx context.Context, replica string) error
}

// ToNano converts an amount of units to nano-units, rounded down and
// clamped to [0, MaxLedgerNano].
func ToNano(v float64) int64 {
	if !(v > 0) {
		return 0
	}
	n := math.Floor(v * 1e9)
	if !(n < float64(MaxLedgerNano)) {
		return MaxLedgerNano
	}
	return int64(n)
}

// FormatNano spells an amount of nano-units as units.
func FormatNano(n int64) string {
	return fmt.Sprintf("%.9f", float64(n)/1e9)
}

// LimitNano is a budget's limit in nano-units for a grant made at now and
// lent until at most now+lendFor (the longest a request runs): a temporary
// increase counts only when it outlives that, and the grant is then
// lendable until its expiry - lendFor.
func LimitNano(b Budget, now time.Time, lendFor time.Duration) (int64, time.Time) {
	l, until := b.MaxCost, time.Time{}
	if b.TempIncrease > 0 && b.TempExpiry != nil && b.TempExpiry.After(now.Add(lendFor)) {
		l += b.TempIncrease
		until = b.TempExpiry.Add(-lendFor)
	}
	return ToNano(l), until
}

// RecordCostNano is a record's cost in nano-units: CostNano, or its Cost
// rounded up for a record written before hard budgets.
func RecordCostNano(r Record) int64 {
	if r.CostNano > 0 {
		return min(r.CostNano, MaxLedgerNano)
	}
	if r.Cost > 0 {
		return min(int64(math.Ceil(r.Cost*1e9)), MaxLedgerNano)
	}
	return 0
}

// Attrs are what a request is charged to: its budget scopes. User is the
// person, as the reports name them (a key's owner, or a JWT's user): all
// their keys and tokens share one user budget.
type Attrs struct {
	Org, Team, Project, KeyID, User, Model string
	Tags                                   []string
	Unit                                   string
	// Overflow: admitted beyond the user's workload profile (Record.Overflow).
	Overflow bool
}

// RecordAttrs are a record's scopes.
func RecordAttrs(r Record) Attrs {
	return Attrs{Org: r.Org, Team: r.Team, Project: r.Project, KeyID: r.KeyID, User: r.User, Model: r.Model, Tags: r.Tags, Unit: r.Unit,
		Overflow: r.Overflow}
}

// ScopeKeys lists the budget keys a request of these attributes falls
// under: each scope, for every model and for its own model, in its unit. A
// user's request falls under their profile's weekly and session budgets,
// or, admitted beyond them, under its overflow budget.
func ScopeKeys(a Attrs) []BudgetKey {
	names := make([]BudgetKey, 0, 7+len(a.Tags))
	names = append(names, BudgetKey{Kind: ScopeOrg, Name: a.Org, Unit: a.Unit}, BudgetKey{Kind: ScopeTeam, Name: a.Team, Unit: a.Unit},
		BudgetKey{Kind: ScopeKey, Name: a.KeyID, Unit: a.Unit})
	if a.Project != "" {
		names = append(names, BudgetKey{Kind: ScopeProject, Name: a.Team + "/" + a.Project, Unit: a.Unit})
	}
	if a.User != "" {
		names = append(names, BudgetKey{Kind: ScopeUser, Name: a.User, Unit: a.Unit})
		if a.Overflow {
			names = append(names, BudgetKey{Kind: ScopeProfileOverflow, Name: a.User, Unit: a.Unit})
		} else {
			names = append(names, BudgetKey{Kind: ScopeProfileWeek, Name: a.User, Unit: a.Unit},
				BudgetKey{Kind: ScopeProfileSession, Name: a.User, Unit: a.Unit})
		}
	}
	for _, t := range a.Tags {
		names = append(names, BudgetKey{Kind: ScopeTag, Name: t, Unit: a.Unit})
	}
	out := make([]BudgetKey, 0, 2*len(names))
	for _, k := range names {
		out = append(out, k)
		k.Model = a.Model
		out = append(out, k)
	}
	return out
}

// Matches reports whether a usage row of these attributes counts for
// budget key k.
func Matches(k BudgetKey, a Attrs) bool {
	if k.Unit != a.Unit || k.Model != "" && k.Model != a.Model {
		return false
	}
	switch k.Kind {
	case ScopeOrg:
		return a.Org == k.Name
	case ScopeTeam:
		return a.Team == k.Name
	case ScopeProject:
		return a.Project != "" && a.Team+"/"+a.Project == k.Name
	case ScopeKey:
		return a.KeyID == k.Name
	case ScopeUser:
		return a.User != "" && a.User == k.Name
	case ScopeProfileWeek, ScopeProfileSession:
		return a.User != "" && a.User == k.Name && !a.Overflow
	case ScopeProfileOverflow:
		return a.User != "" && a.User == k.Name && a.Overflow
	case ScopeTag:
		for _, t := range a.Tags {
			if t == k.Name {
				return true
			}
		}
	}
	return false
}

// ledgerLess is the canonical order of budget periods: every transaction
// locks ledger rows in it.
func ledgerLess(a BudgetKey, as time.Time, b BudgetKey, bs time.Time) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	if a.Unit != b.Unit {
		return a.Unit < b.Unit
	}
	return as.Before(bs)
}

// checkChange decides a change of limit: whether it lowers the limit (a
// new budget lowers it from none), and whether it is refused: a lower limit
// below what the period committed, unless forced. A change that raises the
// limit is never refused, even when the period is over a forced lower one.
func checkChange(old *Budget, nb Budget, st LedgerState, now time.Time, lendFor time.Duration, force bool) (bool, error) {
	newL, _ := LimitNano(nb, now, lendFor)
	lower := old == nil
	if old != nil {
		oldL, _ := LimitNano(*old, now, lendFor)
		lower = newL < oldL
	}
	if lower && !force && st.Spent+st.Granted > newL {
		return lower, &BelowCommittedError{Spent: st.Spent, Granted: st.Granted, Limit: newL}
	}
	return lower, nil
}

// nextEpoch is an epoch never used before by a budget of that key, even one
// deleted and created again.
func nextEpoch(old int64, now time.Time) int64 { return max(old+1, now.UnixNano()) }

// ---- memory ----

type ledgerKey struct {
	k     BudgetKey
	start int64 // unix nanoseconds
}

type memLedger struct {
	spent  int64
	grants map[Holder]int64
	// buckets: a rolling window's spent by bucket (unix seconds).
	buckets map[int64]int64
}

// rolling reports whether a ledger row is a rolling window's.
func rollingRow(start time.Time) bool { return start.Equal(RollingStart) }

// bucketAt is when a record counts in a rolling window: when it ended (its
// admission for a record without an end). Not its admission: a request
// admitted at t may be held on a grant grown at a later t' (the time is
// read before the hold), whose room left out buckets that a window ending
// between t and t' still counts; the property test of #90 found a window
// over its limit that way. The end comes after the hold, hence after every
// growth that funded it, and the grant covers the request until then.
func bucketAt(r Record) time.Time {
	if r.Time.IsZero() {
		return r.AdmittedAt
	}
	return r.Time
}

// recordAt is when a record counts: its admission (its end for a record
// written before hard budgets).
func recordAt(r Record) time.Time {
	if r.AdmittedAt.IsZero() {
		return r.Time
	}
	return r.AdmittedAt
}

// windowSpent is a rolling row's spent at now for a window of d; it drops
// the buckets that left the window long ago.
func (l *memLedger) windowSpent(now time.Time, d time.Duration) int64 {
	from := WindowStart(now, d).Unix()
	var s int64
	for b, v := range l.buckets {
		switch {
		case b >= from:
			s = min(s+v, MaxLedgerNano)
		case b < from-int64(d/time.Second):
			delete(l.buckets, b)
		}
	}
	return s
}

// windowBuckets lists a rolling row's buckets in the window, oldest first.
func (l *memLedger) windowBuckets(now time.Time, d time.Duration) []bucketSpend {
	from := WindowStart(now, d).Unix()
	var out []bucketSpend
	for b, v := range l.buckets {
		if b >= from {
			out = append(out, bucketSpend{time.Unix(b, 0).UTC(), v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// spentAt is a row's spent at now under period p.
func (l *memLedger) spentAt(p Period, now time.Time) int64 {
	if d, ok := p.Rolling(); ok {
		return l.windowSpent(now, d)
	}
	return l.spent
}

// seed sets a row's spent from the stored records of its period (m.mu
// held): a new row, or one whose budget was created again (records stored
// while no budget existed settled nothing in it).
func (m *Memory) seed(l *memLedger, k BudgetKey, start time.Time) {
	l.spent, l.buckets = 0, nil
	if rollingRow(start) {
		l.buckets = map[int64]int64{}
	}
	for _, r := range m.records {
		at := recordAt(r)
		if !Matches(k, RecordAttrs(r)) {
			continue
		}
		if l.buckets != nil {
			b := BucketOf(bucketAt(r)).Unix()
			l.buckets[b] = min(l.buckets[b]+RecordCostNano(r), MaxLedgerNano)
		} else if !at.Before(start) {
			l.spent = min(l.spent+RecordCostNano(r), MaxLedgerNano)
		}
	}
}

// addSpent settles a record's actual nano-units in a row.
func (l *memLedger) addSpent(start time.Time, r Record, actual int64) {
	if rollingRow(start) {
		if l.buckets == nil {
			l.buckets = map[int64]int64{}
		}
		b := BucketOf(bucketAt(r)).Unix()
		l.buckets[b] = min(l.buckets[b]+actual, MaxLedgerNano)
		return
	}
	l.spent = min(l.spent+actual, MaxLedgerNano)
}

func (l *memLedger) granted() int64 {
	var s int64
	for _, g := range l.grants {
		s += g
	}
	return s
}

func (m *Memory) ledgerRow(k BudgetKey, start time.Time, create bool) *memLedger {
	lk := ledgerKey{k, start.UnixNano()}
	if l := m.ledger[lk]; l != nil || !create {
		return l
	}
	if m.ledger == nil {
		m.ledger = map[ledgerKey]*memLedger{}
	}
	l := &memLedger{grants: map[Holder]int64{}}
	// A new period starts from the cost of its usage records.
	m.seed(l, k, start)
	m.ledger[lk] = l
	return l
}

func (m *Memory) SetBudgetChecked(_ context.Context, b Budget, now time.Time, lendFor time.Duration, force bool) error {
	if err := ValidateBudget(b); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.budgets == nil {
		m.budgets = map[BudgetKey]*memBudget{}
	}
	cur := m.budgets[b.BudgetKey]
	var old *Budget
	if cur != nil {
		b.TempIncrease, b.TempExpiry, b.Epoch = cur.b.TempIncrease, cur.b.TempExpiry, cur.b.Epoch
		old = &cur.b
		b.Arming = cur.b.Arming
	} else {
		b.TempIncrease, b.TempExpiry, b.Epoch = 0, nil, 0
		b.Arming = !m.NoArming
		delete(m.arming, b.BudgetKey)
	}
	var st LedgerState
	if l := m.ledgerRow(b.BudgetKey, b.Period.Start(now), false); l != nil {
		if cur == nil {
			// Created again: what was stored meanwhile settled nothing here.
			m.seed(l, b.BudgetKey, b.Period.Start(now))
		}
		st = LedgerState{l.spentAt(b.Period, now), l.granted()}
	}
	if _, err := checkChange(old, b, st, now, lendFor, force); err != nil {
		return err
	}
	// Every change takes a new epoch: the replicas drop what they knew of
	// the budget (unused grants, a refusal they cached).
	b.Epoch = nextEpoch(b.Epoch, now)
	if cur != nil {
		cur.b = b
		return nil
	}
	m.budgets[b.BudgetKey] = &memBudget{b: b}
	return nil
}

func (m *Memory) IncreaseBudgetChecked(_ context.Context, k BudgetKey, amount float64, expiry, now time.Time, lendFor time.Duration, force bool) error {
	if !validAmount(amount) {
		return fmt.Errorf("amount %v: want 0..1e15", amount)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	cur, ok := m.budgets[k]
	if !ok {
		return ErrNotFound
	}
	nb := cur.b
	e := expiry.UTC()
	nb.TempIncrease, nb.TempExpiry = amount, &e
	var st LedgerState
	if l := m.ledgerRow(k, nb.Period.Start(now), false); l != nil {
		st = LedgerState{l.spentAt(nb.Period, now), l.granted()}
	}
	if _, err := checkChange(&cur.b, nb, st, now, lendFor, force); err != nil {
		return err
	}
	// Always a new epoch: grants lent under the previous increase may
	// outlive the new one.
	nb.Epoch = nextEpoch(nb.Epoch, now)
	cur.b = nb
	return nil
}

func (m *Memory) GrowGrants(_ context.Context, h Holder, now time.Time, lendFor time.Duration, reqs []GrantRequest) ([]GrantResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	// All or none: check every request first.
	out := make([]GrantResult, 0, len(reqs))
	for _, r := range reqs {
		res := GrantResult{Key: r.Key, Start: r.Start}
		mb := m.budgets[r.Key]
		if mb == nil {
			res.NoBudget = true
			out = append(out, res)
			continue
		}
		if _, rolling := mb.b.Period.Rolling(); rolling != rollingRow(r.Start) {
			return nil, &NoRoomError{Key: r.Key} // see Postgres.GrowGrants
		}
		limit, until := LimitNano(mb.b, now, lendFor)
		l := m.ledgerRow(r.Key, r.Start, true)
		pre, ready := m.armingPre(mb.b)
		if !ready {
			return nil, &NoRoomError{Key: r.Key, Arming: true}
		}
		room := limit - l.spentAt(mb.b.Period, now) - l.granted() - pre
		if room < r.Need || r.Need < 0 {
			nr := &NoRoomError{Key: r.Key, Room: max(room, 0)}
			if d, ok := mb.b.Period.Rolling(); ok {
				nr.Reset = rollingReset(l.windowBuckets(now, d), now, d, r.Need-room)
			}
			return nil, nr
		}
		res.Grown = r.growth(room)
		res.Room, res.Epoch, res.Until = room-res.Grown, mb.b.Epoch, until
		out = append(out, res)
	}
	for _, res := range out {
		if !res.NoBudget {
			m.ledgerRow(res.Key, res.Start, true).grants[h] += res.Grown
		}
	}
	return out, nil
}

func (m *Memory) ReturnGrants(_ context.Context, h Holder, rets []GrantReturn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for _, r := range rets {
		if l := m.ledgerRow(r.Key, r.Start, false); l != nil {
			l.grants[h] = max(0, l.grants[h]-max(r.Amount, 0))
			if l.grants[h] == 0 {
				delete(l.grants, h)
			}
		}
	}
	return nil
}

func (m *Memory) ReleaseIncarnation(_ context.Context, inc string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	var n int64
	for _, l := range m.ledger {
		for h, g := range l.grants {
			if h.Incarnation == inc {
				n += g
				delete(l.grants, h)
			}
		}
	}
	m.dropIncarnation(inc)
	return n, nil
}

func (m *Memory) Grants(context.Context) ([]GrantRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []GrantRow
	for lk, l := range m.ledger {
		for h, g := range l.grants {
			out = append(out, GrantRow{Key: lk.k, Start: time.Unix(0, lk.start).UTC(), Holder: h, Granted: g})
		}
	}
	sortGrants(out)
	return out, nil
}

func sortGrants(out []GrantRow) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Key != b.Key || !a.Start.Equal(b.Start) {
			return ledgerLess(a.Key, a.Start, b.Key, b.Start)
		}
		if a.Holder.Replica != b.Holder.Replica {
			return a.Holder.Replica < b.Holder.Replica
		}
		return a.Holder.Incarnation < b.Holder.Incarnation
	})
}

func (m *Memory) ReleaseGrants(_ context.Context, rows []GrantRow) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	var n int64
	for _, r := range rows {
		if l := m.ledgerRow(r.Key, r.Start, false); l != nil {
			n += l.grants[r.Holder]
			delete(l.grants, r.Holder)
		}
	}
	return n, nil
}

func (m *Memory) Ledger(ctx context.Context, k BudgetKey, start time.Time) (LedgerState, error) {
	return m.LedgerAt(ctx, k, start, time.Now())
}

func (m *Memory) LedgerAt(_ context.Context, k BudgetKey, start, now time.Time) (LedgerState, error) {
	m.mu.Lock() // a rolling row drops its old buckets
	defer m.mu.Unlock()
	if m.Fail != nil {
		return LedgerState{}, m.Fail
	}
	if l := m.ledger[ledgerKey{k, start.UnixNano()}]; l != nil {
		st := LedgerState{l.spent, l.granted()}
		if rollingRow(start) {
			st.Spent = 0
			if mb := m.budgets[k]; mb != nil {
				st.Spent = l.spentAt(mb.b.Period, now)
			}
		}
		return st, nil
	}
	return LedgerState{}, nil
}

// armingPre is the sum of an arming budget's reports, and whether every
// registered process has reported (m.mu held).
func (m *Memory) armingPre(b Budget) (int64, bool) {
	if !b.Arming {
		return 0, true
	}
	reps := m.arming[b.BudgetKey]
	var pre int64
	for inc := range m.holders {
		p, ok := reps[inc]
		if !ok {
			return 0, false
		}
		pre = min(pre+p, MaxLedgerNano)
	}
	return pre, true
}

func (m *Memory) RegisterHolder(_ context.Context, h Holder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.holders == nil {
		m.holders = map[string]Holder{}
	}
	m.holders[h.Incarnation] = h
	return nil
}

func (m *Memory) UnregisterHolder(_ context.Context, h Holder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.dropIncarnation(h.Incarnation)
	return nil
}

// dropIncarnation forgets a process's registration and reports (m.mu held).
func (m *Memory) dropIncarnation(inc string) {
	delete(m.holders, inc)
	for _, reps := range m.arming {
		delete(reps, inc)
	}
}

func (m *Memory) ReportArming(_ context.Context, h Holder, k BudgetKey, pre int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return false, m.Fail
	}
	mb := m.budgets[k]
	if mb == nil || !mb.b.Arming {
		return true, nil
	}
	if m.arming == nil {
		m.arming = map[BudgetKey]map[string]int64{}
	}
	if m.arming[k] == nil {
		m.arming[k] = map[string]int64{}
	}
	m.arming[k][h.Incarnation] = max(pre, 0)
	if total, ready := m.armingPre(mb.b); ready && total == 0 {
		mb.b.Arming = false
		delete(m.arming, k)
		return true, nil
	}
	return false, nil
}

func (m *Memory) ReleaseReplica(_ context.Context, replica string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for inc, h := range m.holders {
		if h.Replica == replica {
			m.dropIncarnation(inc)
		}
	}
	return nil
}

// unheldSettlements lists the budget periods a stored record counts for
// without a hold: the budgets of its scope (by its admission time) that it
// was not held against. Its cost lands in their spent (v0.17.1): requests
// admitted before a budget existed, or before a process loaded it.
func unheldSettlements(r Record, budgets func(BudgetKey) (Period, bool)) []HoldRef {
	at := recordAt(r)
	var out []HoldRef
	seen := map[BudgetKey]bool{}
	for _, k := range ScopeKeys(RecordAttrs(r)) {
		// A record without a model lists its keys twice (every model, its
		// own): once is its cost.
		if seen[k] {
			continue
		}
		seen[k] = true
		p, ok := budgets(k)
		if !ok {
			continue
		}
		start := p.Start(at)
		held := false
		for _, h := range r.Holds {
			if h.Key == k {
				held = true
				break
			}
		}
		if !held {
			out = append(out, HoldRef{Key: k, Start: start})
		}
	}
	return out
}

// settle applies the holds of a record just stored (m.mu held).
func (m *Memory) settle(r Record) {
	actual := RecordCostNano(r)
	if actual > 0 {
		for _, u := range unheldSettlements(r, func(k BudgetKey) (Period, bool) {
			mb := m.budgets[k]
			if mb == nil {
				return "", false
			}
			return mb.b.Period, true
		}) {
			if l := m.ledgerRow(u.Key, u.Start, false); l != nil {
				l.addSpent(u.Start, r, actual)
			}
		}
	}
	if len(r.Holds) == 0 {
		return
	}
	h := Holder{r.HoldReplica, r.HoldIncarnation}
	for _, hr := range r.Holds {
		l := m.ledgerRow(hr.Key, hr.Start, false)
		if l == nil {
			continue
		}
		l.addSpent(hr.Start, r, actual)
		l.grants[h] = max(0, l.grants[h]-min(actual, max(r.HoldNano, 0)))
		if l.grants[h] == 0 {
			delete(l.grants, h)
		}
	}
}
