package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// The budget ledger on PostgreSQL (ledger.go). Lock order, the same in
// every transaction, so that none can deadlock with another: a budget's
// row (llmflow_budgets_v2), then its ledger rows (llmflow_budget_ledger) in
// the canonical order (kind, name, model, unit, period_start), then the
// grant rows of each ledger row while that ledger row is held. Growth and
// limit changes take the budget row; settlement, returns and releases
// start at the ledger rows and never take a budget row.

const keyWhere = `kind = $1 AND name = $2 AND model = $3 AND unit = $4`

func keyArgs(k BudgetKey) []any { return []any{string(k.Kind), k.Name, k.Model, k.Unit} }

// usageMatch is the condition on llmflow_usage of a budget key's records
// ($1..$4 the key, see Matches).
func usageMatch(k BudgetKey) (string, error) {
	scope := ""
	switch k.Kind {
	case ScopeOrg:
		scope = "org = $2"
	case ScopeTeam:
		scope = "team = $2"
	case ScopeProject:
		scope = "project <> '' AND team || '/' || project = $2"
	case ScopeKey:
		scope = "key_id = $2"
	case ScopeUser:
		scope = "user_name <> '' AND user_name = $2"
	case ScopeProfileWeek, ScopeProfileSession:
		scope = "user_name <> '' AND user_name = $2 AND NOT overflow"
	case ScopeProfileOverflow:
		scope = "user_name <> '' AND user_name = $2 AND overflow"
	case ScopeTag:
		scope = "$2 = ANY (tags)"
	default:
		return "", fmt.Errorf("budget kind %q", k.Kind)
	}
	return "unit = $4 AND ($3 = '' OR model = $3) AND " + scope, nil
}

// costNanoSQL is a usage row's cost in nano-units ($6 the bound), as
// RecordCostNano computes it.
const costNanoSQL = `coalesce(cost_nano, LEAST(ceil(cost * 1e9), $6)::bigint)`

// bucketSQL is the start of a usage row's bucket (BucketOf of its end,
// bucketAt).
const bucketSQL = `to_timestamp(floor(extract(epoch FROM ts) / 300) * 300)`

// seedLedger sets a ledger row's spent from the usage records stored so
// far (the usage inserts must be locked out, lockUsage): a calendar row's
// spent column, or a rolling row's buckets. now bounds the buckets read.
func seedLedger(ctx context.Context, tx pgx.Tx, k BudgetKey, p Period, start, now time.Time) error {
	match, err := usageMatch(k)
	if err != nil {
		return err
	}
	args := append(keyArgs(k), start.UTC(), MaxLedgerNano)
	if d, ok := p.Rolling(); ok && rollingRow(start) {
		// Buckets are written with their ledger row held (when it exists).
		if _, err := holdLedger(ctx, tx, k, start); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_buckets WHERE `+keyWhere, keyArgs(k)...); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO llmflow_budget_buckets (kind, name, model, unit, bucket_start, spent_nano)
			SELECT $1, $2, $3, $4, `+bucketSQL+`, LEAST(sum(`+costNanoSQL+`), $6)
			FROM llmflow_usage WHERE ts >= $5 AND `+match+`
			GROUP BY 5`, append(keyArgs(k), WindowStart(now, d), MaxLedgerNano)...)
		return err
	}
	// ts >= start lets the (ts, team) index serve: a record admitted in the
	// period ended in it or later.
	_, err = tx.Exec(ctx, `UPDATE llmflow_budget_ledger SET spent_nano = (SELECT LEAST(coalesce(sum(`+costNanoSQL+`), 0), $6)
		FROM llmflow_usage WHERE ts >= $5 AND coalesce(admitted_at, ts) >= $5 AND `+match+`)
		WHERE `+keyWhere+` AND period_start = $5`, args...)
	return err
}

// windowSpent is a rolling row's spent at now (its ledger row held), and
// drops only the buckets older than any window can reach (MaxRollingWindow,
// not d: the window may be widened later, #98).
func windowSpent(ctx context.Context, tx pgx.Tx, k BudgetKey, d time.Duration, now time.Time) (int64, error) {
	from := WindowStart(now, d)
	if _, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_buckets WHERE `+keyWhere+` AND bucket_start < $5`,
		append(keyArgs(k), retainFrom(now, d))...); err != nil {
		return 0, err
	}
	var s int64
	err := tx.QueryRow(ctx, `SELECT LEAST(coalesce(sum(spent_nano), 0), $6)::bigint FROM llmflow_budget_buckets WHERE `+keyWhere+` AND bucket_start >= $5`,
		append(keyArgs(k), from, MaxLedgerNano)...).Scan(&s)
	return s, err
}

// windowBuckets lists a rolling row's buckets in the window, oldest first.
func windowBuckets(ctx context.Context, tx pgx.Tx, k BudgetKey, d time.Duration, now time.Time) ([]bucketSpend, error) {
	rows, err := tx.Query(ctx, `SELECT bucket_start, spent_nano FROM llmflow_budget_buckets WHERE `+keyWhere+` AND bucket_start >= $5 ORDER BY bucket_start`,
		append(keyArgs(k), WindowStart(now, d))...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (bucketSpend, error) {
		var b bucketSpend
		err := r.Scan(&b.start, &b.spent)
		return b, err
	})
}

// lockLedger locks a budget period's ledger row, creating it with the cost
// of the period's usage records when it does not exist, and returns its
// spent at now (a rolling window's, under period p) and the sum of its
// grants.
func lockLedger(ctx context.Context, tx pgx.Tx, k BudgetKey, p Period, start, now time.Time) (LedgerState, error) {
	var st LedgerState
	args := append(keyArgs(k), start.UTC())
	err := tx.QueryRow(ctx, `SELECT spent_nano FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5 FOR UPDATE`, args...).Scan(&st.Spent)
	if errors.Is(err, pgx.ErrNoRows) {
		tag, ierr := tx.Exec(ctx, `INSERT INTO llmflow_budget_ledger (kind, name, model, unit, period_start, spent_nano)
			VALUES ($1, $2, $3, $4, $5, 0) ON CONFLICT DO NOTHING`, args...)
		if ierr != nil {
			return st, ierr
		}
		if tag.RowsAffected() == 1 {
			if err := seedLedger(ctx, tx, k, p, start, now); err != nil {
				return st, err
			}
		}
		err = tx.QueryRow(ctx, `SELECT spent_nano FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5 FOR UPDATE`, args...).Scan(&st.Spent)
	}
	if err != nil {
		return st, err
	}
	if d, ok := p.Rolling(); ok && rollingRow(start) {
		if st.Spent, err = windowSpent(ctx, tx, k, d, now); err != nil {
			return st, err
		}
	}
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(granted_nano), 0)::bigint FROM llmflow_budget_grants WHERE `+keyWhere+` AND period_start = $5`, args...).Scan(&st.Granted)
	return st, err
}

// lockedBudget reads a budget row under a lock (share or update); ok is
// false when it does not exist.
func lockedBudget(ctx context.Context, tx pgx.Tx, k BudgetKey, mode string) (Budget, bool, error) {
	b := Budget{BudgetKey: k}
	var period string
	err := tx.QueryRow(ctx, `SELECT period, max_cost, soft_limit, temp_increase, temp_expiry, limit_epoch, arming FROM llmflow_budgets_v2 WHERE `+keyWhere+` FOR `+mode,
		keyArgs(k)...).Scan(&period, &b.MaxCost, &b.SoftLimit, &b.TempIncrease, &b.TempExpiry, &b.Epoch, &b.Arming)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	b.Period = Period(period)
	return b, err == nil, err
}

func (p *Postgres) SetBudgetChecked(ctx context.Context, b Budget, now time.Time, lendFor time.Duration, force bool) error {
	if err := ValidateBudget(b); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := lockUsage(ctx, tx); err != nil {
			return err
		}
		// A budget created by two administrators at once: the second waits.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, b.BudgetKey.String()); err != nil {
			return err
		}
		cur, exists, err := lockedBudget(ctx, tx, b.BudgetKey, "UPDATE")
		if err != nil {
			return err
		}
		var old *Budget
		b.TempIncrease, b.TempExpiry, b.Epoch, b.Arming = 0, nil, 0, !p.NoArming
		if exists {
			b.TempIncrease, b.TempExpiry, b.Epoch, b.Arming = cur.TempIncrease, cur.TempExpiry, cur.Epoch, cur.Arming
			old = &cur
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_arming WHERE `+keyWhere, keyArgs(b.BudgetKey)...); err != nil {
				return err
			}
			// Created again: the records stored while no budget existed
			// settled nothing in a ledger row the old one left.
			if err := seedLedger(ctx, tx, b.BudgetKey, b.Period, b.Period.Start(now), now); err != nil {
				return err
			}
		}
		st, err := lockLedger(ctx, tx, b.BudgetKey, b.Period, b.Period.Start(now), now)
		if err != nil {
			return err
		}
		if _, err := checkChange(old, b, st, now, lendFor, force); err != nil {
			return err
		}
		// Every change takes a new epoch (ledger.go).
		b.Epoch = nextEpoch(b.Epoch, now)
		_, err = tx.Exec(ctx, `
			INSERT INTO llmflow_budgets_v2 (kind, name, model, unit, period, max_cost, soft_limit, limit_epoch, arming, warn_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (kind, name, model, unit) DO UPDATE
			SET period = EXCLUDED.period, max_cost = EXCLUDED.max_cost, soft_limit = EXCLUDED.soft_limit, limit_epoch = EXCLUDED.limit_epoch, arming = EXCLUDED.arming, warn_at = EXCLUDED.warn_at`,
			string(b.Kind), b.Name, b.Model, b.Unit, string(b.Period), b.MaxCost, b.SoftLimit, b.Epoch, b.Arming, toInt32(b.WarnAt))
		return err
	})
}

// lockUsage blocks the inserts of usage records until the transaction
// ends, so that a ledger row's starting spend (the usage stored so far) and
// the settlement of the records stored after it never miss a record
// between them. SHARE conflicts with the inserts' ROW EXCLUSIVE only. Taken
// first, before any row lock, in every transaction that creates a ledger
// row (the inserts take it first too).
func lockUsage(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE llmflow_usage IN SHARE MODE`)
	return err
}

// armingPre is the sum of an arming budget's reports, and whether every
// registered process has reported; its budget row must be held.
func armingPre(ctx context.Context, tx pgx.Tx, b Budget) (int64, bool, error) {
	if !b.Arming {
		return 0, true, nil
	}
	var missing int
	var pre int64
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM llmflow_budget_holders h WHERE NOT EXISTS (SELECT 1 FROM llmflow_budget_arming a
			WHERE a.kind = $1 AND a.name = $2 AND a.model = $3 AND a.unit = $4 AND a.incarnation = h.incarnation)),
		(SELECT LEAST(coalesce(sum(a.pre_nano), 0), $5)::bigint FROM llmflow_budget_arming a JOIN llmflow_budget_holders h USING (incarnation)
			WHERE a.kind = $1 AND a.name = $2 AND a.model = $3 AND a.unit = $4)`,
		append(keyArgs(b.BudgetKey), MaxLedgerNano)...).Scan(&missing, &pre)
	return pre, missing == 0, err
}

func (p *Postgres) RegisterHolder(ctx context.Context, h Holder) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO llmflow_budget_holders (incarnation, replica) VALUES ($1, $2) ON CONFLICT (incarnation) DO NOTHING`,
		h.Incarnation, h.Replica)
	return err
}

func (p *Postgres) UnregisterHolder(ctx context.Context, h Holder) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		return dropIncarnation(ctx, tx, h.Incarnation)
	})
}

func dropIncarnation(ctx context.Context, tx pgx.Tx, inc string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_arming WHERE incarnation = $1`, inc); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_holders WHERE incarnation = $1`, inc)
	return err
}

func (p *Postgres) ReportArming(ctx context.Context, h Holder, k BudgetKey, pre int64) (bool, error) {
	active := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		b, ok, err := lockedBudget(ctx, tx, k, "UPDATE")
		if err != nil || !ok || !b.Arming {
			active = err == nil
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO llmflow_budget_arming (kind, name, model, unit, incarnation, pre_nano) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (kind, name, model, unit, incarnation) DO UPDATE SET pre_nano = EXCLUDED.pre_nano`,
			append(keyArgs(k), h.Incarnation, max(pre, 0))...); err != nil {
			return err
		}
		total, ready, err := armingPre(ctx, tx, b)
		if err != nil || !ready || total > 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE llmflow_budgets_v2 SET arming = false WHERE `+keyWhere, keyArgs(k)...); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM llmflow_budget_arming WHERE `+keyWhere, keyArgs(k)...)
		active = err == nil
		return err
	})
	return active, err
}

func (p *Postgres) ReleaseReplica(ctx context.Context, replica string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_arming WHERE incarnation IN (SELECT incarnation FROM llmflow_budget_holders WHERE replica = $1)`, replica); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_holders WHERE replica = $1`, replica)
		return err
	})
}

func (p *Postgres) IncreaseBudgetChecked(ctx context.Context, k BudgetKey, amount float64, expiry, now time.Time, lendFor time.Duration, force bool) error {
	if !validAmount(amount) {
		return fmt.Errorf("amount %v: want 0..1e15", amount)
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := lockUsage(ctx, tx); err != nil {
			return err
		}
		cur, exists, err := lockedBudget(ctx, tx, k, "UPDATE")
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		nb := cur
		e := expiry.UTC()
		nb.TempIncrease, nb.TempExpiry = amount, &e
		st, err := lockLedger(ctx, tx, k, nb.Period, nb.Period.Start(now), now)
		if err != nil {
			return err
		}
		if _, err := checkChange(&cur, nb, st, now, lendFor, force); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE llmflow_budgets_v2 SET temp_increase = $5, temp_expiry = $6, limit_epoch = $7 WHERE `+keyWhere,
			append(keyArgs(k), amount, e, nextEpoch(cur.Epoch, now))...)
		return err
	})
}

func sortRequests(reqs []GrantRequest) []GrantRequest {
	out := append([]GrantRequest(nil), reqs...)
	sort.SliceStable(out, func(i, j int) bool { return ledgerLess(out[i].Key, out[i].Start, out[j].Key, out[j].Start) })
	return out
}

func (p *Postgres) GrowGrants(ctx context.Context, h Holder, now time.Time, lendFor time.Duration, reqs []GrantRequest) ([]GrantResult, error) {
	sorted := sortRequests(reqs)
	var out []GrantResult
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		out = out[:0]
		// A ledger row to create (a new period): the usage inserts wait
		// (lockUsage), taken before any row lock.
		missing := false
		for _, r := range sorted {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5`, append(keyArgs(r.Key), r.Start.UTC())...).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				missing = true
				break
			}
			if err != nil {
				return err
			}
		}
		if missing {
			if err := lockUsage(ctx, tx); err != nil {
				return err
			}
		}
		for _, r := range sorted {
			res := GrantResult{Key: r.Key, Start: r.Start}
			b, ok, err := lockedBudget(ctx, tx, r.Key, "SHARE")
			if err != nil {
				return err
			}
			if !ok {
				res.NoBudget = true
				out = append(out, res)
				continue
			}
			pre, ready, err := armingPre(ctx, tx, b)
			if err != nil {
				return err
			}
			if !ready {
				return &NoRoomError{Key: r.Key, Arming: true}
			}
			if _, rolling := b.Period.Rolling(); rolling != rollingRow(r.Start) {
				// The period changed between a calendar one and a rolling
				// window since the replica read it: refused until it reloads.
				return &NoRoomError{Key: r.Key}
			}
			st, err := lockLedger(ctx, tx, r.Key, b.Period, r.Start, now)
			if err != nil {
				return err
			}
			limit, until := LimitNano(b, now, lendFor)
			room := limit - st.Spent - st.Granted - pre
			if room < r.Need || r.Need < 0 {
				nr := &NoRoomError{Key: r.Key, Room: max(room, 0)}
				if d, ok := b.Period.Rolling(); ok && rollingRow(r.Start) {
					bs, err := windowBuckets(ctx, tx, r.Key, d, now)
					if err != nil {
						return err
					}
					nr.Reset = rollingReset(bs, now, d, r.Need-room)
				}
				return nr
			}
			res.Grown = r.growth(room)
			res.Room, res.Epoch, res.Until = room-res.Grown, b.Epoch, until
			if res.Grown > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO llmflow_budget_grants (kind, name, model, unit, period_start, replica, incarnation, granted_nano)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (kind, name, model, unit, period_start, replica, incarnation)
					DO UPDATE SET granted_nano = llmflow_budget_grants.granted_nano + EXCLUDED.granted_nano`,
					append(keyArgs(r.Key), r.Start.UTC(), h.Replica, h.Incarnation, res.Grown)...); err != nil {
					return err
				}
			}
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decGrant lowers a holder's grant by amount (never below 0) and drops an
// empty one; its ledger row must be held.
func decGrant(ctx context.Context, tx pgx.Tx, k BudgetKey, start time.Time, h Holder, amount int64) error {
	args := append(keyArgs(k), start.UTC(), h.Replica, h.Incarnation, amount)
	if _, err := tx.Exec(ctx, `UPDATE llmflow_budget_grants SET granted_nano = GREATEST(granted_nano - $8, 0)
		WHERE `+keyWhere+` AND period_start = $5 AND replica = $6 AND incarnation = $7`, args...); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM llmflow_budget_grants WHERE `+keyWhere+` AND period_start = $5 AND replica = $6 AND incarnation = $7 AND granted_nano = 0`,
		args[:7]...)
	return err
}

// holdLedger locks an existing ledger row; ok is false when there is none.
func holdLedger(ctx context.Context, tx pgx.Tx, k BudgetKey, start time.Time) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5 FOR UPDATE`, append(keyArgs(k), start.UTC())...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (p *Postgres) ReturnGrants(ctx context.Context, h Holder, rets []GrantReturn) error {
	sorted := append([]GrantReturn(nil), rets...)
	sort.SliceStable(sorted, func(i, j int) bool { return ledgerLess(sorted[i].Key, sorted[i].Start, sorted[j].Key, sorted[j].Start) })
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, r := range sorted {
			if ok, err := holdLedger(ctx, tx, r.Key, r.Start); err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if err := decGrant(ctx, tx, r.Key, r.Start, h, max(r.Amount, 0)); err != nil {
				return err
			}
		}
		return nil
	})
}

// releaseRows drops grant rows, their ledger rows held in canonical order.
func releaseRows(ctx context.Context, tx pgx.Tx, rows []GrantRow) (int64, error) {
	sortGrants(rows)
	var n int64
	for i, r := range rows {
		if i == 0 || r.Key != rows[i-1].Key || !r.Start.Equal(rows[i-1].Start) {
			if _, err := holdLedger(ctx, tx, r.Key, r.Start); err != nil {
				return 0, err
			}
		}
		var g int64
		err := tx.QueryRow(ctx, `DELETE FROM llmflow_budget_grants WHERE `+keyWhere+` AND period_start = $5 AND replica = $6 AND incarnation = $7 RETURNING granted_nano`,
			append(keyArgs(r.Key), r.Start.UTC(), r.Holder.Replica, r.Holder.Incarnation)...).Scan(&g)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		n += g
	}
	return n, nil
}

func scanGrants(rows pgx.Rows) ([]GrantRow, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GrantRow, error) {
		var g GrantRow
		var kind string
		err := r.Scan(&kind, &g.Key.Name, &g.Key.Model, &g.Key.Unit, &g.Start, &g.Holder.Replica, &g.Holder.Incarnation, &g.Granted)
		g.Key.Kind, g.Start = ScopeKind(kind), g.Start.UTC()
		return g, err
	})
}

const grantCols = `kind, name, model, unit, period_start, replica, incarnation, granted_nano`

func (p *Postgres) ReleaseIncarnation(ctx context.Context, inc string) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `SELECT `+grantCols+` FROM llmflow_budget_grants WHERE incarnation = $1`, inc)
		if err != nil {
			return err
		}
		rows, err := scanGrants(rs)
		if err != nil {
			return err
		}
		if n, err = releaseRows(ctx, tx, rows); err != nil {
			return err
		}
		// Its spool replayed, the process has nothing unsettled left: its
		// registration and arming reports go too.
		return dropIncarnation(ctx, tx, inc)
	})
	return n, err
}

func (p *Postgres) Grants(ctx context.Context) ([]GrantRow, error) {
	rs, err := p.pool.Query(ctx, `SELECT `+grantCols+` FROM llmflow_budget_grants`)
	if err != nil {
		return nil, err
	}
	out, err := scanGrants(rs)
	sortGrants(out)
	return out, err
}

func (p *Postgres) ReleaseGrants(ctx context.Context, rows []GrantRow) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		n, err = releaseRows(ctx, tx, append([]GrantRow(nil), rows...))
		return err
	})
	return n, err
}

func (p *Postgres) Ledger(ctx context.Context, k BudgetKey, start time.Time) (LedgerState, error) {
	return p.LedgerAt(ctx, k, start, time.Now())
}

func (p *Postgres) LedgerAt(ctx context.Context, k BudgetKey, start, now time.Time) (LedgerState, error) {
	var st LedgerState
	args := append(keyArgs(k), start.UTC())
	err := p.pool.QueryRow(ctx, `SELECT spent_nano, (SELECT coalesce(sum(granted_nano), 0)::bigint FROM llmflow_budget_grants g
		WHERE g.kind = l.kind AND g.name = l.name AND g.model = l.model AND g.unit = l.unit AND g.period_start = l.period_start)
		FROM llmflow_budget_ledger l WHERE `+keyWhere+` AND period_start = $5`, args...).Scan(&st.Spent, &st.Granted)
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerState{}, nil
	}
	if err != nil || !rollingRow(start) {
		return st, err
	}
	// A rolling window: its buckets, under the budget's window.
	st.Spent = 0
	var period string
	err = p.pool.QueryRow(ctx, `SELECT period FROM llmflow_budgets_v2 WHERE `+keyWhere, keyArgs(k)...).Scan(&period)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	d, ok := Period(period).Rolling()
	if !ok {
		return st, nil
	}
	err = p.pool.QueryRow(ctx, `SELECT LEAST(coalesce(sum(spent_nano), 0), $6)::bigint FROM llmflow_budget_buckets WHERE `+keyWhere+` AND bucket_start >= $5`,
		append(keyArgs(k), WindowStart(now, d), MaxLedgerNano)...).Scan(&st.Spent)
	return st, err
}

// AdmittedCost is a usage row's admission time and cost in nano-units.
type AdmittedCost struct {
	At       time.Time
	CostNano int64
	// Ended is the row's end (ts): what a rolling window counts it by.
	Ended time.Time
}

// UsageByAdmission lists every usage row's admission time (its end for a
// row written before hard budgets), end and cost, for verifying the ledger
// against the records (tests, audits).
func (p *Postgres) UsageByAdmission(ctx context.Context) ([]AdmittedCost, error) {
	rows, err := p.pool.Query(ctx, `SELECT coalesce(admitted_at, ts), coalesce(cost_nano, 0), ts FROM llmflow_usage`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdmittedCost, error) {
		var a AdmittedCost
		err := r.Scan(&a.At, &a.CostNano, &a.Ended)
		return a, err
	})
}

// settlement is what a batch of stored records does to the ledger.
type settlement struct {
	spent map[ledgerKey]int64
	dec   map[ledgerKey]map[Holder]int64
	// buckets: a rolling row's spent by bucket (unix seconds).
	buckets map[ledgerKey]map[int64]int64
}

func newSettlement() settlement {
	return settlement{spent: map[ledgerKey]int64{}, dec: map[ledgerKey]map[Holder]int64{}, buckets: map[ledgerKey]map[int64]int64{}}
}

// addSpent counts actual for a row: its spent, or a rolling row's bucket of
// the record's admission.
func (s *settlement) addSpent(lk ledgerKey, r Record, actual int64) {
	s.spent[lk] = min(s.spent[lk]+actual, MaxLedgerNano)
	if lk.start == RollingStart.UnixNano() {
		if s.buckets[lk] == nil {
			s.buckets[lk] = map[int64]int64{}
		}
		b := BucketOf(bucketAt(r)).Unix()
		s.buckets[lk][b] = min(s.buckets[lk][b]+actual, MaxLedgerNano)
	}
}

func (s *settlement) add(r Record, budgets map[BudgetKey]Period) {
	actual := RecordCostNano(r)
	if actual > 0 && len(budgets) > 0 {
		for _, u := range unheldSettlements(r, func(k BudgetKey) (Period, bool) { p, ok := budgets[k]; return p, ok }) {
			s.addSpent(ledgerKey{u.Key, u.Start.UnixNano()}, r, actual)
		}
	}
	if len(r.Holds) == 0 {
		return
	}
	h := Holder{r.HoldReplica, r.HoldIncarnation}
	for _, hr := range r.Holds {
		lk := ledgerKey{hr.Key, hr.Start.UnixNano()}
		s.addSpent(lk, r, actual)
		if s.dec[lk] == nil {
			s.dec[lk] = map[Holder]int64{}
		}
		s.dec[lk][h] = min(s.dec[lk][h]+min(actual, max(r.HoldNano, 0)), MaxLedgerNano)
	}
}

// apply settles in the canonical order, in the insert's transaction.
func (s *settlement) apply(ctx context.Context, tx pgx.Tx) error {
	keys := make([]ledgerKey, 0, len(s.spent))
	for lk := range s.spent {
		keys = append(keys, lk)
	}
	sort.Slice(keys, func(i, j int) bool {
		return ledgerLess(keys[i].k, time.Unix(0, keys[i].start), keys[j].k, time.Unix(0, keys[j].start))
	})
	for _, lk := range keys {
		start := time.Unix(0, lk.start).UTC()
		if rollingRow(start) {
			// A rolling window: its row is held while its buckets grow.
			ok, err := holdLedger(ctx, tx, lk.k, start)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			bs := make([]int64, 0, len(s.buckets[lk]))
			for b := range s.buckets[lk] {
				bs = append(bs, b)
			}
			sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
			for _, b := range bs {
				if _, err := tx.Exec(ctx, `INSERT INTO llmflow_budget_buckets (kind, name, model, unit, bucket_start, spent_nano) VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (kind, name, model, unit, bucket_start) DO UPDATE SET spent_nano = LEAST(llmflow_budget_buckets.spent_nano::numeric + EXCLUDED.spent_nano, $7)::bigint`,
					append(keyArgs(lk.k), time.Unix(b, 0).UTC(), s.buckets[lk][b], MaxLedgerNano)...); err != nil {
					return err
				}
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE llmflow_budget_ledger SET spent_nano = LEAST(spent_nano::numeric + $6, $7)::bigint
				WHERE `+keyWhere+` AND period_start = $5`, append(keyArgs(lk.k), start, s.spent[lk], MaxLedgerNano)...)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue // the budget period has no ledger (a record replayed after its rows were removed)
			}
		}
		hs := make([]Holder, 0, len(s.dec[lk]))
		for h := range s.dec[lk] {
			hs = append(hs, h)
		}
		sort.Slice(hs, func(i, j int) bool {
			return hs[i].Replica < hs[j].Replica || hs[i].Replica == hs[j].Replica && hs[i].Incarnation < hs[j].Incarnation
		})
		for _, h := range hs {
			if err := decGrant(ctx, tx, lk.k, start, h, s.dec[lk][h]); err != nil {
				return err
			}
		}
	}
	return nil
}
