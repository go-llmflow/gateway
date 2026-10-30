package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/internal/warnat"
	"github.com/jackc/pgx/v5"
)

// Soft quotas (#96): the store's part. Two things live here.
//
// Marks say which threshold was passed in which window instance, so that a
// notification fires once however many replicas see the crossing and however
// often they are killed. A mark is one row per (person, window, threshold):
//
//	week, a budget on a calendar period: Instance is the window's start (the
//	  ISO week's Monday 00:00 UTC, the day, the month). A threshold fires when
//	  the row is absent or holds an earlier instance; it cannot fire again in
//	  the same instance.
//	session, a rolling window: it has no instance id (it is 5 hours of 5-minute
//	  buckets, sliding). The instance is the bucket in which the first replica
//	  to see the crossing fired it (BucketOf), and the row is armed or not: a
//	  fired threshold is armed again when usage falls below the threshold minus
//	  a hysteresis (warnat.RearmBelow), and only then can fire again.
//
// The decision is one conditional INSERT ... ON CONFLICT DO UPDATE: the row
// lock makes two replicas serialise, the second sees the first's result and
// fires nothing. In PostgreSQL the notification's outbox rows are inserted in
// the same transaction as the mark, so a replica killed between the two
// leaves neither (the next one fires), never a mark without its notification.
//
// Levels are the administrators' runtime overrides of the thresholds of the
// gateway, an organisation or a profile (the configuration's values are the
// starting ones); each change is audited by the caller.

// WarnMark is one threshold of one window of one person.
type WarnMark struct {
	User string
	// Window: "session", "week", or "budget:<kind>:<name>:<model>:<unit>".
	Window    string
	Threshold int
	// Instance: the week's (or calendar period's) start, or, for a rolling
	// window, the bucket of the crossing.
	Instance time.Time
	// Rolling: the window slides (session), the mark is armed again by
	// RearmWarnings.
	Rolling bool
	// At is when the crossing is fired, for the event log of the reports (zero:
	// now).
	At time.Time
}

// Validate bounds a mark's fields.
func (m WarnMark) Validate() error {
	switch {
	case m.User == "" || len(m.User) > 140 || strings.ContainsAny(m.User, "\x00\r\n"):
		return fmt.Errorf("warn mark: user %q", m.User)
	case m.Window == "" || len(m.Window) > 400 || strings.ContainsAny(m.Window, "\x00\r\n"):
		return fmt.Errorf("warn mark: window %q", m.Window)
	case m.Threshold < warnat.Min || m.Threshold > warnat.Max:
		return fmt.Errorf("warn mark: threshold %d", m.Threshold)
	}
	return nil
}

// WarnOutbox is a delivery to queue with the mark (PostgreSQL's alert outbox;
// the memory store has none and ignores it).
type WarnOutbox struct {
	Key, Channel string
	Payload      []byte
}

// WarnLevel is one runtime override of warning thresholds.
type WarnLevel struct {
	Scope  string    `json:"scope"` // "gateway", "org" or "profile"
	Name   string    `json:"name"`  // "" for the gateway
	WarnAt []int     `json:"warn_at"`
	SetBy  string    `json:"set_by,omitempty"`
	SetAt  time.Time `json:"set_at,omitzero"`
}

// Level scopes.
const (
	WarnScopeGateway = "gateway"
	WarnScopeOrg     = "org"
	WarnScopeProfile = "profile"
)

// WarnStore is the store's part of soft quotas.
type WarnStore interface {
	// MarkWarning records that threshold of window was passed in instance and
	// reports whether this call is the one to notify: false when it already
	// was (by another replica, an earlier run), and while a rolling mark is
	// not armed. The outbox rows, when given, are queued in the same
	// transaction, only when it reports true.
	MarkWarning(ctx context.Context, m WarnMark, out []WarnOutbox) (bool, error)
	// RearmWarnings arms again the fired rolling thresholds of a person's
	// window whose RearmBelow lies above pct. It returns how many.
	RearmWarnings(ctx context.Context, user, window string, pct float64) (int, error)
	// WarnLevels lists the runtime overrides.
	WarnLevels(ctx context.Context) ([]WarnLevel, error)
	// SetWarnLevels sets an override (levels non-nil, possibly empty), or
	// removes it (levels nil).
	SetWarnLevels(ctx context.Context, scope, name string, levels []int, by string) error
}

// ValidWarnLevel checks an override's scope, name and list.
func ValidWarnLevel(scope, name string, levels []int) error {
	switch scope {
	case WarnScopeGateway:
		if name != "" {
			return errors.New("warn level: the gateway has no name")
		}
	case WarnScopeOrg, WarnScopeProfile:
		if name == "" || len(name) > 64 || strings.ContainsAny(name, "\x00\r\n") {
			return fmt.Errorf("warn level: %s name %q", scope, name)
		}
	default:
		return fmt.Errorf("warn level: scope %q: want gateway, org or profile", scope)
	}
	return warnat.Validate(levels)
}

var _ WarnStore = (*Memory)(nil)
var _ WarnStore = (*Postgres)(nil)

// ---- memory ----

type memMark struct {
	instance time.Time
	armed    bool
	rolling  bool
}

type memWarn struct {
	marks  map[[3]string]*memMark // user, window, threshold
	levels map[[2]string]WarnLevel
}

func markKey(m WarnMark) [3]string { return [3]string{m.User, m.Window, fmt.Sprint(m.Threshold)} }

func (m *Memory) MarkWarning(_ context.Context, w WarnMark, _ []WarnOutbox) (bool, error) {
	if err := w.Validate(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return false, m.Fail
	}
	if m.warn.marks == nil {
		m.warn.marks = map[[3]string]*memMark{}
	}
	k := markKey(w)
	cur, ok := m.warn.marks[k]
	switch {
	case !ok:
	case w.Rolling && cur.rolling && cur.armed:
	case !w.Rolling && !cur.rolling && cur.instance.Before(w.Instance):
	default:
		return false, nil
	}
	m.warn.marks[k] = &memMark{instance: w.Instance.UTC(), rolling: w.Rolling}
	m.recordWarnEventLocked(w)
	return true, nil
}

func (m *Memory) RearmWarnings(_ context.Context, user, window string, pct float64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	n := 0
	for k, mk := range m.warn.marks {
		if k[0] != user || k[1] != window || !mk.rolling || mk.armed {
			continue
		}
		var th int
		fmt.Sscan(k[2], &th)
		if float64(warnat.RearmBelow(th)) > pct {
			mk.armed = true
			n++
		}
	}
	return n, nil
}

func (m *Memory) WarnLevels(context.Context) ([]WarnLevel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]WarnLevel, 0, len(m.warn.levels))
	for _, l := range m.warn.levels {
		out = append(out, l)
	}
	sortLevels(out)
	return out, nil
}

func (m *Memory) SetWarnLevels(_ context.Context, scope, name string, levels []int, by string) error {
	if levels != nil {
		if err := ValidWarnLevel(scope, name, levels); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.warn.levels == nil {
		m.warn.levels = map[[2]string]WarnLevel{}
	}
	if levels == nil {
		delete(m.warn.levels, [2]string{scope, name})
		return nil
	}
	m.warn.levels[[2]string{scope, name}] = WarnLevel{Scope: scope, Name: name, WarnAt: append([]int{}, levels...), SetBy: by, SetAt: time.Now().UTC()}
	return nil
}

func sortLevels(l []WarnLevel) {
	sort.Slice(l, func(i, j int) bool {
		if l[i].Scope != l[j].Scope {
			return l[i].Scope < l[j].Scope
		}
		return l[i].Name < l[j].Name
	})
}

// ---- PostgreSQL ----

const warnSchema = `
CREATE TABLE IF NOT EXISTS llmflow_warn_marks (
	user_name  text NOT NULL,
	win        text NOT NULL,
	threshold  integer NOT NULL CHECK (threshold BETWEEN 1 AND 99),
	instance   timestamptz NOT NULL,
	rolling    boolean NOT NULL,
	armed      boolean NOT NULL DEFAULT false,
	fired_at   timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (user_name, win, threshold)
);
CREATE TABLE IF NOT EXISTS llmflow_warn_levels (
	scope   text NOT NULL,
	name    text NOT NULL,
	warn_at integer[] NOT NULL,
	set_by  text NOT NULL DEFAULT '',
	set_at  timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (scope, name)
);
ALTER TABLE llmflow_budgets_v2 ADD COLUMN IF NOT EXISTS warn_at integer[];
`

func (p *Postgres) MarkWarning(ctx context.Context, w WarnMark, out []WarnOutbox) (bool, error) {
	if err := w.Validate(); err != nil {
		return false, err
	}
	fired := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The insert wins the first time; later it updates the row only when
		// the window has moved on (a calendar period: an earlier instance), or
		// when a rolling threshold was armed again. The row lock serialises
		// two replicas: the second finds the first's row and changes nothing.
		tag, err := tx.Exec(ctx, `
			INSERT INTO llmflow_warn_marks (user_name, win, threshold, instance, rolling, armed)
			VALUES ($1, $2, $3, $4, $5, false)
			ON CONFLICT (user_name, win, threshold) DO UPDATE
			SET instance = EXCLUDED.instance, rolling = EXCLUDED.rolling, armed = false, fired_at = now()
			WHERE (EXCLUDED.rolling AND llmflow_warn_marks.rolling AND llmflow_warn_marks.armed)
			   OR (NOT EXCLUDED.rolling AND NOT llmflow_warn_marks.rolling AND llmflow_warn_marks.instance < EXCLUDED.instance)`,
			w.User, w.Window, w.Threshold, w.Instance.UTC(), w.Rolling)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		fired = true
		// The event log of the quota reports (quotareport.go): same transaction.
		at := w.At
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO llmflow_warn_events (user_name, win, threshold, instance, fired_at)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, w.User, w.Window, w.Threshold, w.Instance.UTC(), at.UTC()); err != nil {
			return err
		}
		for _, o := range out {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_alert_outbox (key, channel, payload) VALUES ($1, $2, $3)
				ON CONFLICT (key, channel) DO NOTHING`, o.Key, o.Channel, o.Payload); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return fired, nil
}

func (p *Postgres) RearmWarnings(ctx context.Context, user, window string, pct float64) (int, error) {
	// threshold - LEAST(5, threshold/2) is warnat.RearmBelow (the test
	// TestRearmSQLMatchesGo holds the two together).
	tag, err := p.pool.Exec(ctx, `UPDATE llmflow_warn_marks SET armed = true
		WHERE user_name = $1 AND win = $2 AND rolling AND NOT armed AND threshold - LEAST(5, threshold / 2) > $3::float8`,
		user, window, pct)
	return int(tag.RowsAffected()), err
}

func (p *Postgres) WarnLevels(ctx context.Context) ([]WarnLevel, error) {
	rows, err := p.pool.Query(ctx, `SELECT scope, name, warn_at, set_by, set_at FROM llmflow_warn_levels ORDER BY scope, name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WarnLevel, error) {
		var l WarnLevel
		var at []int32
		err := r.Scan(&l.Scope, &l.Name, &at, &l.SetBy, &l.SetAt)
		l.WarnAt = fromInt32(at)
		return l, err
	})
}

func (p *Postgres) SetWarnLevels(ctx context.Context, scope, name string, levels []int, by string) error {
	if levels == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM llmflow_warn_levels WHERE scope = $1 AND name = $2`, scope, name)
		return err
	}
	if err := ValidWarnLevel(scope, name, levels); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_warn_levels (scope, name, warn_at, set_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (scope, name) DO UPDATE SET warn_at = EXCLUDED.warn_at, set_by = EXCLUDED.set_by, set_at = now()`,
		scope, name, toInt32(levels), by)
	return err
}

// toInt32 converts a list for an integer[] column: nil stays NULL, the empty
// list is '{}'.
func toInt32(a []int) []int32 {
	if a == nil {
		return nil
	}
	out := make([]int32, len(a))
	for i, v := range a {
		out[i] = int32(v)
	}
	return out
}

// fromInt32 is toInt32 reversed (NULL is nil, '{}' the empty list).
func fromInt32(a []int32) []int {
	if a == nil {
		return nil
	}
	out := make([]int, len(a))
	for i, v := range a {
		out[i] = int(v)
	}
	return out
}
