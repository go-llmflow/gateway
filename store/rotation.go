package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Key rotation: a key keeps its ID (budgets, limits, allowlists and
// reports continue) and gets a new secret; the previous secret stays valid
// until a grace deadline.
const rotationSchema = `
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS prev_hash bytea UNIQUE;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS prev_expires timestamptz;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS rotated_at timestamptz;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS rotate_every bigint NOT NULL DEFAULT 0;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS rotate_grace bigint NOT NULL DEFAULT 0;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS rotate_target text NOT NULL DEFAULT '';
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS policy text NOT NULL DEFAULT '';
`

// Rotation is a scheduled rotation: every Every, the key gets a new secret,
// written to Target (a vault: secret reference); the replaced secret stays
// valid for Grace.
type Rotation struct {
	Every  Duration `json:"every,omitzero"`
	Grace  Duration `json:"grace_period,omitzero"`
	Target string   `json:"target,omitempty"`
}

// Duration is a time.Duration written as a Go duration string in JSON.
type Duration time.Duration

// MarshalJSON writes "720h0m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

// UnmarshalJSON reads a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("duration: want a string such as \"720h\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (p *Postgres) RotateKey(ctx context.Context, id string, newHash, expect [32]byte, prevExpires, at time.Time) error {
	var exp []byte
	if expect != ([32]byte{}) {
		exp = expect[:]
	}
	// The unique constraints do not see across the two columns, so the
	// statement checks that the new hash is no other key's secret (it may
	// be this key's previous one: undoing a rotation).
	tag, err := p.pool.Exec(ctx, `
		UPDATE llmflow_keys SET
			prev_hash    = CASE WHEN $4::timestamptz > $5::timestamptz THEN hash END,
			prev_expires = CASE WHEN $4::timestamptz > $5::timestamptz THEN $4::timestamptz END,
			hash = $2, rotated_at = $6
		WHERE id = $1 AND NOT revoked AND hash <> $2
		  AND ($3::bytea IS NULL OR hash = $3::bytea)
		  AND NOT EXISTS (SELECT 1 FROM llmflow_keys o WHERE o.id <> $1 AND (o.hash = $2 OR o.prev_hash = $2))`,
		id, newHash[:], exp, prevExpires, at, nullTime(at))
	switch {
	case uniqueViolation(err):
		return ErrExists
	case err != nil:
		return err
	case tag.RowsAffected() == 1:
		return nil
	}
	k, err := p.GetKey(ctx, id)
	switch {
	case err != nil:
		return err
	case k.Revoked:
		return ErrNotFound
	case exp != nil && k.Hash != expect:
		return ErrConflict
	}
	return ErrExists
}

func (p *Postgres) SetKeyRotation(ctx context.Context, id string, r Rotation) error {
	return affected(p.pool.Exec(ctx, `UPDATE llmflow_keys SET rotate_every = $2, rotate_grace = $3, rotate_target = $4 WHERE id = $1 AND NOT revoked`,
		id, int64(r.Every), int64(r.Grace), r.Target))
}

func (p *Postgres) DueKeys(ctx context.Context, now time.Time) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+keyCols+keyFrom+`
		WHERE NOT k.revoked AND k.rotate_every > 0
		  AND coalesce(k.rotated_at, k.created_at) + make_interval(secs => k.rotate_every / 1e9) <= $1
		ORDER BY k.id`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (m *Memory) RotateKey(_ context.Context, id string, newHash, expect [32]byte, prevExpires, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	k, ok := m.keys[id]
	if !ok || k.Revoked {
		return ErrNotFound
	}
	if expect != ([32]byte{}) && k.Hash != expect {
		return ErrConflict
	}
	if owner, taken := m.byHash[newHash]; taken && (owner != id || k.Hash == newHash) {
		return ErrExists
	}
	if !k.PrevExpires.IsZero() {
		delete(m.byHash, k.PrevHash)
	}
	k.PrevHash, k.PrevExpires = [32]byte{}, time.Time{}
	if prevExpires.After(at) {
		k.PrevHash, k.PrevExpires = k.Hash, prevExpires
	} else {
		delete(m.byHash, k.Hash)
	}
	k.Hash, k.RotatedAt = newHash, at
	m.byHash[newHash] = id
	return nil
}

func (m *Memory) SetKeyRotation(_ context.Context, id string, r Rotation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	k, ok := m.keys[id]
	if !ok || k.Revoked {
		return ErrNotFound
	}
	k.Rotation = r
	return nil
}

func (m *Memory) DueKeys(_ context.Context, now time.Time) ([]Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Key{}
	for _, k := range m.keys {
		last := k.RotatedAt
		if last.IsZero() {
			last = k.CreatedAt
		}
		if !k.Revoked && k.Rotation.Every > 0 && !last.Add(time.Duration(k.Rotation.Every)).After(now) {
			out = append(out, m.withOrg(k))
		}
	}
	return out, nil
}
