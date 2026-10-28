package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Workload profiles (#90). The profiles themselves are configuration
// (config.Profile); the store keeps who has which (an assignment, with its
// history, so that a report can say which profile was in force when) and
// the calibrated value of a workday per cost unit, which a proxy admin
// accepts from measured usage.

// Bounds of the profile data.
const (
	MaxProfileID = 32
	// MaxWorkday bounds a workday's value in any unit: the profile budgets
	// it becomes (at most 7 workdays a week, 31×7 a month) stay far within
	// the budget amounts the store accepts (1e15).
	MaxWorkday = 1e9
)

// Assignment is a user's workload profile from From until To (nil: in
// force). Overflow lets the user continue beyond the profile's allowance on
// its monthly overflow cap. SetBy is who decided it.
type Assignment struct {
	User     string     `json:"user"`
	Profile  string     `json:"profile"`
	Overflow bool       `json:"overflow"`
	SetBy    string     `json:"set_by"`
	From     time.Time  `json:"from"`
	To       *time.Time `json:"to,omitempty"`
}

// Workday is the calibrated consumption of one person-day, in a unit.
type Workday struct {
	Unit   string    `json:"unit"` // "" is GPU-hours
	Amount float64   `json:"amount"`
	SetBy  string    `json:"set_by"`
	SetAt  time.Time `json:"set_at"`
}

// ProfileStore is the store's part of workload profiles.
type ProfileStore interface {
	// AssignProfile ends the user's assignment in force (if any) at at and,
	// unless profile is "", starts the new one. It refuses an invalid user
	// or profile id.
	AssignProfile(ctx context.Context, a Assignment) error
	// Assignments lists the assignments in force.
	Assignments(ctx context.Context) ([]Assignment, error)
	// AssignmentHistory lists a user's assignments, oldest first.
	AssignmentHistory(ctx context.Context, user string) ([]Assignment, error)
	// SetWorkday records an accepted calibration; Workdays lists them.
	SetWorkday(ctx context.Context, w Workday) error
	Workdays(ctx context.Context) ([]Workday, error)
}

// ValidProfileID accepts [a-z0-9_-]{1,32}.
func ValidProfileID(s string) bool {
	if len(s) == 0 || len(s) > MaxProfileID {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// validUser accepts what can name a user in a record: a gateway user name
// or a JWT's synthetic id (printable ASCII, at most 140 bytes).
func validUser(s string) bool {
	if len(s) == 0 || len(s) > 140 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validateAssignment(a Assignment) error {
	if !validUser(a.User) {
		return fmt.Errorf("user %q is invalid", a.User)
	}
	if a.Profile != "" && !ValidProfileID(a.Profile) {
		return fmt.Errorf("profile %q: want [a-z0-9_-]{1,32}", a.Profile)
	}
	if len(a.SetBy) > 140 {
		return errors.New("set_by is too long")
	}
	return nil
}

func validateWorkday(w Workday) error {
	if u, err := NormalizeUnit(w.Unit); err != nil {
		return err
	} else if u != w.Unit {
		return fmt.Errorf("unit %q: store GPU-hours as \"\"", w.Unit)
	}
	if !(w.Amount > 0 && w.Amount <= MaxWorkday) || math.IsInf(w.Amount, 0) {
		return fmt.Errorf("workday %v: want more than 0 and at most %g", w.Amount, float64(MaxWorkday))
	}
	if len(w.SetBy) > 140 {
		return errors.New("set_by is too long")
	}
	return nil
}

// ---- memory ----

type memProfiles struct {
	hist     []Assignment // every assignment, in order
	workdays map[string]Workday
}

func (m *Memory) AssignProfile(_ context.Context, a Assignment) error {
	if err := validateAssignment(a); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	at := a.From.UTC()
	for i := range m.prof.hist {
		if h := &m.prof.hist[i]; h.User == a.User && h.To == nil {
			h.To = &at
		}
	}
	if a.Profile != "" {
		a.From, a.To = at, nil
		m.prof.hist = append(m.prof.hist, a)
	}
	return nil
}

func (m *Memory) Assignments(context.Context) ([]Assignment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Assignment{}
	for _, h := range m.prof.hist {
		if h.To == nil {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (m *Memory) AssignmentHistory(_ context.Context, user string) ([]Assignment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Assignment{}
	for _, h := range m.prof.hist {
		if h.User == user {
			out = append(out, h)
		}
	}
	return out, nil
}

func (m *Memory) SetWorkday(_ context.Context, w Workday) error {
	if err := validateWorkday(w); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.prof.workdays == nil {
		m.prof.workdays = map[string]Workday{}
	}
	w.SetAt = w.SetAt.UTC()
	m.prof.workdays[w.Unit] = w
	return nil
}

func (m *Memory) Workdays(context.Context) ([]Workday, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Workday{}
	for _, w := range m.prof.workdays {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit < out[j].Unit })
	return out, nil
}

// ---- PostgreSQL ----

const profilesSchema = `
CREATE TABLE IF NOT EXISTS llmflow_profile_assignments (
	id         bigserial PRIMARY KEY,
	user_name  text NOT NULL,
	profile    text NOT NULL,
	overflow   boolean NOT NULL DEFAULT false,
	set_by     text NOT NULL DEFAULT '',
	valid_from timestamptz NOT NULL,
	valid_to   timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS llmflow_profile_assignments_current ON llmflow_profile_assignments (user_name) WHERE valid_to IS NULL;
CREATE TABLE IF NOT EXISTS llmflow_workdays (
	unit   text PRIMARY KEY,
	amount double precision NOT NULL,
	set_by text NOT NULL DEFAULT '',
	set_at timestamptz NOT NULL
);
`

func (p *Postgres) AssignProfile(ctx context.Context, a Assignment) error {
	if err := validateAssignment(a); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// Two administrators assigning the same person at once: the second
		// waits, then ends the first's assignment.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('profile:' || $1, 0))`, a.User); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE llmflow_profile_assignments SET valid_to = $2 WHERE user_name = $1 AND valid_to IS NULL`,
			a.User, a.From.UTC()); err != nil {
			return err
		}
		if a.Profile == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO llmflow_profile_assignments (user_name, profile, overflow, set_by, valid_from) VALUES ($1, $2, $3, $4, $5)`,
			a.User, a.Profile, a.Overflow, a.SetBy, a.From.UTC())
		return err
	})
}

func scanAssignments(rows pgx.Rows) ([]Assignment, error) {
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Assignment, error) {
		var a Assignment
		err := r.Scan(&a.User, &a.Profile, &a.Overflow, &a.SetBy, &a.From, &a.To)
		a.From = a.From.UTC()
		if a.To != nil {
			t := a.To.UTC()
			a.To = &t
		}
		return a, err
	})
	if out == nil {
		out = []Assignment{}
	}
	return out, err
}

func (p *Postgres) Assignments(ctx context.Context) ([]Assignment, error) {
	rows, err := p.pool.Query(ctx, `SELECT user_name, profile, overflow, set_by, valid_from, valid_to FROM llmflow_profile_assignments
		WHERE valid_to IS NULL ORDER BY user_name`)
	if err != nil {
		return nil, err
	}
	return scanAssignments(rows)
}

func (p *Postgres) AssignmentHistory(ctx context.Context, user string) ([]Assignment, error) {
	rows, err := p.pool.Query(ctx, `SELECT user_name, profile, overflow, set_by, valid_from, valid_to FROM llmflow_profile_assignments
		WHERE user_name = $1 ORDER BY id`, user)
	if err != nil {
		return nil, err
	}
	return scanAssignments(rows)
}

func (p *Postgres) SetWorkday(ctx context.Context, w Workday) error {
	if err := validateWorkday(w); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_workdays (unit, amount, set_by, set_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (unit) DO UPDATE SET amount = EXCLUDED.amount, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at`,
		w.Unit, w.Amount, w.SetBy, w.SetAt.UTC())
	return err
}

func (p *Postgres) Workdays(ctx context.Context) ([]Workday, error) {
	rows, err := p.pool.Query(ctx, `SELECT unit, amount, set_by, set_at FROM llmflow_workdays ORDER BY unit`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Workday, error) {
		var w Workday
		err := r.Scan(&w.Unit, &w.Amount, &w.SetBy, &w.SetAt)
		w.SetAt = w.SetAt.UTC()
		return w, err
	})
	if out == nil {
		out = []Workday{}
	}
	return out, err
}
