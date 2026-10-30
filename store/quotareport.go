package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Quotas in the decision report (#97, item 3). Three things are stored for it.
//
//   - Soft-quota events: every threshold crossing the gateway notified, one
//     row per (person, window, threshold, instance) with its time. The marks
//     of soft_quotas.go keep only the LAST instance per (person, window,
//     threshold), so a month of weekly windows would count one; the event is
//     written in the same transaction as the mark. Events before this release
//     are the marks that exist (the schema copies them once).
//   - Hard refusals: requests refused with 429 because a budget or a workload
//     profile was exhausted, counted per day, organisation, team, profile and
//     kind. No person is stored: the counts are aggregates from the start.
//   - The assignment history, already stored (llmflow_profile_assignments).

// WarnEventRow is a (person, window, threshold) with the number of
// notifications in the period.
type WarnEventRow struct {
	User, Window string
	Threshold    int
	N            int64
}

// Refusal kinds.
const (
	RefusalBudget          = "budget"
	RefusalProfileSession  = "profile_session"
	RefusalProfileWeek     = "profile_week"
	RefusalProfileOverflow = "profile_overflow"
)

// RefusalRow is a count of hard refusals.
type RefusalRow struct {
	Org, Team, Profile, Kind string
	N                        int64
}

// QuotaReportStore is the store's part of the quota figures.
type QuotaReportStore interface {
	// WarnEventsBetween counts the notifications fired in [from, to) per
	// (person, window, threshold); ErrTooMany beyond max rows.
	WarnEventsBetween(ctx context.Context, from, to time.Time, max int) ([]WarnEventRow, error)
	// AddRefusals adds counts to the day's totals.
	AddRefusals(ctx context.Context, day time.Time, rows []RefusalRow) error
	// RefusalsBetween sums the refusals of the days in [from, to) per
	// (org, team, profile, kind); ErrTooMany beyond max rows.
	RefusalsBetween(ctx context.Context, from, to time.Time, max int) ([]RefusalRow, error)
	// AssignmentsBetween lists the assignments in force at any time in
	// [from, to); ErrTooMany beyond max.
	AssignmentsBetween(ctx context.Context, from, to time.Time, max int) ([]Assignment, error)
}

func validRefusalKind(k string) bool {
	switch k {
	case RefusalBudget, RefusalProfileSession, RefusalProfileWeek, RefusalProfileOverflow:
		return true
	}
	return false
}

func validRefusalRow(r RefusalRow) error {
	for _, s := range []string{r.Org, r.Team, r.Profile} {
		if len(s) > 140 || strings.ContainsAny(s, "\x00\r\n") {
			return fmt.Errorf("refusal: bad name %q", s)
		}
	}
	if !validRefusalKind(r.Kind) || r.N < 0 {
		return fmt.Errorf("refusal: kind %q count %d", r.Kind, r.N)
	}
	return nil
}

// ---- memory ----

type memEvent struct {
	user, window string
	threshold    int
	instance, at time.Time
}

type memQuota struct {
	events   []memEvent
	refusals map[string]map[[4]string]int64 // day → (org, team, profile, kind) → n
}

func (m *Memory) recordWarnEventLocked(w WarnMark) {
	at := w.At
	if at.IsZero() {
		at = time.Now()
	}
	m.quota.events = append(m.quota.events, memEvent{w.User, w.Window, w.Threshold, w.Instance.UTC(), at.UTC()})
}

func (m *Memory) WarnEventsBetween(ctx context.Context, from, to time.Time, max int) ([]WarnEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	agg := map[[3]string]*WarnEventRow{}
	for _, e := range m.quota.events {
		if e.at.Before(from) || !e.at.Before(to) {
			continue
		}
		k := [3]string{e.user, e.window, fmt.Sprint(e.threshold)}
		if a := agg[k]; a != nil {
			a.N++
			continue
		}
		if len(agg) == max {
			return nil, ErrTooMany
		}
		agg[k] = &WarnEventRow{User: e.user, Window: e.window, Threshold: e.threshold, N: 1}
	}
	out := make([]WarnEventRow, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.User != b.User {
			return a.User < b.User
		}
		if a.Window != b.Window {
			return a.Window < b.Window
		}
		return a.Threshold < b.Threshold
	})
	return out, nil
}

func (m *Memory) AddRefusals(_ context.Context, day time.Time, rows []RefusalRow) error {
	for _, r := range rows {
		if err := validRefusalRow(r); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.quota.refusals == nil {
		m.quota.refusals = map[string]map[[4]string]int64{}
	}
	d := day.UTC().Format("2006-01-02")
	if m.quota.refusals[d] == nil {
		m.quota.refusals[d] = map[[4]string]int64{}
	}
	for _, r := range rows {
		m.quota.refusals[d][[4]string{r.Org, r.Team, r.Profile, r.Kind}] += r.N
	}
	return nil
}

func (m *Memory) RefusalsBetween(ctx context.Context, from, to time.Time, max int) ([]RefusalRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	f, t := from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")
	agg := map[[4]string]int64{}
	for d, rows := range m.quota.refusals {
		if d < f || d >= t {
			continue
		}
		for k, n := range rows {
			agg[k] += n
		}
	}
	if len(agg) > max {
		return nil, ErrTooMany
	}
	out := make([]RefusalRow, 0, len(agg))
	for k, n := range agg {
		out = append(out, RefusalRow{k[0], k[1], k[2], k[3], n})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Org+"\x00"+a.Team+"\x00"+a.Profile+"\x00"+a.Kind < b.Org+"\x00"+b.Team+"\x00"+b.Profile+"\x00"+b.Kind
	})
	return out, nil
}

func (m *Memory) AssignmentsBetween(ctx context.Context, from, to time.Time, max int) ([]Assignment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []Assignment
	for _, h := range m.prof.hist {
		if !h.From.Before(to) || h.To != nil && !h.To.After(from) {
			continue
		}
		if len(out) == max {
			return nil, ErrTooMany
		}
		out = append(out, h)
	}
	return out, nil
}

// ---- PostgreSQL ----

const quotaReportSchema = `
CREATE TABLE IF NOT EXISTS llmflow_warn_events (
	user_name text NOT NULL,
	win       text NOT NULL,
	threshold integer NOT NULL,
	instance  timestamptz NOT NULL,
	fired_at  timestamptz NOT NULL,
	PRIMARY KEY (user_name, win, threshold, instance)
);
CREATE INDEX IF NOT EXISTS llmflow_warn_events_at ON llmflow_warn_events (fired_at);
INSERT INTO llmflow_warn_events (user_name, win, threshold, instance, fired_at)
	SELECT user_name, win, threshold, instance, fired_at FROM llmflow_warn_marks ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS llmflow_refusals (
	day     date NOT NULL,
	org     text NOT NULL,
	team    text NOT NULL,
	profile text NOT NULL,
	kind    text NOT NULL,
	n       bigint NOT NULL,
	PRIMARY KEY (day, org, team, profile, kind)
);
`

func (p *Postgres) WarnEventsBetween(ctx context.Context, from, to time.Time, max int) ([]WarnEventRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT user_name, win, threshold, count(*) FROM llmflow_warn_events
		WHERE fired_at >= $1 AND fired_at < $2 GROUP BY user_name, win, threshold ORDER BY user_name, win, threshold LIMIT $3`, from, to, max+1)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WarnEventRow, error) {
		var w WarnEventRow
		err := r.Scan(&w.User, &w.Window, &w.Threshold, &w.N)
		return w, err
	})
	if err == nil && len(out) > max {
		return nil, ErrTooMany
	}
	return out, err
}

func (p *Postgres) AddRefusals(ctx context.Context, day time.Time, rows []RefusalRow) error {
	for _, r := range rows {
		if err := validRefusalRow(r); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_refusals (day, org, team, profile, kind, n) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (day, org, team, profile, kind) DO UPDATE SET n = llmflow_refusals.n + EXCLUDED.n`,
				day.UTC().Format("2006-01-02"), r.Org, r.Team, r.Profile, r.Kind, r.N); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) RefusalsBetween(ctx context.Context, from, to time.Time, max int) ([]RefusalRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT org, team, profile, kind, sum(n)::bigint FROM llmflow_refusals
		WHERE day >= $1::date AND day < $2::date GROUP BY org, team, profile, kind ORDER BY org, team, profile, kind LIMIT $3`,
		from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"), max+1)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RefusalRow, error) {
		var x RefusalRow
		err := r.Scan(&x.Org, &x.Team, &x.Profile, &x.Kind, &x.N)
		return x, err
	})
	if err == nil && len(out) > max {
		return nil, ErrTooMany
	}
	return out, err
}

func (p *Postgres) AssignmentsBetween(ctx context.Context, from, to time.Time, max int) ([]Assignment, error) {
	rows, err := p.pool.Query(ctx, `SELECT user_name, profile, overflow, set_by, valid_from, valid_to FROM llmflow_profile_assignments
		WHERE valid_from < $2 AND (valid_to IS NULL OR valid_to > $1) ORDER BY id LIMIT $3`, from, to, max+1)
	if err != nil {
		return nil, err
	}
	out, err := scanAssignments(rows)
	if err == nil && len(out) > max {
		return nil, ErrTooMany
	}
	return out, err
}

var (
	_ QuotaReportStore = (*Memory)(nil)
	_ QuotaReportStore = (*Postgres)(nil)
)
