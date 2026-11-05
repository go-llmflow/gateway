package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Billing state: closed months (an immutable snapshot of what was billed),
// and the network and storage the gateway meters per organisation.

// ClosedMonth is the snapshot of a closed month. Payload is opaque to the
// store (the gateway's JSON: lines and the parameters used); Checksum its
// SHA-256, entered in the audit log. A closed month is never updated or
// deleted: in PostgreSQL a trigger refuses both.
type ClosedMonth struct {
	Month    string // YYYY-MM
	ClosedAt time.Time
	ClosedBy string
	Checksum string
	Payload  []byte
}

// BillingStore keeps closed months. Memory and Postgres implement it.
type BillingStore interface {
	// CloseMonth stores the snapshot; ErrConflict when the month is closed.
	CloseMonth(ctx context.Context, c ClosedMonth) error
	// ClosedMonth returns a closed month (ok false when it is open).
	ClosedMonth(ctx context.Context, month string) (c ClosedMonth, ok bool, err error)
	// ClosedMonths lists the closed months, ascending.
	ClosedMonths(ctx context.Context) ([]string, error)
}

// NetworkCell is the network a consumer used on a UTC day, in bytes: the
// request bodies forwarded (In) and the answers written to clients (Out).
type NetworkCell struct {
	Day                                        string
	Org, Lab, Team, Project, User, Model, Pool string
	In, Out                                    int64
}

// StorageCell is what the gateway stores for an organisation at the end of a
// UTC day, in bytes: its usage records in PostgreSQL and the request-log
// objects it wrote to the export.
type StorageCell struct {
	Day   string
	Org   string
	Bytes int64
}

// MeterStore keeps what is metered per organisation besides tokens.
type MeterStore interface {
	// NetworkBetween sums the bytes of the requests that finished in [from, to).
	NetworkBetween(ctx context.Context, from, to time.Time) ([]NetworkCell, error)
	// StorageBetween returns, for every UTC day of [from, to) and every
	// organisation that stores anything by the end of it, the bytes stored.
	StorageBetween(ctx context.Context, from, to time.Time) ([]StorageCell, error)
	// AddStorage adds bytes written to the export (request-log objects) for an
	// organisation on a UTC day.
	AddStorage(ctx context.Context, day, org string, bytes int64) error
}

// RecordSize is the size the memory store charges a usage record for
// storage: its JSON. PostgreSQL measures the row itself (pg_column_size).
func RecordSize(r Record) int64 {
	b, _ := json.Marshal(r)
	return int64(len(b))
}

// cumulate turns per-day increments into end-of-day totals for every day of
// [from, to): incs are the bytes added per (day, org), all days before to.
func cumulate(incs []StorageCell, from, to time.Time) []StorageCell {
	byOrg := map[string]map[string]int64{}
	for _, c := range incs {
		if byOrg[c.Org] == nil {
			byOrg[c.Org] = map[string]int64{}
		}
		byOrg[c.Org][c.Day] += c.Bytes
	}
	orgs := make([]string, 0, len(byOrg))
	for o := range byOrg {
		orgs = append(orgs, o)
	}
	sort.Strings(orgs)
	first := from.UTC().Truncate(24 * time.Hour)
	var out []StorageCell
	for _, o := range orgs {
		var run int64
		days := make([]string, 0, len(byOrg[o]))
		for d := range byOrg[o] {
			days = append(days, d)
		}
		sort.Strings(days)
		i := 0
		for d := first; d.Before(to); d = d.AddDate(0, 0, 1) {
			ds := d.Format("2006-01-02")
			for i < len(days) && days[i] <= ds {
				run += byOrg[o][days[i]]
				i++
			}
			// Days before the first day of the window carry into it.
			if run > 0 {
				out = append(out, StorageCell{ds, o, run})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Org < out[j].Org
	})
	return out
}

type memBilling struct {
	closed  map[string]ClosedMonth
	storage []StorageCell // request-log objects, per (day, org)
}

func (m *Memory) CloseMonth(ctx context.Context, c ClosedMonth) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.billing.closed[c.Month]; ok {
		return ErrConflict
	}
	if m.billing.closed == nil {
		m.billing.closed = map[string]ClosedMonth{}
	}
	c.Payload = append([]byte(nil), c.Payload...)
	m.billing.closed[c.Month] = c
	return nil
}

func (m *Memory) ClosedMonth(ctx context.Context, month string) (ClosedMonth, bool, error) {
	if err := ctx.Err(); err != nil {
		return ClosedMonth{}, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return ClosedMonth{}, false, m.Fail
	}
	c, ok := m.billing.closed[month]
	return c, ok, nil
}

func (m *Memory) ClosedMonths(ctx context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]string, 0, len(m.billing.closed))
	for k := range m.billing.closed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) NetworkBetween(ctx context.Context, from, to time.Time) ([]NetworkCell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	type k struct{ day, org, lab, team, project, user, model, pool string }
	agg := map[k]*NetworkCell{}
	for _, r := range m.records {
		if r.Time.Before(from) || !r.Time.Before(to) || r.BytesIn == 0 && r.BytesOut == 0 {
			continue
		}
		key := k{day(r.Time), r.Org, r.Lab, r.Team, r.Project, r.User, r.Model, r.Pool}
		a := agg[key]
		if a == nil {
			a = &NetworkCell{Day: key.day, Org: r.Org, Lab: r.Lab, Team: r.Team, Project: r.Project, User: r.User, Model: r.Model, Pool: r.Pool}
			agg[key] = a
		}
		a.In += r.BytesIn
		a.Out += r.BytesOut
	}
	out := make([]NetworkCell, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sortNetwork(out)
	return out, nil
}

func sortNetwork(out []NetworkCell) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		return a.Org+"\x00"+a.Lab+"\x00"+a.Team+"\x00"+a.Project+"\x00"+a.User+"\x00"+a.Model+"\x00"+a.Pool < b.Org+"\x00"+b.Lab+"\x00"+b.Team+"\x00"+b.Project+"\x00"+b.User+"\x00"+b.Model+"\x00"+b.Pool
	})
}

func (m *Memory) StorageBetween(ctx context.Context, from, to time.Time) ([]StorageCell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var incs []StorageCell
	for _, r := range m.records {
		if r.Org != "" && r.Time.Before(to) {
			incs = append(incs, StorageCell{day(r.Time), r.Org, RecordSize(r)})
		}
	}
	incs = append(incs, m.billing.storage...) // days after the window are never reached by cumulate
	return cumulate(incs, from, to), nil
}

func (m *Memory) AddStorage(ctx context.Context, day, org string, bytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.billing.storage = append(m.billing.storage, StorageCell{day, org, bytes})
	return nil
}

const billingSchema = `
CREATE TABLE IF NOT EXISTS llmflow_billing_closed (
	month     text PRIMARY KEY,
	closed_at timestamptz NOT NULL,
	closed_by text NOT NULL,
	checksum  text NOT NULL,
	payload   bytea NOT NULL
);
CREATE OR REPLACE FUNCTION llmflow_billing_closed_immutable() RETURNS trigger AS $$
BEGIN
	RAISE EXCEPTION 'a closed billing month is immutable';
END
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS llmflow_billing_closed_immutable ON llmflow_billing_closed;
CREATE TRIGGER llmflow_billing_closed_immutable BEFORE UPDATE OR DELETE ON llmflow_billing_closed
	FOR EACH ROW EXECUTE FUNCTION llmflow_billing_closed_immutable();
CREATE TABLE IF NOT EXISTS llmflow_storage_log (
	day   text NOT NULL,
	org   text NOT NULL,
	bytes bigint NOT NULL,
	PRIMARY KEY (day, org)
);
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS bytes_in bigint NOT NULL DEFAULT 0;
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS bytes_out bigint NOT NULL DEFAULT 0;
`

func (p *Postgres) CloseMonth(ctx context.Context, c ClosedMonth) error {
	tag, err := p.pool.Exec(ctx, `INSERT INTO llmflow_billing_closed (month, closed_at, closed_by, checksum, payload) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (month) DO NOTHING`,
		c.Month, c.ClosedAt.UTC(), c.ClosedBy, c.Checksum, c.Payload)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (p *Postgres) ClosedMonth(ctx context.Context, month string) (ClosedMonth, bool, error) {
	var c ClosedMonth
	err := p.pool.QueryRow(ctx, `SELECT month, closed_at, closed_by, checksum, payload FROM llmflow_billing_closed WHERE month = $1`, month).
		Scan(&c.Month, &c.ClosedAt, &c.ClosedBy, &c.Checksum, &c.Payload)
	if err == pgx.ErrNoRows {
		return ClosedMonth{}, false, nil
	}
	return c, err == nil, err
}

func (p *Postgres) ClosedMonths(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT month FROM llmflow_billing_closed ORDER BY month`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) NetworkBetween(ctx context.Context, from, to time.Time) ([]NetworkCell, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD'), org, lab, team, project, user_name, model, pool, sum(bytes_in)::bigint, sum(bytes_out)::bigint
		FROM llmflow_usage WHERE ts >= $1 AND ts < $2 AND (bytes_in <> 0 OR bytes_out <> 0)
		GROUP BY 1, org, lab, team, project, user_name, model, pool`, from, to)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NetworkCell, error) {
		var c NetworkCell
		err := r.Scan(&c.Day, &c.Org, &c.Lab, &c.Team, &c.Project, &c.User, &c.Model, &c.Pool, &c.In, &c.Out)
		return c, err
	})
	sortNetwork(out)
	return out, err
}

func (p *Postgres) StorageBetween(ctx context.Context, from, to time.Time) ([]StorageCell, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD'), org, sum(pg_column_size(u.*))::bigint
		FROM llmflow_usage u WHERE ts < $1 AND org <> '' GROUP BY 1, org`, to)
	if err != nil {
		return nil, err
	}
	incs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StorageCell, error) {
		var c StorageCell
		err := r.Scan(&c.Day, &c.Org, &c.Bytes)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = p.pool.Query(ctx, `SELECT day, org, bytes FROM llmflow_storage_log WHERE day <= $1`, to.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	logs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StorageCell, error) {
		var c StorageCell
		err := r.Scan(&c.Day, &c.Org, &c.Bytes)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	return cumulate(append(incs, logs...), from, to), nil
}

func (p *Postgres) AddStorage(ctx context.Context, day, org string, bytes int64) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_storage_log (day, org, bytes) VALUES ($1,$2,$3)
		ON CONFLICT (day, org) DO UPDATE SET bytes = llmflow_storage_log.bytes + EXCLUDED.bytes`, day, org, bytes)
	return err
}

var (
	_ BillingStore = (*Memory)(nil)
	_ BillingStore = (*Postgres)(nil)
	_ MeterStore   = (*Memory)(nil)
	_ MeterStore   = (*Postgres)(nil)
)
