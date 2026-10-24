package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The audit log: an append-only sequence of entries, each committing to the
// one before it through PrevHash, and signed checkpoints over the chain.
// The store only keeps rows in order; hashing and signing are the caller's
// (package audit), and Append hands the caller the predecessor under the
// same lock that assigns the sequence number, so two writers -- two
// replicas -- can never chain from the same entry.

// AuditEntry is one recorded event. Before and After are JSON, kept as the
// exact bytes that were hashed.
type AuditEntry struct {
	// V is the canonical form the hash was computed with (package audit).
	V         int
	Seq       int64
	Time      time.Time // microsecond precision
	Kind      string    // admin, auth, scim, system
	Actor     string
	Roles     string // JSON list of the actor's role bindings
	Action    string
	Target    string
	Before    string
	After     string
	Status    int
	SourceIP  string
	RequestID string
	PrevHash  []byte
	Hash      []byte
}

// AuditCheckpoint signs the chain up to Seq, whose entry hash is Hash.
type AuditCheckpoint struct {
	Seq       int64
	Hash      []byte
	Time      time.Time
	KeyID     string
	Signature []byte
}

// AuditHasher computes an entry's hash once its Seq and PrevHash are known.
type AuditHasher func(e *AuditEntry) []byte

// ErrAuditUpdate is what the PostgreSQL trigger raises on an UPDATE.
var ErrAuditUpdate = errors.New("the audit log is append-only")

const auditSchema = `
CREATE TABLE IF NOT EXISTS llmflow_audit (
	seq        bigint PRIMARY KEY,
	ts         timestamptz NOT NULL,
	kind       text NOT NULL,
	actor      text NOT NULL,
	roles      text NOT NULL,
	action     text NOT NULL,
	target     text NOT NULL,
	before     text NOT NULL,
	after      text NOT NULL,
	status     integer NOT NULL,
	source_ip  text NOT NULL,
	request_id text NOT NULL,
	prev_hash  bytea NOT NULL,
	hash       bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS llmflow_audit_checkpoints (
	seq       bigint PRIMARY KEY,
	hash      bytea NOT NULL,
	ts        timestamptz NOT NULL,
	key_id    text NOT NULL,
	signature bytea NOT NULL
);
ALTER TABLE llmflow_audit ADD COLUMN IF NOT EXISTS v smallint NOT NULL DEFAULT 1;
CREATE OR REPLACE FUNCTION llmflow_audit_no_update() RETURNS trigger AS $$
BEGIN
	RAISE EXCEPTION 'the audit log is append-only';
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS llmflow_audit_no_update ON llmflow_audit;
CREATE TRIGGER llmflow_audit_no_update BEFORE UPDATE ON llmflow_audit
	FOR EACH ROW EXECUTE FUNCTION llmflow_audit_no_update();
DROP TRIGGER IF EXISTS llmflow_audit_cp_no_update ON llmflow_audit_checkpoints;
CREATE TRIGGER llmflow_audit_cp_no_update BEFORE UPDATE ON llmflow_audit_checkpoints
	FOR EACH ROW EXECUTE FUNCTION llmflow_audit_no_update();
`

// auditLock is the advisory lock key serialising appends across replicas.
const auditLock = 0x6c6c6d66_61756474 // "llmfaudt"

const auditCols = `seq, ts, kind, actor, roles, action, target, before, after, status, source_ip, request_id, prev_hash, hash, v`

func scanAudit(r pgx.Row) (AuditEntry, error) {
	var e AuditEntry
	var v int16
	err := r.Scan(&e.Seq, &e.Time, &e.Kind, &e.Actor, &e.Roles, &e.Action, &e.Target, &e.Before, &e.After, &e.Status, &e.SourceIP, &e.RequestID, &e.PrevHash, &e.Hash, &v)
	e.V = int(v)
	e.Time = e.Time.UTC()
	return e, err
}

func (p *Postgres) AuditAppend(ctx context.Context, e AuditEntry, hash AuditHasher) (AuditEntry, error) {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(auditLock)); err != nil {
			return err
		}
		var seq int64
		var prev []byte
		err := tx.QueryRow(ctx, `SELECT seq, hash FROM llmflow_audit ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// An empty table after a prune starts from the last checkpoint.
			err = tx.QueryRow(ctx, `SELECT seq, hash FROM llmflow_audit_checkpoints ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
			if errors.Is(err, pgx.ErrNoRows) {
				seq, prev, err = 0, make([]byte, 32), nil
			}
		}
		if err != nil {
			return err
		}
		e.Seq, e.PrevHash = seq+1, prev
		e.Time = e.Time.UTC().Truncate(time.Microsecond)
		e.Hash = hash(&e)
		_, err = tx.Exec(ctx, `INSERT INTO llmflow_audit (`+auditCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			e.Seq, e.Time, e.Kind, e.Actor, e.Roles, e.Action, e.Target, e.Before, e.After, e.Status, e.SourceIP, e.RequestID, e.PrevHash, e.Hash, int16(e.V))
		return err
	})
	return e, err
}

func (p *Postgres) AuditRange(ctx context.Context, from int64, limit int) ([]AuditEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+auditCols+` FROM llmflow_audit WHERE seq >= $1 ORDER BY seq LIMIT $2`, from, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) { return scanAudit(r) })
}

func (p *Postgres) AuditAddCheckpoint(ctx context.Context, c AuditCheckpoint) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_audit_checkpoints (seq, hash, ts, key_id, signature) VALUES ($1,$2,$3,$4,$5)`,
		c.Seq, c.Hash, c.Time.UTC().Truncate(time.Microsecond), c.KeyID, c.Signature)
	if uniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (p *Postgres) AuditCheckpoints(ctx context.Context) ([]AuditCheckpoint, error) {
	rows, err := p.pool.Query(ctx, `SELECT seq, hash, ts, key_id, signature FROM llmflow_audit_checkpoints ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditCheckpoint, error) {
		var c AuditCheckpoint
		err := r.Scan(&c.Seq, &c.Hash, &c.Time, &c.KeyID, &c.Signature)
		c.Time = c.Time.UTC()
		return c, err
	})
}

func (p *Postgres) AuditPrune(ctx context.Context, through int64) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM llmflow_audit WHERE seq <= $1`, through)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type memAudit struct {
	entries     []AuditEntry
	checkpoints []AuditCheckpoint
}

func (m *Memory) AuditAppend(_ context.Context, e AuditEntry, hash AuditHasher) (AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return e, m.Fail
	}
	seq, prev := int64(0), make([]byte, 32)
	if n := len(m.audit.entries); n > 0 {
		seq, prev = m.audit.entries[n-1].Seq, m.audit.entries[n-1].Hash
	} else if n := len(m.audit.checkpoints); n > 0 {
		seq, prev = m.audit.checkpoints[n-1].Seq, m.audit.checkpoints[n-1].Hash
	}
	e.Seq, e.PrevHash = seq+1, prev
	e.Time = e.Time.UTC().Truncate(time.Microsecond)
	e.Hash = hash(&e)
	m.audit.entries = append(m.audit.entries, e)
	return e, nil
}

func (m *Memory) AuditRange(_ context.Context, from int64, limit int) ([]AuditEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []AuditEntry{}
	for _, e := range m.audit.entries {
		if e.Seq >= from && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *Memory) AuditAddCheckpoint(_ context.Context, c AuditCheckpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for _, o := range m.audit.checkpoints {
		if o.Seq == c.Seq {
			return ErrExists
		}
	}
	c.Time = c.Time.UTC().Truncate(time.Microsecond)
	m.audit.checkpoints = append(m.audit.checkpoints, c)
	return nil
}

func (m *Memory) AuditCheckpoints(context.Context) ([]AuditCheckpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	return append([]AuditCheckpoint{}, m.audit.checkpoints...), nil
}

func (m *Memory) AuditPrune(_ context.Context, through int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	kept := m.audit.entries[:0:0]
	var n int64
	for _, e := range m.audit.entries {
		if e.Seq <= through {
			n++
			continue
		}
		kept = append(kept, e)
	}
	m.audit.entries = kept
	return n, nil
}

// DeleteAudit removes one stored entry, bypassing everything: for tests
// that must show the verifier catches it.
func (m *Memory) DeleteAudit(seq int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.audit.entries[:0:0]
	for _, e := range m.audit.entries {
		if e.Seq != seq {
			kept = append(kept, e)
		}
	}
	m.audit.entries = kept
}

// TamperAudit rewrites one stored entry in place, bypassing the chain: for
// tests that must show the verifier catches it.
func (m *Memory) TamperAudit(seq int64, f func(*AuditEntry)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.audit.entries {
		if m.audit.entries[i].Seq == seq {
			f(&m.audit.entries[i])
		}
	}
}
