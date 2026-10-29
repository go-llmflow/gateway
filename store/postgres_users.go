package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const usersSchema = `
CREATE TABLE IF NOT EXISTS llmflow_users (
	name       text PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	disabled   boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS llmflow_user_tokens (
	id         text PRIMARY KEY,
	hash       bytea NOT NULL UNIQUE,
	username   text NOT NULL REFERENCES llmflow_users(name),
	created_at timestamptz NOT NULL DEFAULT now(),
	revoked    boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS llmflow_bindings (
	username text NOT NULL REFERENCES llmflow_users(name),
	role     text NOT NULL,
	org      text NOT NULL DEFAULT '',
	team     text NOT NULL DEFAULT '',
	PRIMARY KEY (username, role, org, team)
);
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS owner text NOT NULL DEFAULT '';
-- v0.18.2 (#104): who created a binding. Rows stored before it keep '' (unknown),
-- which is treated as legitimate.
ALTER TABLE llmflow_bindings ADD COLUMN IF NOT EXISTS provenance text NOT NULL DEFAULT '';
`

func (p *Postgres) CreateUser(ctx context.Context, name string) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_users (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET disabled = false WHERE llmflow_users.disabled`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

func (p *Postgres) GetUser(ctx context.Context, name string) (User, error) {
	var u User
	err := p.pool.QueryRow(ctx, `SELECT name, created_at, disabled FROM llmflow_users WHERE name = $1`, name).
		Scan(&u.Name, &u.CreatedAt, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (p *Postgres) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, created_at, disabled FROM llmflow_users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) {
		var u User
		err := r.Scan(&u.Name, &u.CreatedAt, &u.Disabled)
		return u, err
	})
}

func (p *Postgres) DisableUser(ctx context.Context, name string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := affected(tx.Exec(ctx, `UPDATE llmflow_users SET disabled = true WHERE name = $1`, name)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE llmflow_user_tokens SET revoked = true WHERE username = $1`, name)
		return err
	})
}

func (p *Postgres) CreateUserToken(ctx context.Context, t UserToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_user_tokens (id, hash, username, created_at)
		SELECT $1, $2, $3, $4 WHERE EXISTS (SELECT 1 FROM llmflow_users WHERE name = $3 AND NOT disabled)`,
		t.ID, t.Hash[:], t.User, t.CreatedAt)
	switch {
	case uniqueViolation(err):
		return ErrExists
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

func scanToken(r pgx.Row) (UserToken, error) {
	var t UserToken
	var h []byte
	if err := r.Scan(&t.ID, &h, &t.User, &t.CreatedAt, &t.Revoked); err != nil {
		return t, err
	}
	if len(h) != len(t.Hash) {
		return t, fmt.Errorf("token %s: stored hash has %d bytes", t.ID, len(h))
	}
	copy(t.Hash[:], h)
	return t, nil
}

func (p *Postgres) ListUserTokens(ctx context.Context, user string) ([]UserToken, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, hash, username, created_at, revoked FROM llmflow_user_tokens WHERE username = $1 ORDER BY id`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserToken, error) { return scanToken(r) })
}

func (p *Postgres) RevokeUserToken(ctx context.Context, user, id string) error {
	return affected(p.pool.Exec(ctx, `UPDATE llmflow_user_tokens SET revoked = true WHERE id = $1 AND username = $2`, id, user))
}

func (p *Postgres) UserByTokenHash(ctx context.Context, h [32]byte) (User, UserToken, error) {
	t, err := scanToken(p.pool.QueryRow(ctx, `SELECT id, hash, username, created_at, revoked FROM llmflow_user_tokens WHERE hash = $1`, h[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, UserToken{}, ErrNotFound
	}
	if err != nil {
		return User{}, UserToken{}, err
	}
	u, err := p.GetUser(ctx, t.User)
	return u, t, err
}

func (p *Postgres) Grant(ctx context.Context, b Binding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_bindings (username, role, org, team, provenance)
		SELECT $1, $2, $3, $4, $5
		WHERE EXISTS (SELECT 1 FROM llmflow_users WHERE name = $1)
		  AND ($3 = '' OR EXISTS (SELECT 1 FROM llmflow_orgs WHERE name = $3))
		  AND ($4 = '' OR EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $4))
		ON CONFLICT DO NOTHING`, b.User, string(b.Role), b.Org, b.Team, string(b.Provenance))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_bindings WHERE username = $1 AND role = $2 AND org = $3 AND team = $4)`,
			b.User, string(b.Role), b.Org, b.Team).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrExists
		}
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Ungrant(ctx context.Context, b Binding) error {
	return affected(p.pool.Exec(ctx, `DELETE FROM llmflow_bindings WHERE username = $1 AND role = $2 AND org = $3 AND team = $4`,
		b.User, string(b.Role), b.Org, b.Team))
}

func (p *Postgres) Bindings(ctx context.Context, user string) ([]Binding, error) {
	rows, err := p.pool.Query(ctx, `SELECT username, role, org, team, provenance FROM llmflow_bindings WHERE $1 = '' OR username = $1
		ORDER BY username, role, org, team`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Binding, error) {
		var b Binding
		var role, prov string
		err := r.Scan(&b.User, &role, &b.Org, &b.Team, &prov)
		b.Role, b.Provenance = Role(role), Provenance(prov)
		return b, err
	})
}
