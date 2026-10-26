package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pool ownership for several gateway replicas (package ha): one lease row
// per pool, whose epoch is the fencing token, and the state an owner
// leaves for the next one.
//
// Every expiry is computed with PostgreSQL's clock (now()), never with a
// replica's, so a replica whose clock is wrong cannot take a lease early
// or keep one late. A takeover is one conditional statement: of several
// replicas racing on an expired lease exactly one row update wins (the
// conflicting row is locked by INSERT ... ON CONFLICT DO UPDATE), and it
// increments the epoch.
const haSchema = `
CREATE TABLE IF NOT EXISTS llmflow_pool_leases (
	pool       text PRIMARY KEY,
	owner      text NOT NULL,
	addr       text NOT NULL,
	epoch      bigint NOT NULL,
	expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS llmflow_pool_usage (
	pool       text NOT NULL,
	org        text NOT NULL,
	usage      double precision NOT NULL,
	inflight   integer NOT NULL,
	epoch      bigint NOT NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (pool, org)
);
`

// Lease is a pool's lease as PostgreSQL holds it.
type Lease struct {
	Pool, Owner, Addr string
	// Epoch is the fencing token: it grows by one at every change of
	// owner (or when an owner takes back a lease it let expire).
	Epoch int64
	// Live: the lease has not expired (by PostgreSQL's clock).
	Live bool
}

// PoolUsage is one organisation's fair-share state in a pool, as the
// owner persisted it.
type PoolUsage struct {
	Org      string
	Usage    float64
	InFlight int
	// Age is how long ago it was written, by PostgreSQL's clock.
	Age time.Duration
}

// ErrFenced: the write came from an owner whose lease is no longer the
// current one (expired, or taken over with a higher epoch).
var ErrFenced = errors.New("store: fenced: the lease is held by a newer owner or has expired")

// HAStore is what package ha needs from the store. Only PostgreSQL has it.
type HAStore interface {
	// ClaimLease renews the lease when owner holds it at epoch held and it
	// has not expired, takes it (epoch + 1) when it has expired or does
	// not exist, and otherwise changes nothing. It returns the lease as it
	// stands afterwards.
	ClaimLease(ctx context.Context, pool, owner, addr string, held int64, ttl time.Duration) (Lease, error)
	// ReleaseLease ends owner's lease at epoch now (graceful shutdown).
	ReleaseLease(ctx context.Context, pool, owner string, epoch int64) error
	// SaveUsage writes the owner's state, fenced: ErrFenced unless owner
	// holds the live lease at epoch, checked under a row lock that a
	// takeover must wait for.
	SaveUsage(ctx context.Context, pool, owner string, epoch int64, rows []PoolUsage) error
	// LoadUsage reads what the last owner persisted.
	LoadUsage(ctx context.Context, pool string) ([]PoolUsage, error)
}

var _ HAStore = (*Postgres)(nil)

func (p *Postgres) ClaimLease(ctx context.Context, pool, owner, addr string, held int64, ttl time.Duration) (Lease, error) {
	ms := ttl.Milliseconds()
	_, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_pool_leases AS l (pool, owner, addr, epoch, expires_at)
		VALUES ($1, $2, $3, 1, now() + $5 * interval '1 millisecond')
		ON CONFLICT (pool) DO UPDATE
		SET owner = EXCLUDED.owner, addr = EXCLUDED.addr, expires_at = EXCLUDED.expires_at,
		    epoch = CASE WHEN l.owner = EXCLUDED.owner AND l.epoch = $4 AND l.expires_at > now()
		                 THEN l.epoch ELSE l.epoch + 1 END
		WHERE l.expires_at <= now() OR (l.owner = EXCLUDED.owner AND l.epoch = $4)`,
		pool, owner, addr, held, ms)
	if err != nil {
		return Lease{}, err
	}
	l := Lease{Pool: pool}
	err = p.pool.QueryRow(ctx, `SELECT owner, addr, epoch, expires_at > now() FROM llmflow_pool_leases WHERE pool = $1`, pool).
		Scan(&l.Owner, &l.Addr, &l.Epoch, &l.Live)
	return l, err
}

func (p *Postgres) ReleaseLease(ctx context.Context, pool, owner string, epoch int64) error {
	_, err := p.pool.Exec(ctx, `UPDATE llmflow_pool_leases SET expires_at = now() WHERE pool = $1 AND owner = $2 AND epoch = $3 AND expires_at > now()`,
		pool, owner, epoch)
	return err
}

func (p *Postgres) SaveUsage(ctx context.Context, pool, owner string, epoch int64, rows []PoolUsage) error {
	orgs := make([]string, len(rows))
	usage := make([]float64, len(rows))
	inflight := make([]int32, len(rows))
	for i, r := range rows {
		orgs[i], usage[i], inflight[i] = r.Org, r.Usage, int32(min(r.InFlight, 1<<30))
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// FOR SHARE: a takeover (an UPDATE of this row) waits until this
		// transaction ends, so the check holds until the commit
		// (Kleppmann's fencing token, checked by the storage).
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM llmflow_pool_leases
			WHERE pool = $1 AND owner = $2 AND epoch = $3 AND expires_at > now() FOR SHARE`, pool, owner, epoch).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO llmflow_pool_usage (pool, org, usage, inflight, epoch, updated_at)
			SELECT $1, o, u, n, $2, now() FROM unnest($3::text[], $4::float8[], $5::int[]) AS t(o, u, n)
			ON CONFLICT (pool, org) DO UPDATE
			SET usage = EXCLUDED.usage, inflight = EXCLUDED.inflight, epoch = EXCLUDED.epoch, updated_at = EXCLUDED.updated_at
			WHERE llmflow_pool_usage.epoch <= EXCLUDED.epoch`, pool, epoch, orgs, usage, inflight)
		return err
	})
}

func (p *Postgres) LoadUsage(ctx context.Context, pool string) ([]PoolUsage, error) {
	rows, err := p.pool.Query(ctx, `SELECT org, usage, inflight, GREATEST(extract(epoch FROM now() - updated_at), 0)::float8
		FROM llmflow_pool_usage WHERE pool = $1 ORDER BY org`, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolUsage
	for rows.Next() {
		var u PoolUsage
		var age float64
		var n int32
		if err := rows.Scan(&u.Org, &u.Usage, &n, &age); err != nil {
			return nil, err
		}
		u.InFlight, u.Age = int(n), time.Duration(age*float64(time.Second))
		out = append(out, u)
	}
	return out, rows.Err()
}
