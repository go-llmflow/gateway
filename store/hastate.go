package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The rest of what several replicas share (#74): SSO logins and sessions
// (sealed by the gateway: the store keeps opaque bytes under a hashed id),
// the soft-budget alert outbox, and key revocations broadcast with
// LISTEN/NOTIFY. Expiry is PostgreSQL's clock here too.
const haStateSchema = `
CREATE TABLE IF NOT EXISTS llmflow_sso (
	kind       text NOT NULL,
	id         bytea NOT NULL,
	net        text NOT NULL DEFAULT '',
	sealed     bytea NOT NULL,
	expires_at timestamptz NOT NULL,
	PRIMARY KEY (kind, id)
);
CREATE INDEX IF NOT EXISTS llmflow_sso_expiry ON llmflow_sso (expires_at);
CREATE TABLE IF NOT EXISTS llmflow_alert_outbox (
	key        text NOT NULL,
	channel    text NOT NULL,
	payload    bytea NOT NULL,
	attempts   integer NOT NULL DEFAULT 0,
	state      text NOT NULL DEFAULT 'pending',
	next_at    timestamptz NOT NULL DEFAULT now(),
	created_at timestamptz NOT NULL DEFAULT now(),
	last_error text NOT NULL DEFAULT '',
	PRIMARY KEY (key, channel)
);
CREATE INDEX IF NOT EXISTS llmflow_alert_outbox_due ON llmflow_alert_outbox (next_at) WHERE state = 'pending';
`

// ErrFull: a bound on stored logins or sessions was reached.
var ErrFull = errors.New("store: too many entries")

// SealedLimits bound what PutSealed stores: in all for the kind, and per
// client network (0: no bound).
type SealedLimits struct{ Total, PerNet int }

// HAStateStore is the shared state besides pool ownership.
type HAStateStore interface {
	// PutSealed stores sealed bytes under (kind, id) for ttl (by the
	// store's clock), unless a limit is reached (ErrFull).
	PutSealed(ctx context.Context, kind string, id [32]byte, net string, sealed []byte, ttl time.Duration, lim SealedLimits) error
	// GetSealed reads a live entry.
	GetSealed(ctx context.Context, kind string, id [32]byte) ([]byte, bool, error)
	// TakeSealed reads and deletes a live entry (one use).
	TakeSealed(ctx context.Context, kind string, id [32]byte) ([]byte, bool, error)
	DropSealed(ctx context.Context, kind string, id [32]byte) error
	// PurgeSealed deletes expired entries and old finished alerts.
	PurgeSealed(ctx context.Context) error

	// EnqueueAlert adds one alert per channel, once per key: a second
	// enqueue of the same (key, channel) changes nothing.
	EnqueueAlert(ctx context.Context, key string, channels []string, payload []byte) error
	// ClaimAlerts takes up to n due alerts and hides them for lease (a
	// replica that dies while sending leaves them to the next one).
	ClaimAlerts(ctx context.Context, n int, lease time.Duration) ([]OutboxAlert, error)
	// FinishAlert records a delivery attempt: sent, given up (failed), or
	// retried after the given delay.
	FinishAlert(ctx context.Context, key, channel, state string, retryIn time.Duration, lastErr string) error

	// NotifyKeys broadcasts a revocation to every replica.
	NotifyKeys(ctx context.Context, payload string) error
	// ListenKeys calls on for every revocation until ctx ends, and
	// connected each time it (re)connects (notifications sent while it was
	// not connected are lost, PostgreSQL does not keep them).
	ListenKeys(ctx context.Context, connected func(), on func(payload string))
}

// OutboxAlert is a claimed alert.
type OutboxAlert struct {
	Key, Channel string
	Payload      []byte
	Attempts     int
}

var _ HAStateStore = (*Postgres)(nil)

func (p *Postgres) PutSealed(ctx context.Context, kind string, id [32]byte, net string, sealed []byte, ttl time.Duration, lim SealedLimits) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_sso (kind, id, net, sealed, expires_at)
		SELECT $1, $2, $3, $4, now() + $5 * interval '1 millisecond'
		WHERE ($6 = 0 OR (SELECT count(*) FROM llmflow_sso WHERE kind = $1 AND expires_at > now()) < $6)
		  AND ($7 = 0 OR (SELECT count(*) FROM llmflow_sso WHERE kind = $1 AND net = $3 AND expires_at > now()) < $7)
		ON CONFLICT (kind, id) DO UPDATE SET sealed = EXCLUDED.sealed, expires_at = EXCLUDED.expires_at, net = EXCLUDED.net`,
		kind, id[:], net, sealed, ttl.Milliseconds(), lim.Total, lim.PerNet)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFull
	}
	return nil
}

func (p *Postgres) GetSealed(ctx context.Context, kind string, id [32]byte) ([]byte, bool, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT sealed FROM llmflow_sso WHERE kind = $1 AND id = $2 AND expires_at > now()`, kind, id[:]).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (p *Postgres) TakeSealed(ctx context.Context, kind string, id [32]byte) ([]byte, bool, error) {
	var b []byte
	var live bool
	err := p.pool.QueryRow(ctx, `DELETE FROM llmflow_sso WHERE kind = $1 AND id = $2 RETURNING sealed, expires_at > now()`, kind, id[:]).Scan(&b, &live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (p *Postgres) DropSealed(ctx context.Context, kind string, id [32]byte) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM llmflow_sso WHERE kind = $1 AND id = $2`, kind, id[:])
	return err
}

func (p *Postgres) PurgeSealed(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM llmflow_sso WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `DELETE FROM llmflow_alert_outbox WHERE state <> 'pending' AND created_at < now() - interval '30 days'`)
	return err
}

func (p *Postgres) EnqueueAlert(ctx context.Context, key string, channels []string, payload []byte) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO llmflow_alert_outbox (key, channel, payload)
		SELECT $1, c, $3 FROM unnest($2::text[]) AS c
		ON CONFLICT (key, channel) DO NOTHING`, key, channels, payload)
	return err
}

func (p *Postgres) ClaimAlerts(ctx context.Context, n int, lease time.Duration) ([]OutboxAlert, error) {
	// SKIP LOCKED: two replicas claiming at once take different rows; the
	// lease (next_at moved forward) keeps a claimed row from the others
	// after the transaction.
	rows, err := p.pool.Query(ctx, `
		UPDATE llmflow_alert_outbox o SET attempts = o.attempts + 1, next_at = now() + $2 * interval '1 millisecond'
		FROM (SELECT key, channel FROM llmflow_alert_outbox
		      WHERE state = 'pending' AND next_at <= now() ORDER BY next_at LIMIT $1 FOR UPDATE SKIP LOCKED) d
		WHERE o.key = d.key AND o.channel = d.channel
		RETURNING o.key, o.channel, o.payload, o.attempts`, n, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxAlert
	for rows.Next() {
		var a OutboxAlert
		var att int32
		if err := rows.Scan(&a.Key, &a.Channel, &a.Payload, &att); err != nil {
			return nil, err
		}
		a.Attempts = int(att)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *Postgres) FinishAlert(ctx context.Context, key, channel, state string, retryIn time.Duration, lastErr string) error {
	if len(lastErr) > 512 {
		lastErr = lastErr[:512]
	}
	_, err := p.pool.Exec(ctx, `UPDATE llmflow_alert_outbox SET state = $3, next_at = now() + $4 * interval '1 millisecond', last_error = $5
		WHERE key = $1 AND channel = $2`, key, channel, state, retryIn.Milliseconds(), lastErr)
	return err
}

// keysChannel is the NOTIFY channel of key revocations.
const keysChannel = "llmflow_keys"

func (p *Postgres) NotifyKeys(ctx context.Context, payload string) error {
	_, err := p.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, keysChannel, payload)
	return err
}

func (p *Postgres) ListenKeys(ctx context.Context, connected func(), on func(payload string)) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		if p.listenOnce(ctx, connected, on) {
			backoff = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// listenOnce holds one connection in LISTEN until it fails; a silent
// connection (a partition) is found by a ping every few seconds.
func (p *Postgres) listenOnce(ctx context.Context, connected func(), on func(string)) bool {
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	pc, err := p.pool.Acquire(actx)
	cancel()
	if err != nil {
		return false
	}
	// Out of the pool: a connection in LISTEN must not serve queries.
	c := pc.Hijack()
	defer c.Close(context.Background())
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = c.Exec(lctx, "LISTEN "+keysChannel)
	cancel()
	if err != nil {
		return false
	}
	connected()
	for {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := c.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			on(n.Payload)
			continue
		case ctx.Err() != nil:
			return true
		case errors.Is(err, context.DeadlineExceeded):
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = c.Ping(pctx)
			cancel()
			if err == nil {
				continue
			}
		}
		return true
	}
}
