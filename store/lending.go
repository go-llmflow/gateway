package store

import (
	"context"
	"errors"
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
	LatencyMS float64
	Borrowing bool
	Cost      float64
}

// LendingStore lists the requests behind the lending report (#97).
type LendingStore interface {
	// LendingRequests returns the successful (2xx) requests of the GPU-hour
	// unit that finished in [from, to), in time order; ErrTooMany when there
	// are more than max.
	LendingRequests(ctx context.Context, from, to time.Time, max int) ([]LendingRow, error)
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
		out = append(out, LendingRow{r.Org, r.Pool, r.Time, r.LatencyMS, r.Borrowing, r.Cost})
	}
	return out, nil
}

func (p *Postgres) LendingRequests(ctx context.Context, from, to time.Time, max int) ([]LendingRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT org, pool, ts, latency_ms, borrowing, cost FROM llmflow_usage
		WHERE ts >= $1 AND ts < $2 AND unit = '' AND status BETWEEN 200 AND 299
		ORDER BY ts LIMIT $3`, from, to, max+1)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LendingRow, error) {
		var l LendingRow
		err := r.Scan(&l.Org, &l.Pool, &l.End, &l.LatencyMS, &l.Borrowing, &l.Cost)
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

var (
	_ LendingStore = (*Memory)(nil)
	_ LendingStore = (*Postgres)(nil)
)
