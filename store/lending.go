package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrTooMany is returned when a period holds more requests than a report may
// read into memory.
var ErrTooMany = errors.New("store: too many records in the period")

// LendingRow is the part of a finished GPU-hour request the lending report
// needs.
type LendingRow struct {
	Org, Pool string
	End       time.Time
	// LatencyMS is arrival to end; WaitMS the part of it spent waiting for
	// the fair-share grant. The request holds its slot for the difference.
	LatencyMS, WaitMS float64
	Borrowing         bool
	Cost              float64
}

// LendingStore lists the requests behind the lending report (#97).
type LendingStore interface {
	// LendingRequests returns the successful (2xx) requests of the GPU-hour
	// unit that finished in [from, to), in time order; ErrTooMany when there
	// are more than max.
	LendingRequests(ctx context.Context, from, to time.Time, max int) ([]LendingRow, error)
}

// CapacityPoint is a pool's C from At on: its configured capacity plus that
// of its discovered Slurm instances, as the gateway applied it.
type CapacityPoint struct {
	Pool string
	At   time.Time
	C    int
}

// CapacityStore keeps the history of every pool's C, so that a period with a
// changing C is integrated as it was (#97, #108).
type CapacityStore interface {
	// RecordCapacity stores a point; a point for the same pool and instant is
	// kept once, the last value winning (replicas record the same change).
	RecordCapacity(ctx context.Context, p CapacityPoint) error
	// CapacityBetween returns the points in [from, to) and, for each pool,
	// the last point before from, sorted by pool then time.
	CapacityBetween(ctx context.Context, from, to time.Time) ([]CapacityPoint, error)
}

func (m *Memory) LendingRequests(ctx context.Context, from, to time.Time, max int) ([]LendingRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []LendingRow
	for _, r := range m.records {
		if r.Unit != "" || r.Status < 200 || r.Status > 299 || r.Time.Before(from) || !r.Time.Before(to) {
			continue
		}
		if len(out) == max {
			return nil, ErrTooMany
		}
		out = append(out, LendingRow{r.Org, r.Pool, r.Time, r.LatencyMS, r.AdmissionWaitMS, r.Borrowing, r.Cost})
	}
	return out, nil
}

const lendingSchema = `
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS wait_ms double precision NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS llmflow_pool_capacity (
	pool     text NOT NULL,
	ts       timestamptz NOT NULL,
	capacity integer NOT NULL,
	PRIMARY KEY (pool, ts)
);
`

func (p *Postgres) LendingRequests(ctx context.Context, from, to time.Time, max int) ([]LendingRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT org, pool, ts, latency_ms, wait_ms, borrowing, cost FROM llmflow_usage
		WHERE ts >= $1 AND ts < $2 AND unit = '' AND status BETWEEN 200 AND 299
		ORDER BY ts LIMIT $3`, from, to, max+1)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LendingRow, error) {
		var l LendingRow
		err := r.Scan(&l.Org, &l.Pool, &l.End, &l.LatencyMS, &l.WaitMS, &l.Borrowing, &l.Cost)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, ErrTooMany
	}
	return out, nil
}

func (m *Memory) RecordCapacity(ctx context.Context, p CapacityPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for i, q := range m.en.caps {
		if q.Pool == p.Pool && q.At.Equal(p.At) {
			m.en.caps[i].C = p.C // the last value at an instant is the C then
			return nil
		}
	}
	m.en.caps = append(m.en.caps, p)
	return nil
}

func (m *Memory) CapacityBetween(ctx context.Context, from, to time.Time) ([]CapacityPoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	before := map[string]CapacityPoint{}
	var out []CapacityPoint
	for _, c := range m.en.caps {
		switch {
		case c.At.Before(from):
			if b, ok := before[c.Pool]; !ok || c.At.After(b.At) {
				before[c.Pool] = c
			}
		case c.At.Before(to):
			out = append(out, c)
		}
	}
	for _, b := range before {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return out[i].Pool < out[j].Pool
		}
		return out[i].At.Before(out[j].At)
	})
	return out, nil
}

func (p *Postgres) RecordCapacity(ctx context.Context, c CapacityPoint) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_pool_capacity (pool, ts, capacity) VALUES ($1, $2, $3) ON CONFLICT (pool, ts) DO UPDATE SET capacity = EXCLUDED.capacity`, c.Pool, c.At.UTC(), c.C)
	return err
}

func (p *Postgres) CapacityBetween(ctx context.Context, from, to time.Time) ([]CapacityPoint, error) {
	rows, err := p.pool.Query(ctx, `
		(SELECT pool, ts, capacity FROM llmflow_pool_capacity WHERE ts >= $1 AND ts < $2)
		UNION ALL
		(SELECT DISTINCT ON (pool) pool, ts, capacity FROM llmflow_pool_capacity WHERE ts < $1 ORDER BY pool, ts DESC)
		`, from, to)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CapacityPoint, error) {
		var c CapacityPoint
		err := r.Scan(&c.Pool, &c.At, &c.C)
		return c, err
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return out[i].Pool < out[j].Pool
		}
		return out[i].At.Before(out[j].At)
	})
	return out, err
}

var (
	_ LendingStore  = (*Memory)(nil)
	_ LendingStore  = (*Postgres)(nil)
	_ CapacityStore = (*Memory)(nil)
	_ CapacityStore = (*Postgres)(nil)
)
