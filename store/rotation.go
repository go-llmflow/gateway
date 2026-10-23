package store

import (
	"context"
	"time"
)

// Key rotation: a key keeps its ID (budgets, limits, allowlists and
// reports continue) and gets a new secret; the previous secret stays valid
// until a grace deadline.
const rotationSchema = `
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS prev_hash bytea UNIQUE;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS prev_expires timestamptz;
ALTER TABLE llmflow_keys ADD COLUMN IF NOT EXISTS rotated_at timestamptz;
`

func (p *Postgres) RotateKey(ctx context.Context, id string, newHash [32]byte, prevExpires, at time.Time) error {
	// A hash may be nobody's current or previous secret: the two unique
	// constraints do not see across columns, so check both in the statement.
	tag, err := p.pool.Exec(ctx, `
		UPDATE llmflow_keys SET
			prev_hash    = CASE WHEN $3::timestamptz > $4::timestamptz THEN hash END,
			prev_expires = CASE WHEN $3::timestamptz > $4::timestamptz THEN $3::timestamptz END,
			hash = $2, rotated_at = $4
		WHERE id = $1 AND NOT revoked
		  AND NOT EXISTS (SELECT 1 FROM llmflow_keys WHERE hash = $2 OR prev_hash = $2)`,
		id, newHash[:], prevExpires, at)
	switch {
	case uniqueViolation(err):
		return ErrExists
	case err != nil:
		return err
	case tag.RowsAffected() == 1:
		return nil
	}
	// Tell an unknown or revoked key from a taken hash.
	k, err := p.GetKey(ctx, id)
	if err != nil {
		return err
	}
	if k.Revoked {
		return ErrNotFound
	}
	return ErrExists
}
