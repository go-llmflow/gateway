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
)

// Valid reports whether k is a known scope kind.
func (k ScopeKind) Valid() bool {
	switch k {
	case ScopeOrg, ScopeTeam, ScopeProject, ScopeKey, ScopeTag:
		return true
	}
	return false
}

// BudgetKey identifies a budget: a scope, optionally restricted to one
// model group (Model "" covers every model).
type BudgetKey struct {
	Kind  ScopeKind `json:"kind"`
	Name  string    `json:"name"`
	Model string    `json:"model,omitempty"`
}

func (k BudgetKey) String() string { return string(k.Kind) + ":" + k.Name + "@" + k.Model }

// Budget caps the cost (GPU-hours) of a scope per period. A temporary
// increase adds to MaxCost until it expires; SoftLimit (0 = none) triggers
// one alert per period when the spend reaches it.
type Budget struct {
	BudgetKey
	Period       Period     `json:"period"`
	MaxCost      float64    `json:"max_cost"`
	SoftLimit    float64    `json:"soft_limit,omitempty"`
	TempIncrease float64    `json:"temp_increase,omitempty"`
	TempExpiry   *time.Time `json:"temp_expiry,omitempty"`
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
		return fmt.Errorf("kind %q: want org, team, project, key or tag", b.Kind)
	}
	if b.Name == "" || len(b.Name) > 140 || strings.ContainsAny(b.Name, "\x00\r\n") {
		return fmt.Errorf("name %q is invalid", b.Name)
	}
	if len(b.Model) > 256 || strings.ContainsAny(b.Model, "\x00\r\n") {
		return fmt.Errorf("model %q is invalid", b.Model)
	}
	if !b.Period.Valid() {
		return fmt.Errorf("period %q: want day, week or month", b.Period)
	}
	if !validAmount(b.MaxCost) {
		return fmt.Errorf("max_cost %v: want 0..1e15", b.MaxCost)
	}
	if !validAmount(b.SoftLimit) {
		return fmt.Errorf("soft_limit %v: want 0..1e15", b.SoftLimit)
	}
	return nil
}

// SpendRow is the cost of one combination of attributes since a time.
type SpendRow struct {
	Org, Team, Project, KeyID, Model string
	Tags                             []string
	Cost                             float64
}

// ---- memory ----

type memBudget struct {
	b       Budget
	alerted time.Time
}

func (m *Memory) SetBudget(_ context.Context, b Budget) error {
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
	if cur, ok := m.budgets[b.BudgetKey]; ok {
		b.TempIncrease, b.TempExpiry = cur.b.TempIncrease, cur.b.TempExpiry
		cur.b = b
		return nil
	}
	b.TempIncrease, b.TempExpiry = 0, nil
	m.budgets[b.BudgetKey] = &memBudget{b: b}
	return nil
}

func (m *Memory) IncreaseBudget(_ context.Context, k BudgetKey, amount float64, expiry time.Time) error {
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
	e := expiry.UTC()
	cur.b.TempIncrease, cur.b.TempExpiry = amount, &e
	return nil
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
		k := strings.Join([]string{r.Org, r.Team, r.Project, r.KeyID, r.Model, strings.Join(tags, ",")}, "\x00")
		row := agg[k]
		if row == nil {
			row = &SpendRow{Org: r.Org, Team: r.Team, Project: r.Project, KeyID: r.KeyID, Model: r.Model, Tags: tags}
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
`

func (p *Postgres) SetBudget(ctx context.Context, b Budget) error {
	if err := ValidateBudget(b); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_budgets_v2 (kind, name, model, period, max_cost, soft_limit) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (kind, name, model) DO UPDATE
		SET period = EXCLUDED.period, max_cost = EXCLUDED.max_cost, soft_limit = EXCLUDED.soft_limit`,
		string(b.Kind), b.Name, b.Model, string(b.Period), b.MaxCost, b.SoftLimit)
	return err
}

func (p *Postgres) IncreaseBudget(ctx context.Context, k BudgetKey, amount float64, expiry time.Time) error {
	if !validAmount(amount) {
		return fmt.Errorf("amount %v: want 0..1e15", amount)
	}
	return affected(p.pool.Exec(ctx, `UPDATE llmflow_budgets_v2 SET temp_increase = $4, temp_expiry = $5
		WHERE kind = $1 AND name = $2 AND model = $3`, string(k.Kind), k.Name, k.Model, amount, expiry.UTC()))
}

func (p *Postgres) DeleteBudget(ctx context.Context, k BudgetKey) error {
	return affected(p.pool.Exec(ctx, `DELETE FROM llmflow_budgets_v2 WHERE kind = $1 AND name = $2 AND model = $3`,
		string(k.Kind), k.Name, k.Model))
}

func (p *Postgres) Budgets(ctx context.Context) ([]Budget, error) {
	rows, err := p.pool.Query(ctx, `SELECT kind, name, model, period, max_cost, soft_limit, temp_increase, temp_expiry
		FROM llmflow_budgets_v2`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Budget, error) {
		var b Budget
		var kind, period string
		err := r.Scan(&kind, &b.Name, &b.Model, &period, &b.MaxCost, &b.SoftLimit, &b.TempIncrease, &b.TempExpiry)
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
	tag, err := p.pool.Exec(ctx, `UPDATE llmflow_budgets_v2 SET soft_alerted = $4
		WHERE kind = $1 AND name = $2 AND model = $3 AND soft_alerted IS DISTINCT FROM $4`,
		string(k.Kind), k.Name, k.Model, period.UTC())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_budgets_v2 WHERE kind = $1 AND name = $2 AND model = $3)`,
		string(k.Kind), k.Name, k.Model).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

func (p *Postgres) SpendRows(ctx context.Context, since time.Time) ([]SpendRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT org, team, project, key_id, model, tags, sum(cost) FROM llmflow_usage
		WHERE ts >= $1 GROUP BY org, team, project, key_id, model, tags`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SpendRow, error) {
		var s SpendRow
		err := r.Scan(&s.Org, &s.Team, &s.Project, &s.KeyID, &s.Model, &s.Tags, &s.Cost)
		s.Tags = dedupSorted(s.Tags)
		return s, err
	})
}
