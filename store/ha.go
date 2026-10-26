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
ALTER TABLE llmflow_pool_leases ADD COLUMN IF NOT EXISTS yield boolean NOT NULL DEFAULT false;
ALTER TABLE llmflow_pool_leases ADD COLUMN IF NOT EXISTS capacity integer NOT NULL DEFAULT -1;
ALTER TABLE llmflow_pool_leases ADD COLUMN IF NOT EXISTS reserved text NOT NULL DEFAULT '';
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
	// Yield: the owner asks to be replaced (it cannot read the pool's
	// membership any more, #75); Quiet: this claim took such a lease, so
	// the new owner must not decide for one lease duration.
	Yield, Quiet bool
	// Capacity and Reserved are what the owner published: the pool's C
	// and the integer reservations (JSON) it decides with at this epoch
	// (-1 and "": not published).
	Capacity int
	Reserved string
}

// Claim is one claim of a pool's lease.
type Claim struct {
	Pool, Owner, Addr string
	// Held is the epoch the claimant owns (0: none).
	Held int64
	TTL  time.Duration
	// MayTake: the claimant may take an expired lease (it knows the
	// pool's membership); MayTakeYielded: it may take a live lease whose
	// owner yields (its own view of the membership is fresh).
	MayTake, MayTakeYielded bool
	// Yield, Capacity and Reserved are written with the claimant's lease.
	Yield    bool
	Capacity int
	Reserved string
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
	// ClaimLease renews the lease when the claimant holds it at epoch Held
	// and it has not expired; takes it (epoch + 1) when it has expired (or
	// does not exist) and MayTake, or when it is live but its owner yields
	// and MayTakeYielded (Quiet); otherwise changes nothing. It returns the
	// lease as it stands afterwards.
	ClaimLease(ctx context.Context, c Claim) (Lease, error)
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

func (p *Postgres) ClaimLease(ctx context.Context, c Claim) (Lease, error) {
	l := Lease{Pool: c.Pool}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The row exists (epoch 0, expired) before anyone locks it, so the
		// decision below is always taken under the row lock: of several
		// claimants exactly one sees the lease free.
		if _, err := tx.Exec(ctx, `INSERT INTO llmflow_pool_leases (pool, owner, addr, epoch, expires_at)
			VALUES ($1, '', '', 0, '-infinity') ON CONFLICT (pool) DO NOTHING`, c.Pool); err != nil {
			return err
		}
		var owner string
		var epoch int64
		var live, yield bool
		if err := tx.QueryRow(ctx, `SELECT owner, epoch, expires_at > now(), yield FROM llmflow_pool_leases WHERE pool = $1 FOR UPDATE`, c.Pool).
			Scan(&owner, &epoch, &live, &yield); err != nil {
			return err
		}
		next, quiet := int64(-1), false
		switch {
		case owner == c.Owner && epoch == c.Held && c.Held > 0 && live:
			next = epoch // renewal
		case !live && (c.MayTake || owner == c.Owner && epoch == c.Held && c.Held > 0):
			next = epoch + 1 // an expired lease (or our own, lapsed)
		case live && yield && owner != c.Owner && c.MayTakeYielded:
			// A handover: the owner can no longer read its membership. It
			// may still be deciding until its validity ends, so the new
			// owner keeps quiet for a whole lease first.
			next, quiet = epoch+1, true
		}
		if next >= 0 {
			if _, err := tx.Exec(ctx, `UPDATE llmflow_pool_leases SET owner = $2, addr = $3, epoch = $4,
				expires_at = now() + $5 * interval '1 millisecond', yield = $6, capacity = $7, reserved = $8 WHERE pool = $1`,
				c.Pool, c.Owner, c.Addr, next, c.TTL.Milliseconds(), c.Yield, c.Capacity, c.Reserved); err != nil {
				return err
			}
		}
		l.Quiet = quiet
		return tx.QueryRow(ctx, `SELECT owner, addr, epoch, expires_at > now(), yield, capacity, reserved FROM llmflow_pool_leases WHERE pool = $1`, c.Pool).
			Scan(&l.Owner, &l.Addr, &l.Epoch, &l.Live, &l.Yield, &l.Capacity, &l.Reserved)
	})
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
