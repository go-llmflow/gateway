package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Adoption figures (#97): how much a platform is used, never by whom. Only
// counts leave the store: a name never does.

// AdoptionPoint is a day or a week (its Monday): distinct people who made a
// request (a request of a service key, which has no person, counts as a
// request and as no one), and requests.
type AdoptionPoint struct {
	Day         string `json:"period"`
	ActiveUsers int64  `json:"active_users"`
	Requests    int64  `json:"requests"`
}

// AdoptionModel is a model's use over the period.
type AdoptionModel struct {
	Model    string `json:"model"`
	Users    int64  `json:"active_users"`
	Requests int64  `json:"requests"`
}

// Adoption is the adoption of a period.
type Adoption struct {
	Daily, Weekly []AdoptionPoint
	Models        []AdoptionModel
	// Users is the distinct people over the whole period.
	Users int64
	// Hide is the cells the caller's secondary suppression adds, by dimension
	// (DimDay, DimWeek, DimModel); MergedUsers is the distinct people over the
	// models it hides. Set by the caller, not by Adoption.
	Hide        map[string]map[string]bool
	MergedUsers int64
}

// The dimensions of an adoption report, for UsersOf.
const (
	DimDay   = "day"   // keys are YYYY-MM-DD (UTC)
	DimWeek  = "week"  // keys are the Monday, YYYY-MM-DD (UTC)
	DimModel = "model" // keys are model names
)

// AdoptionUnionStore counts the distinct people over a set of cells of one
// dimension: the union, never a sum (one person on two days is one person).
type AdoptionUnionStore interface {
	UsersOf(ctx context.Context, from, to time.Time, orgs []string, dim string, keys []string) (int64, error)
}

func (m *Memory) UsersOf(ctx context.Context, from, to time.Time, orgs []string, dim string, keys []string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key, err := memAdoptionKey(dim)
	if err != nil {
		return 0, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	users := map[string]bool{}
	for _, r := range m.records {
		if r.Time.Before(from) || !r.Time.Before(to) || orgs != nil && !contains(orgs, r.Org) || r.User == "" || !contains(keys, key(r)) {
			continue
		}
		users[r.User] = true
	}
	return int64(len(users)), nil
}

func memAdoptionKey(dim string) (func(Record) string, error) {
	switch dim {
	case DimDay:
		return func(r Record) string { return r.Time.UTC().Format("2006-01-02") }, nil
	case DimWeek:
		return func(r Record) string { return weekStart(r.Time) }, nil
	case DimModel:
		return func(r Record) string { return r.Model }, nil
	}
	return nil, fmt.Errorf("store: unknown adoption dimension %q", dim)
}

// pgAdoptionKey is the SQL expression of a dimension: chosen from a fixed
// list, never built from the caller's text.
func pgAdoptionKey(dim string) (string, error) {
	switch dim {
	case DimDay:
		return `to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD')`, nil
	case DimWeek:
		return `to_char(date_trunc('week', ts AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`, nil
	case DimModel:
		return `model`, nil
	}
	return "", fmt.Errorf("store: unknown adoption dimension %q", dim)
}

func (p *Postgres) UsersOf(ctx context.Context, from, to time.Time, orgs []string, dim string, keys []string) (int64, error) {
	expr, err := pgAdoptionKey(dim)
	if err != nil {
		return 0, err
	}
	var n int64
	err = p.pool.QueryRow(ctx, `SELECT count(DISTINCT user_name) FILTER (WHERE user_name <> '') FROM llmflow_usage
		WHERE ts >= $1 AND ts < $2 AND ($3::text[] IS NULL OR org = ANY($3)) AND `+expr+` = ANY($4)`, from, to, orgs, keys).Scan(&n)
	return n, err
}

// AdoptionStore counts adoption. orgs restricts it to organisations (nil:
// all).
type AdoptionStore interface {
	Adoption(ctx context.Context, from, to time.Time, orgs []string) (*Adoption, error)
}

func weekStart(t time.Time) string {
	t = t.UTC()
	d := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func (m *Memory) Adoption(ctx context.Context, from, to time.Time, orgs []string) (*Adoption, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	type acc struct {
		users map[string]bool
		reqs  int64
	}
	newAcc := func() *acc { return &acc{users: map[string]bool{}} }
	day, week, model := map[string]*acc{}, map[string]*acc{}, map[string]*acc{}
	all := newAcc()
	touch := func(mm map[string]*acc, k string, user string) {
		a := mm[k]
		if a == nil {
			a = newAcc()
			mm[k] = a
		}
		a.reqs++
		if user != "" {
			a.users[user] = true
		}
	}
	for _, r := range m.records {
		if r.Time.Before(from) || !r.Time.Before(to) || orgs != nil && !contains(orgs, r.Org) {
			continue
		}
		touch(day, r.Time.UTC().Format("2006-01-02"), r.User)
		touch(week, weekStart(r.Time), r.User)
		touch(model, r.Model, r.User)
		if r.User != "" {
			all.users[r.User] = true
		}
	}
	out := &Adoption{Users: int64(len(all.users))}
	points := func(mm map[string]*acc) []AdoptionPoint {
		ks := make([]string, 0, len(mm))
		for k := range mm {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		ps := make([]AdoptionPoint, 0, len(ks))
		for _, k := range ks {
			ps = append(ps, AdoptionPoint{k, int64(len(mm[k].users)), mm[k].reqs})
		}
		return ps
	}
	out.Daily, out.Weekly = points(day), points(week)
	for _, p := range points(model) {
		out.Models = append(out.Models, AdoptionModel{Model: p.Day, Users: p.ActiveUsers, Requests: p.Requests})
	}
	return out, nil
}

func (p *Postgres) Adoption(ctx context.Context, from, to time.Time, orgs []string) (*Adoption, error) {
	filter := `ts >= $1 AND ts < $2 AND ($3::text[] IS NULL OR org = ANY($3))`
	pts := func(expr string) ([]AdoptionPoint, error) {
		rows, err := p.pool.Query(ctx, `SELECT `+expr+` AS k, count(DISTINCT user_name) FILTER (WHERE user_name <> ''), count(*)
			FROM llmflow_usage WHERE `+filter+` GROUP BY k ORDER BY k`, from, to, orgs)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdoptionPoint, error) {
			var a AdoptionPoint
			err := r.Scan(&a.Day, &a.ActiveUsers, &a.Requests)
			return a, err
		})
	}
	out := &Adoption{}
	var err error
	if out.Daily, err = pts(`to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD')`); err != nil {
		return nil, err
	}
	if out.Weekly, err = pts(`to_char(date_trunc('week', ts AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`); err != nil {
		return nil, err
	}
	ms, err := pts(`model`)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		out.Models = append(out.Models, AdoptionModel{Model: m.Day, Users: m.ActiveUsers, Requests: m.Requests})
	}
	err = p.pool.QueryRow(ctx, `SELECT count(DISTINCT user_name) FILTER (WHERE user_name <> '') FROM llmflow_usage WHERE `+filter, from, to, orgs).Scan(&out.Users)
	return out, err
}

var (
	_ AdoptionStore = (*Memory)(nil)
	_ AdoptionStore = (*Postgres)(nil)
)
