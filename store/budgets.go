package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScopeKind is what a budget caps.
type ScopeKind string

// Budget scopes.
const (
	ScopeOrg     ScopeKind = "org"
	ScopeTeam    ScopeKind = "team"
	ScopeProject ScopeKind = "project" // Name is "team/project"
	ScopeKey     ScopeKind = "key"     // Name is the key ID
	ScopeTag     ScopeKind = "tag"
	// ScopeUser is a person (Name is the user as the reports name them: a
	// key's owner, or a JWT's user): every key and token of theirs counts.
	ScopeUser ScopeKind = "user"
	// The budgets a workload profile becomes (#90), managed by the gateway
	// (never through the budgets API), Name the user as for ScopeUser: the
	// weekly allowance and the rolling session allowance count the
	// requests admitted within the profile, the overflow cap (monthly) the
	// requests admitted beyond it (Record.Overflow).
	ScopeProfileWeek     ScopeKind = "profile_week"
	ScopeProfileSession  ScopeKind = "profile_session"
	ScopeProfileOverflow ScopeKind = "profile_overflow"
)

// Valid reports whether k is a known scope kind.
func (k ScopeKind) Valid() bool {
	switch k {
	case ScopeOrg, ScopeTeam, ScopeProject, ScopeKey, ScopeTag, ScopeUser:
		return true
	}
	return k.Profile()
}

// Profile reports whether k is one of a workload profile's budgets.
func (k ScopeKind) Profile() bool {
	return k == ScopeProfileWeek || k == ScopeProfileSession || k == ScopeProfileOverflow
}

// BudgetKey identifies a budget: a scope, optionally restricted to one
// model group (Model "" covers every model), in one cost unit (Unit ""
// is GPU-hours; "USD" or another currency code caps what an external
// provider charges). A budget counts only the costs of its own unit:
// GPU-hours and dollars are never added together.
type BudgetKey struct {
	Kind  ScopeKind `json:"kind"`
	Name  string    `json:"name"`
	Model string    `json:"model,omitempty"`
	Unit  string    `json:"unit,omitempty"`
}

func (k BudgetKey) String() string {
	s := string(k.Kind) + ":" + k.Name + "@" + k.Model
	if k.Unit != "" {
		s += "#" + k.Unit
	}
	return s
}

// GPUHours is the unit of the costs of vLLM models, as the API and the
// reports spell it; it is stored as "".
const GPUHours = "gpu_hours"

// NormalizeUnit maps the spelled-out GPU-hours unit to "" and checks a
// currency unit: 1 to 16 letters, digits or underscores (USD, EUR,
// usd_cents…). Units are compared exactly.
func NormalizeUnit(u string) (string, error) {
	if u == "" || u == GPUHours {
		return "", nil
	}
	if len(u) > 16 {
		return "", fmt.Errorf("unit %q: at most 16 characters", u)
	}
	for _, r := range u {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "", fmt.Errorf("unit %q: want letters, digits or _", u)
		}
	}
	return u, nil
}

// UnitName is the unit as the API and the reports show it.
func UnitName(u string) string {
	if u == "" {
		return GPUHours
	}
	return u
}

// Budget caps the cost of a scope per period, in its unit. A temporary
// increase adds to MaxCost until it expires; SoftLimit (0 = none) triggers
// one alert per period when the spend reaches it.
type Budget struct {
	BudgetKey
	Period       Period     `json:"period"`
	MaxCost      float64    `json:"max_cost"`
	SoftLimit    float64    `json:"soft_limit,omitempty"`
	TempIncrease float64    `json:"temp_increase,omitempty"`
	TempExpiry   *time.Time `json:"temp_expiry,omitempty"`
	// Epoch changes whenever the limit is lowered or a temporary increase
	// is set (hard budgets): a replica stops lending the grants it obtained
	// under an earlier epoch. Set by the store.
	Epoch int64 `json:"epoch,omitempty"`
	// Arming: created over a scope in use, the budget waits for every
	// gateway process to report what it had in flight (ledger.go). Set by
	// the store.
	Arming bool `json:"arming,omitempty"`
}

// Limit returns the effective maximum at now.
func (b Budget) Limit(now time.Time) float64 {
	if b.TempIncrease > 0 && b.TempExpiry != nil && now.Before(*b.TempExpiry) {
		return b.MaxCost + b.TempIncrease
	}
	return b.MaxCost
}

func validAmount(v float64) bool { return v >= 0 && v <= 1e15 && !math.IsNaN(v) }

// ValidateBudget checks a budget before it is stored.
func ValidateBudget(b Budget) error {
	if !b.Kind.Valid() {
		return fmt.Errorf("kind %q: want org, team, project, key, user or tag", b.Kind)
	}
	if _, rolling := b.Period.Rolling(); rolling && b.SoftLimit != 0 {
		return fmt.Errorf("soft_limit: not with a rolling period (its alert is once per calendar period)")
	}
	if b.Name == "" || len(b.Name) > 140 || strings.ContainsAny(b.Name, "\x00\r\n") {
		return fmt.Errorf("name %q is invalid", b.Name)
	}
	if len(b.Model) > 256 || strings.ContainsAny(b.Model, "\x00\r\n") {
		return fmt.Errorf("model %q is invalid", b.Model)
	}
	if u, err := NormalizeUnit(b.Unit); err != nil {
		return err
	} else if u != b.Unit {
		return fmt.Errorf("unit %q: store GPU-hours as \"\" (NormalizeUnit)", b.Unit)
	}
	if !b.Period.Valid() {
		return fmt.Errorf("period %q: want day, week, month or rolling:<window> (5m..168h in whole 5m, e.g. rolling:5h)", b.Period)
	}
	if !validAmount(b.MaxCost) {
		return fmt.Errorf("max_cost %v: want 0..1e15", b.MaxCost)
	}
	if !validAmount(b.SoftLimit) {
		return fmt.Errorf("soft_limit %v: want 0..1e15", b.SoftLimit)
	}
	return nil
}

// SpendRow is the cost of one combination of attributes since a time, in
// one unit.
type SpendRow struct {
	Org, Team, Project, KeyID, User, Model string
	Tags                                   []string
	Cost                                   float64
	Unit                                   string
	Overflow                               bool
}

// ---- memory ----

type memBudget struct {
	b       Budget
	alerted time.Time
}

func (m *Memory) SetBudget(ctx context.Context, b Budget) error {
	return m.SetBudgetChecked(ctx, b, time.Now(), 0, true)
}

func (m *Memory) IncreaseBudget(ctx context.Context, k BudgetKey, amount float64, expiry time.Time) error {
	return m.IncreaseBudgetChecked(ctx, k, amount, expiry, time.Now(), 0, true)
}

func (m *Memory) DeleteBudget(_ context.Context, k BudgetKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.budgets[k]; !ok {
		return ErrNotFound
	}
	delete(m.budgets, k)
	return nil
}

func sortBudgets(out []Budget) {
	sort.Slice(out, func(i, j int) bool { return out[i].BudgetKey.String() < out[j].BudgetKey.String() })
}

func (m *Memory) Budgets(context.Context) ([]Budget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Budget, 0, len(m.budgets))
	for _, b := range m.budgets {
		out = append(out, b.b)
	}
	sortBudgets(out)
	return out, nil
}

func (m *Memory) MarkSoftAlert(_ context.Context, k BudgetKey, period time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return false, m.Fail
	}
	cur, ok := m.budgets[k]
	if !ok {
		return false, ErrNotFound
	}
	if cur.alerted.Equal(period) {
		return false, nil
	}
	cur.alerted = period
	return true, nil
}

func (m *Memory) SpendRows(_ context.Context, since time.Time) ([]SpendRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	agg := map[string]*SpendRow{}
	for _, r := range m.records {
		if r.Time.Before(since) {
			continue
		}
		tags := dedupSorted(r.Tags)
		k := strings.Join([]string{r.Org, r.Team, r.Project, r.KeyID, r.User, r.Model, strings.Join(tags, ","), r.Unit, fmt.Sprint(r.Overflow)}, "\x00")
		row := agg[k]
		if row == nil {
			row = &SpendRow{Org: r.Org, Team: r.Team, Project: r.Project, KeyID: r.KeyID, User: r.User, Model: r.Model, Tags: tags, Unit: r.Unit,
				Overflow: r.Overflow}
			agg[k] = row
		}
		row.Cost += r.Cost
	}
	out := make([]SpendRow, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	return out, nil
}

// ---- PostgreSQL ----

const budgetsSchema = `
CREATE TABLE IF NOT EXISTS llmflow_budgets_v2 (
	kind          text NOT NULL,
	name          text NOT NULL,
	model         text NOT NULL DEFAULT '',
	period        text NOT NULL,
	max_cost      double precision NOT NULL,
	soft_limit    double precision NOT NULL DEFAULT 0,
	temp_increase double precision NOT NULL DEFAULT 0,
	temp_expiry   timestamptz,
	soft_alerted  timestamptz,
	PRIMARY KEY (kind, name, model)
);
INSERT INTO llmflow_budgets_v2 (kind, name, period, max_cost)
	SELECT 'team', team, period, max_cost FROM llmflow_budgets ON CONFLICT DO NOTHING;
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS tags text[] NOT NULL DEFAULT '{}';
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS tags text[] NOT NULL DEFAULT '{}';
-- v0.12.0: costs carry a unit ('' = GPU-hours, the only unit before), and a
-- budget is per unit: the primary key gains the unit once.
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS unit text NOT NULL DEFAULT '';
ALTER TABLE llmflow_budgets_v2 ADD COLUMN IF NOT EXISTS unit text NOT NULL DEFAULT '';
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = 'llmflow_budgets_v2'::regclass AND i.indisprimary AND a.attname = 'unit'
	) THEN
		ALTER TABLE llmflow_budgets_v2 DROP CONSTRAINT IF EXISTS llmflow_budgets_v2_pkey;
		ALTER TABLE llmflow_budgets_v2 ADD PRIMARY KEY (kind, name, model, unit);
	END IF;
END $$;
-- v0.17.0, hard budgets (#87): the ledger (ledger_pg.go).
ALTER TABLE llmflow_budgets_v2 ADD COLUMN IF NOT EXISTS limit_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS cost_nano bigint;
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS admitted_at timestamptz;
CREATE TABLE IF NOT EXISTS llmflow_budget_ledger (
	kind         text NOT NULL,
	name         text NOT NULL,
	model        text NOT NULL,
	unit         text NOT NULL,
	period_start timestamptz NOT NULL,
	spent_nano   bigint NOT NULL DEFAULT 0 CHECK (spent_nano >= 0),
	PRIMARY KEY (kind, name, model, unit, period_start)
);
CREATE TABLE IF NOT EXISTS llmflow_budget_grants (
	kind         text NOT NULL,
	name         text NOT NULL,
	model        text NOT NULL,
	unit         text NOT NULL,
	period_start timestamptz NOT NULL,
	replica      text NOT NULL,
	incarnation  text NOT NULL,
	granted_nano bigint NOT NULL CHECK (granted_nano >= 0),
	PRIMARY KEY (kind, name, model, unit, period_start, replica, incarnation)
);
CREATE INDEX IF NOT EXISTS llmflow_budget_grants_incarnation ON llmflow_budget_grants (incarnation);
-- v0.17.1: arming budgets (ledger.go).
ALTER TABLE llmflow_budgets_v2 ADD COLUMN IF NOT EXISTS arming boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS llmflow_budget_holders (
	incarnation text PRIMARY KEY,
	replica     text NOT NULL,
	started_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS llmflow_budget_arming (
	kind        text NOT NULL,
	name        text NOT NULL,
	model       text NOT NULL,
	unit        text NOT NULL,
	incarnation text NOT NULL,
	pre_nano    bigint NOT NULL CHECK (pre_nano >= 0),
	PRIMARY KEY (kind, name, model, unit, incarnation)
);
-- v0.18.0, workload profiles (#90): a rolling window's spent, by bucket
-- (ledger.go); the profile in force and the overflow flag of each record.
CREATE TABLE IF NOT EXISTS llmflow_budget_buckets (
	kind         text NOT NULL,
	name         text NOT NULL,
	model        text NOT NULL,
	unit         text NOT NULL,
	bucket_start timestamptz NOT NULL,
	spent_nano   bigint NOT NULL CHECK (spent_nano >= 0),
	PRIMARY KEY (kind, name, model, unit, bucket_start)
);
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS profile text NOT NULL DEFAULT '';
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS overflow boolean NOT NULL DEFAULT false;
`

func (p *Postgres) SetBudget(ctx context.Context, b Budget) error {
	return p.SetBudgetChecked(ctx, b, time.Now(), 0, true)
}

func (p *Postgres) IncreaseBudget(ctx context.Context, k BudgetKey, amount float64, expiry time.Time) error {
	return p.IncreaseBudgetChecked(ctx, k, amount, expiry, time.Now(), 0, true)
}

func (p *Postgres) DeleteBudget(ctx context.Context, k BudgetKey) error {
	return affected(p.pool.Exec(ctx, `DELETE FROM llmflow_budgets_v2 WHERE kind = $1 AND name = $2 AND model = $3 AND unit = $4`,
		string(k.Kind), k.Name, k.Model, k.Unit))
}

func (p *Postgres) Budgets(ctx context.Context) ([]Budget, error) {
	rows, err := p.pool.Query(ctx, `SELECT kind, name, model, unit, period, max_cost, soft_limit, temp_increase, temp_expiry, limit_epoch, arming
		FROM llmflow_budgets_v2`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Budget, error) {
		var b Budget
		var kind, period string
		err := r.Scan(&kind, &b.Name, &b.Model, &b.Unit, &period, &b.MaxCost, &b.SoftLimit, &b.TempIncrease, &b.TempExpiry, &b.Epoch, &b.Arming)
		b.Kind, b.Period = ScopeKind(kind), Period(period)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	sortBudgets(out)
	return out, nil
}

func (p *Postgres) MarkSoftAlert(ctx context.Context, k BudgetKey, period time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE llmflow_budgets_v2 SET soft_alerted = $5
		WHERE kind = $1 AND name = $2 AND model = $3 AND unit = $4 AND soft_alerted IS DISTINCT FROM $5`,
		string(k.Kind), k.Name, k.Model, k.Unit, period.UTC())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_budgets_v2 WHERE kind = $1 AND name = $2 AND model = $3 AND unit = $4)`,
		string(k.Kind), k.Name, k.Model, k.Unit).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

func (p *Postgres) SpendRows(ctx context.Context, since time.Time) ([]SpendRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT org, team, project, key_id, user_name, model, tags, unit, overflow, sum(cost) FROM llmflow_usage
		WHERE ts >= $1 GROUP BY org, team, project, key_id, user_name, model, tags, unit, overflow`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SpendRow, error) {
		var s SpendRow
		err := r.Scan(&s.Org, &s.Team, &s.Project, &s.KeyID, &s.User, &s.Model, &s.Tags, &s.Unit, &s.Overflow, &s.Cost)
		s.Tags = dedupSorted(s.Tags)
		return s, err
	})
}
