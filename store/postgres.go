package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a Store on PostgreSQL through pgx.
type Postgres struct {
	pool *pgxpool.Pool
}

const schema = `
CREATE TABLE IF NOT EXISTS llmflow_teams (
	name       text PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	disabled   boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS llmflow_keys (
	id           text PRIMARY KEY,
	hash         bytea NOT NULL UNIQUE,
	team         text NOT NULL REFERENCES llmflow_teams(name),
	name         text NOT NULL DEFAULT '',
	rpm          integer NOT NULL DEFAULT 0,
	tpm          integer NOT NULL DEFAULT 0,
	max_inflight integer NOT NULL DEFAULT 0,
	created_at   timestamptz NOT NULL DEFAULT now(),
	revoked      boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS llmflow_budgets (
	team     text PRIMARY KEY REFERENCES llmflow_teams(name),
	period   text NOT NULL,
	max_cost double precision NOT NULL
);
CREATE TABLE IF NOT EXISTS llmflow_usage (
	id                bigserial PRIMARY KEY,
	ts                timestamptz NOT NULL,
	team              text NOT NULL,
	key_id            text NOT NULL,
	model             text NOT NULL,
	pool              text NOT NULL,
	upstream          text NOT NULL,
	prompt_tokens     bigint NOT NULL,
	completion_tokens bigint NOT NULL,
	cached_tokens     bigint NOT NULL,
	cost              double precision NOT NULL,
	latency_ms        double precision NOT NULL,
	status            integer NOT NULL,
	priority          integer NOT NULL,
	borrowing         boolean NOT NULL,
	stream            boolean NOT NULL
);
CREATE INDEX IF NOT EXISTS llmflow_usage_ts_team ON llmflow_usage (ts, team);
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS estimated boolean NOT NULL DEFAULT false;
`

// OpenPostgres connects and creates the schema if needed. The error never
// contains the DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("postgres: the DSN does not parse")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func uniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func fkViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) CreateTeam(ctx context.Context, name string) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_teams (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET disabled = false WHERE llmflow_teams.disabled`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

func (p *Postgres) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, created_at, disabled FROM llmflow_teams ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) {
		var t Team
		err := r.Scan(&t.Name, &t.CreatedAt, &t.Disabled)
		return t, err
	})
}

func (p *Postgres) DisableTeam(ctx context.Context, name string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	tag, err := tx.Exec(ctx, `UPDATE llmflow_teams SET disabled = true WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE llmflow_keys SET revoked = true WHERE team = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) CreateKey(ctx context.Context, k Key) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_keys (id, hash, team, name, rpm, tpm, max_inflight, created_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8
		WHERE EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $3 AND NOT disabled)`,
		k.ID, k.Hash[:], k.Team, k.Name, k.RPM, k.TPM, k.MaxInFlight, k.CreatedAt)
	switch {
	case uniqueViolation(err):
		return ErrExists
	case fkViolation(err):
		return ErrNotFound
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

const keyCols = `id, hash, team, name, rpm, tpm, max_inflight, created_at, revoked`

func scanKey(r pgx.Row) (Key, error) {
	var k Key
	var h []byte
	err := r.Scan(&k.ID, &h, &k.Team, &k.Name, &k.RPM, &k.TPM, &k.MaxInFlight, &k.CreatedAt, &k.Revoked)
	if err != nil {
		return k, err
	}
	if len(h) != len(k.Hash) {
		return k, fmt.Errorf("key %s: stored hash has %d bytes", k.ID, len(h))
	}
	copy(k.Hash[:], h)
	return k, nil
}

func (p *Postgres) ListKeys(ctx context.Context, team string) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+keyCols+` FROM llmflow_keys WHERE $1 = '' OR team = $1 ORDER BY id`, team)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (p *Postgres) RevokeKey(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE llmflow_keys SET revoked = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) KeyByHash(ctx context.Context, h [32]byte) (Key, error) {
	k, err := scanKey(p.pool.QueryRow(ctx, `SELECT `+keyCols+` FROM llmflow_keys WHERE hash = $1`, h[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

func (p *Postgres) SetBudget(ctx context.Context, b Budget) error {
	if err := ValidateBudget(b); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_budgets (team, period, max_cost) VALUES ($1, $2, $3)
		ON CONFLICT (team) DO UPDATE SET period = EXCLUDED.period, max_cost = EXCLUDED.max_cost`,
		b.Team, string(b.Period), b.MaxCost)
	if fkViolation(err) {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) DeleteBudget(ctx context.Context, team string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM llmflow_budgets WHERE team = $1`, team)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Budgets(ctx context.Context) ([]Budget, error) {
	rows, err := p.pool.Query(ctx, `SELECT team, period, max_cost FROM llmflow_budgets ORDER BY team`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Budget, error) {
		var b Budget
		var period string
		err := r.Scan(&b.Team, &period, &b.MaxCost)
		b.Period = Period(period)
		return b, err
	})
}

func (p *Postgres) SpendSince(ctx context.Context, since time.Time) (map[string]float64, error) {
	rows, err := p.pool.Query(ctx, `SELECT team, sum(cost) FROM llmflow_usage WHERE ts >= $1 GROUP BY team`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var team string
		var cost float64
		if err := rows.Scan(&team, &cost); err != nil {
			return nil, err
		}
		out[team] = cost
	}
	return out, rows.Err()
}

var usageCols = []string{"ts", "team", "key_id", "model", "pool", "upstream", "prompt_tokens",
	"completion_tokens", "cached_tokens", "cost", "latency_ms", "status", "priority", "borrowing", "stream", "estimated"}

// InsertRecords writes a batch with COPY.
func (p *Postgres) InsertRecords(ctx context.Context, recs []Record) error {
	rows := make([][]any, len(recs))
	for i, r := range recs {
		rows[i] = []any{r.Time, r.Team, r.KeyID, r.Model, r.Pool, r.Upstream, r.PromptTokens,
			r.CompletionTokens, r.CachedTokens, r.Cost, r.LatencyMS, r.Status, r.Priority, r.Borrowing, r.Stream, r.Estimated}
	}
	_, err := p.pool.CopyFrom(ctx, pgx.Identifier{"llmflow_usage"}, usageCols, pgx.CopyFromRows(rows))
	return err
}

func (p *Postgres) Usage(ctx context.Context, from, to time.Time) ([]TeamUsage, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT team, pool, count(*), sum(prompt_tokens), sum(completion_tokens), sum(cached_tokens), sum(cost),
		       count(*) FILTER (WHERE estimated)
		FROM llmflow_usage WHERE ts >= $1 AND ts < $2
		GROUP BY team, pool ORDER BY team, pool`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TeamUsage, error) {
		var u TeamUsage
		err := r.Scan(&u.Team, &u.Pool, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &u.CachedTokens, &u.GPUHours, &u.EstimatedRequests)
		return u, err
	})
}

func (p *Postgres) Close() { p.pool.Close() }
