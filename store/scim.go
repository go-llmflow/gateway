package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// SCIM provisioning state: the Users and Groups an identity provider
// pushed, kept as JSON documents (the gateway's own users and role
// bindings are derived from them). A User's name (its userName) is unique
// without regard to case (RFC 7643 §4.1.1); a Group's name (displayName)
// is only indexed.

// SCIMDoc is one provisioned resource.
type SCIMDoc struct {
	Kind    string // "User" or "Group"
	ID      string
	Name    string
	Data    []byte // the resource as JSON
	Version string
}

const scimSchema = `
CREATE TABLE IF NOT EXISTS llmflow_scim (
	kind    text NOT NULL,
	id      text NOT NULL,
	name    text NOT NULL,
	data    jsonb NOT NULL,
	version text NOT NULL,
	PRIMARY KEY (kind, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS llmflow_scim_username ON llmflow_scim (lower(name)) WHERE kind = 'User';
CREATE INDEX IF NOT EXISTS llmflow_scim_name ON llmflow_scim (kind, lower(name));
`

func (p *Postgres) SCIMCreate(ctx context.Context, d SCIMDoc) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_scim (kind, id, name, data, version) VALUES ($1, $2, $3, $4, $5)`,
		d.Kind, d.ID, d.Name, string(d.Data), d.Version)
	if uniqueViolation(err) {
		return ErrExists
	}
	return err
}

func scanDoc(r pgx.Row) (SCIMDoc, error) {
	var d SCIMDoc
	err := r.Scan(&d.Kind, &d.ID, &d.Name, &d.Data, &d.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (p *Postgres) SCIMGet(ctx context.Context, kind, id string) (SCIMDoc, error) {
	return scanDoc(p.pool.QueryRow(ctx, `SELECT kind, id, name, data, version FROM llmflow_scim WHERE kind = $1 AND id = $2`, kind, id))
}

func (p *Postgres) SCIMReplace(ctx context.Context, d SCIMDoc, ifVersion string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE llmflow_scim SET name = $3, data = $4, version = $5 WHERE kind = $1 AND id = $2 AND version = $6`,
		d.Kind, d.ID, d.Name, string(d.Data), d.Version, ifVersion)
	switch {
	case uniqueViolation(err):
		return ErrExists
	case err != nil:
		return err
	case tag.RowsAffected() == 1:
		return nil
	}
	if _, err := p.SCIMGet(ctx, d.Kind, d.ID); err != nil {
		return err
	}
	return ErrConflict
}

func (p *Postgres) SCIMDelete(ctx context.Context, kind, id string) error {
	return affected(p.pool.Exec(ctx, `DELETE FROM llmflow_scim WHERE kind = $1 AND id = $2`, kind, id))
}

func (p *Postgres) SCIMList(ctx context.Context, kind, name string) ([]SCIMDoc, error) {
	rows, err := p.pool.Query(ctx, `SELECT kind, id, name, data, version FROM llmflow_scim
		WHERE kind = $1 AND ($2 = '' OR lower(name) = lower($2)) ORDER BY id`, kind, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SCIMDoc, error) { return scanDoc(r) })
}

type memSCIM map[[2]string]SCIMDoc

func (m *Memory) scim() memSCIM {
	if m.scimDocs == nil {
		m.scimDocs = memSCIM{}
	}
	return m.scimDocs
}

func (m *Memory) scimNameTaken(d SCIMDoc) bool {
	if d.Kind != "User" {
		return false
	}
	for k, o := range m.scim() {
		if k[0] == "User" && k[1] != d.ID && strings.EqualFold(o.Name, d.Name) {
			return true
		}
	}
	return false
}

func (m *Memory) SCIMCreate(_ context.Context, d SCIMDoc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.scim()[[2]string{d.Kind, d.ID}]; ok || m.scimNameTaken(d) {
		return ErrExists
	}
	d.Data = append([]byte(nil), d.Data...)
	m.scim()[[2]string{d.Kind, d.ID}] = d
	return nil
}

func (m *Memory) SCIMGet(_ context.Context, kind, id string) (SCIMDoc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return SCIMDoc{}, m.Fail
	}
	d, ok := m.scimDocs[[2]string{kind, id}]
	if !ok {
		return SCIMDoc{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) SCIMReplace(_ context.Context, d SCIMDoc, ifVersion string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	cur, ok := m.scim()[[2]string{d.Kind, d.ID}]
	switch {
	case !ok:
		return ErrNotFound
	case cur.Version != ifVersion:
		return ErrConflict
	case m.scimNameTaken(d):
		return ErrExists
	}
	d.Data = append([]byte(nil), d.Data...)
	m.scim()[[2]string{d.Kind, d.ID}] = d
	return nil
}

func (m *Memory) SCIMDelete(_ context.Context, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.scim()[[2]string{kind, id}]; !ok {
		return ErrNotFound
	}
	delete(m.scim(), [2]string{kind, id})
	return nil
}

func (m *Memory) SCIMList(_ context.Context, kind, name string) ([]SCIMDoc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []SCIMDoc{}
	for k, d := range m.scimDocs {
		if k[0] == kind && (name == "" || strings.EqualFold(d.Name, name)) {
			out = append(out, d)
		}
	}
	return out, nil
}
