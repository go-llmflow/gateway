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

// schema is idempotent; the ALTER TABLE statements upgrade a v0.1.0
// database in place.
const schema = `
CREATE TABLE IF NOT EXISTS llmflow_orgs (
	name       text PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	disabled   boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS llmflow_teams (
	name       text PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	disabled   boolean NOT NULL DEFAULT false
);
ALTER TABLE llmflow_teams ADD COLUMN IF NOT EXISTS org text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS llmflow_projects (
	team       text NOT NULL REFERENCES llmflow_teams(name),
	name       text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	disabled   boolean NOT NULL DEFAULT false,
	PRIMARY KEY (team, name)
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
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS project text NOT NULL DEFAULT '';
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
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS org text NOT NULL DEFAULT '';
ALTER TABLE llmflow_usage ADD COLUMN IF NOT EXISTS project text NOT NULL DEFAULT '';
`

// OpenPostgres connects and creates or upgrades the schema. The error
// never contains the DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("postgres: the DSN does not parse")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	for _, s := range []string{schema, usersSchema, modelsSchema, budgetsSchema, allowSchema} {
		if _, err := pool.Exec(ctx, s); err != nil {
			pool.Close()
			return nil, fmt.Errorf("postgres: schema: %w", err)
		}
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

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) CreateOrg(ctx context.Context, name string) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_orgs (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET disabled = false WHERE llmflow_orgs.disabled`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

func (p *Postgres) ListOrgs(ctx context.Context) ([]Org, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, created_at, disabled FROM llmflow_orgs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Org, error) {
		var o Org
		err := r.Scan(&o.Name, &o.CreatedAt, &o.Disabled)
		return o, err
	})
}

func (p *Postgres) DisableOrg(ctx context.Context, name string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := affected(tx.Exec(ctx, `UPDATE llmflow_orgs SET disabled = true WHERE name = $1`, name)); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE llmflow_projects SET disabled = true WHERE team IN (SELECT name FROM llmflow_teams WHERE org = $1)`,
			`UPDATE llmflow_keys SET revoked = true WHERE team IN (SELECT name FROM llmflow_teams WHERE org = $1)`,
			`UPDATE llmflow_teams SET disabled = true WHERE org = $1`,
		} {
			if _, err := tx.Exec(ctx, q, name); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) CreateTeam(ctx context.Context, name, org string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var disabled bool
		err := tx.QueryRow(ctx, `SELECT disabled FROM llmflow_orgs WHERE name = $1 FOR SHARE`, org).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && disabled {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var curOrg string
		err = tx.QueryRow(ctx, `SELECT org, disabled FROM llmflow_teams WHERE name = $1 FOR UPDATE`, name).Scan(&curOrg, &disabled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO llmflow_teams (name, org) VALUES ($1, $2)`, name, org)
			if uniqueViolation(err) {
				return ErrExists
			}
			return err
		case err != nil:
			return err
		case curOrg == "":
			_, err = tx.Exec(ctx, `UPDATE llmflow_teams SET org = $2, disabled = false WHERE name = $1`, name, org)
			return err
		case curOrg != org:
			return ErrConflict
		case disabled:
			_, err = tx.Exec(ctx, `UPDATE llmflow_teams SET disabled = false WHERE name = $1`, name)
			return err
		}
		return ErrExists
	})
}

func scanTeam(r pgx.Row) (Team, error) {
	var t Team
	err := r.Scan(&t.Name, &t.Org, &t.CreatedAt, &t.Disabled)
	return t, err
}

func (p *Postgres) GetTeam(ctx context.Context, name string) (Team, error) {
	t, err := scanTeam(p.pool.QueryRow(ctx, `SELECT name, org, created_at, disabled FROM llmflow_teams WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrNotFound
	}
	return t, err
}

func (p *Postgres) ListTeams(ctx context.Context, org string) ([]Team, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, org, created_at, disabled FROM llmflow_teams WHERE $1 = '' OR org = $1 ORDER BY name`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) { return scanTeam(r) })
}

func (p *Postgres) DisableTeam(ctx context.Context, name string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := affected(tx.Exec(ctx, `UPDATE llmflow_teams SET disabled = true WHERE name = $1`, name)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE llmflow_projects SET disabled = true WHERE team = $1`, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE llmflow_keys SET revoked = true WHERE team = $1`, name)
		return err
	})
}

func (p *Postgres) CreateProject(ctx context.Context, team, name string) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_projects (team, name)
		SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $1 AND NOT disabled)
		ON CONFLICT (team, name) DO UPDATE SET disabled = false WHERE llmflow_projects.disabled`, team, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_projects WHERE team = $1 AND name = $2)`, team, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrExists
		}
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ListProjects(ctx context.Context, team string) ([]Project, error) {
	rows, err := p.pool.Query(ctx, `SELECT team, name, created_at, disabled FROM llmflow_projects WHERE team = $1 ORDER BY name`, team)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		var pr Project
		err := r.Scan(&pr.Team, &pr.Name, &pr.CreatedAt, &pr.Disabled)
		return pr, err
	})
}

func (p *Postgres) DisableProject(ctx context.Context, team, name string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := affected(tx.Exec(ctx, `UPDATE llmflow_projects SET disabled = true WHERE team = $1 AND name = $2`, team, name)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE llmflow_keys SET revoked = true WHERE team = $1 AND project = $2`, team, name)
		return err
	})
}

func (p *Postgres) CreateKey(ctx context.Context, k Key) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_keys (id, hash, team, project, name, rpm, tpm, max_inflight, created_at, owner, tags)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		WHERE EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $3 AND NOT disabled)
		  AND ($4 = '' OR EXISTS (SELECT 1 FROM llmflow_projects WHERE team = $3 AND name = $4 AND NOT disabled))`,
		k.ID, k.Hash[:], k.Team, k.Project, k.Name, k.RPM, k.TPM, k.MaxInFlight, k.CreatedAt, k.Owner, dedupSorted(k.Tags))
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

const keyCols = `k.id, k.hash, coalesce(t.org, ''), k.team, k.project, k.owner, k.tags, k.name, k.rpm, k.tpm, k.max_inflight, k.created_at, k.revoked`
const keyFrom = ` FROM llmflow_keys k LEFT JOIN llmflow_teams t ON t.name = k.team `

func scanKey(r pgx.Row) (Key, error) {
	var k Key
	var h []byte
	err := r.Scan(&k.ID, &h, &k.Org, &k.Team, &k.Project, &k.Owner, &k.Tags, &k.Name, &k.RPM, &k.TPM, &k.MaxInFlight, &k.CreatedAt, &k.Revoked)
	if err != nil {
		return k, err
	}
	if len(k.Tags) == 0 {
		k.Tags = nil
	}
	if len(h) != len(k.Hash) {
		return k, fmt.Errorf("key %s: stored hash has %d bytes", k.ID, len(h))
	}
	copy(k.Hash[:], h)
	return k, nil
}

func (p *Postgres) ListKeys(ctx context.Context, team string) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+keyCols+keyFrom+`WHERE $1 = '' OR k.team = $1 ORDER BY k.id`, team)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (p *Postgres) GetKey(ctx context.Context, id string) (Key, error) {
	k, err := scanKey(p.pool.QueryRow(ctx, `SELECT `+keyCols+keyFrom+`WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

func (p *Postgres) RevokeKey(ctx context.Context, id string) error {
	return affected(p.pool.Exec(ctx, `UPDATE llmflow_keys SET revoked = true WHERE id = $1`, id))
}

func (p *Postgres) KeyByHash(ctx context.Context, h [32]byte) (Key, error) {
	k, err := scanKey(p.pool.QueryRow(ctx, `SELECT `+keyCols+keyFrom+`WHERE k.hash = $1`, h[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

var usageCols = []string{"ts", "org", "team", "project", "key_id", "model", "pool", "upstream", "prompt_tokens",
	"completion_tokens", "cached_tokens", "cost", "latency_ms", "status", "priority", "borrowing", "stream", "estimated", "tags"}

// InsertRecords writes a batch with COPY.
func (p *Postgres) InsertRecords(ctx context.Context, recs []Record) error {
	rows := make([][]any, len(recs))
	for i, r := range recs {
		tags := r.Tags
		if tags == nil {
			tags = []string{}
		}
		rows[i] = []any{r.Time, r.Org, r.Team, r.Project, r.KeyID, r.Model, r.Pool, r.Upstream, r.PromptTokens,
			r.CompletionTokens, r.CachedTokens, r.Cost, r.LatencyMS, r.Status, r.Priority, r.Borrowing, r.Stream, r.Estimated, tags}
	}
	_, err := p.pool.CopyFrom(ctx, pgx.Identifier{"llmflow_usage"}, usageCols, pgx.CopyFromRows(rows))
	return err
}

func (p *Postgres) Close() { p.pool.Close() }
