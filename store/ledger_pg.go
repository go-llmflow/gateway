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
	case ScopeTag:
		scope = "$2 = ANY (tags)"
	default:
		return "", fmt.Errorf("budget kind %q", k.Kind)
	}
	return "unit = $4 AND ($3 = '' OR model = $3) AND " + scope, nil
}

// lockLedger locks a budget period's ledger row, creating it with the cost
// of the period's usage records when it does not exist, and returns its
// spent and the sum of its grants.
func lockLedger(ctx context.Context, tx pgx.Tx, k BudgetKey, start time.Time) (LedgerState, error) {
	var st LedgerState
	args := append(keyArgs(k), start.UTC())
	err := tx.QueryRow(ctx, `SELECT spent_nano FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5 FOR UPDATE`, args...).Scan(&st.Spent)
	if errors.Is(err, pgx.ErrNoRows) {
		match, merr := usageMatch(k)
		if merr != nil {
			return st, merr
		}
		// ts >= start lets the (ts, team) index serve: a record admitted in
		// the period ended in it or later.
		if _, err = tx.Exec(ctx, `INSERT INTO llmflow_budget_ledger (kind, name, model, unit, period_start, spent_nano)
			SELECT $1, $2, $3, $4, $5, LEAST(coalesce(sum(coalesce(cost_nano, LEAST(ceil(cost * 1e9), $6)::bigint)), 0), $6)
			FROM llmflow_usage WHERE ts >= $5 AND coalesce(admitted_at, ts) >= $5 AND `+match+`
			ON CONFLICT DO NOTHING`, append(args, MaxLedgerNano)...); err != nil {
			return st, err
		}
		err = tx.QueryRow(ctx, `SELECT spent_nano FROM llmflow_budget_ledger WHERE `+keyWhere+` AND period_start = $5 FOR UPDATE`, args...).Scan(&st.Spent)
	}
	if err != nil {
		return st, err
	}
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(granted_nano), 0)::bigint FROM llmflow_budget_grants WHERE `+keyWhere+` AND period_start = $5`, args...).Scan(&st.Granted)
	return st, err
}

// lockedBudget reads a budget row under a lock (share or update); ok is
// false when it does not exist.
func lockedBudget(ctx context.Context, tx pgx.Tx, k BudgetKey, mode string) (Budget, bool, error) {
	b := Budget{BudgetKey: k}
	var period string
	err := tx.QueryRow(ctx, `SELECT period, max_cost, soft_limit, temp_increase, temp_expiry, limit_epoch FROM llmflow_budgets_v2 WHERE `+keyWhere+` FOR `+mode,
		keyArgs(k)...).Scan(&period, &b.MaxCost, &b.SoftLimit, &b.TempIncrease, &b.TempExpiry, &b.Epoch)
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
		// A budget created by two administrators at once: the second waits.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, b.BudgetKey.String()); err != nil {
			return err
		}
		cur, exists, err := lockedBudget(ctx, tx, b.BudgetKey, "UPDATE")
		if err != nil {
			return err
		}
		var old *Budget
		b.TempIncrease, b.TempExpiry, b.Epoch = 0, nil, 0
		if exists {
			b.TempIncrease, b.TempExpiry, b.Epoch = cur.TempIncrease, cur.TempExpiry, cur.Epoch
			old = &cur
		}
		st, err := lockLedger(ctx, tx, b.BudgetKey, b.Period.Start(now))
		if err != nil {
			return err
		}
		if _, err := checkChange(old, b, st, now, lendFor, force); err != nil {
			return err
		}
		// Every change takes a new epoch (ledger.go).
		b.Epoch = nextEpoch(b.Epoch, now)
		_, err = tx.Exec(ctx, `
			INSERT INTO llmflow_budgets_v2 (kind, name, model, unit, period, max_cost, soft_limit, limit_epoch) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (kind, name, model, unit) DO UPDATE
			SET period = EXCLUDED.period, max_cost = EXCLUDED.max_cost, soft_limit = EXCLUDED.soft_limit, limit_epoch = EXCLUDED.limit_epoch`,
			string(b.Kind), b.Name, b.Model, b.Unit, string(b.Period), b.MaxCost, b.SoftLimit, b.Epoch)
		return err
	})
}

func (p *Postgres) IncreaseBudgetChecked(ctx context.Context, k BudgetKey, amount float64, expiry, now time.Time, lendFor time.Duration, force bool) error {
	if !validAmount(amount) {
		return fmt.Errorf("amount %v: want 0..1e15", amount)
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
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
		st, err := lockLedger(ctx, tx, k, nb.Period.Start(now))
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
			st, err := lockLedger(ctx, tx, r.Key, r.Start)
			if err != nil {
				return err
			}
			limit, until := LimitNano(b, now, lendFor)
			room := limit - st.Spent - st.Granted
			if room < r.Need || r.Need < 0 {
				return &NoRoomError{Key: r.Key, Room: max(room, 0)}
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
		n, err = releaseRows(ctx, tx, rows)
		return err
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
	var st LedgerState
	args := append(keyArgs(k), start.UTC())
	err := p.pool.QueryRow(ctx, `SELECT spent_nano, (SELECT coalesce(sum(granted_nano), 0)::bigint FROM llmflow_budget_grants g
		WHERE g.kind = l.kind AND g.name = l.name AND g.model = l.model AND g.unit = l.unit AND g.period_start = l.period_start)
		FROM llmflow_budget_ledger l WHERE `+keyWhere+` AND period_start = $5`, args...).Scan(&st.Spent, &st.Granted)
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerState{}, nil
	}
	return st, err
}

// AdmittedCost is a usage row's admission time and cost in nano-units.
type AdmittedCost struct {
	At       time.Time
	CostNano int64
}

// UsageByAdmission lists every usage row's admission time (its end for a
// row written before hard budgets) and cost, for verifying the ledger
// against the records (tests, audits).
func (p *Postgres) UsageByAdmission(ctx context.Context) ([]AdmittedCost, error) {
	rows, err := p.pool.Query(ctx, `SELECT coalesce(admitted_at, ts), coalesce(cost_nano, 0) FROM llmflow_usage`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdmittedCost, error) {
		var a AdmittedCost
		err := r.Scan(&a.At, &a.CostNano)
		return a, err
	})
}

// settlement is what a batch of stored records does to the ledger.
type settlement struct {
	spent map[ledgerKey]int64
	dec   map[ledgerKey]map[Holder]int64
}

func (s *settlement) add(r Record) {
	if len(r.Holds) == 0 {
		return
	}
	actual := RecordCostNano(r)
	h := Holder{r.HoldReplica, r.HoldIncarnation}
	for _, hr := range r.Holds {
		lk := ledgerKey{hr.Key, hr.Start.UnixNano()}
		s.spent[lk] = min(s.spent[lk]+actual, MaxLedgerNano)
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
		tag, err := tx.Exec(ctx, `UPDATE llmflow_budget_ledger SET spent_nano = LEAST(spent_nano::numeric + $6, $7)::bigint
			WHERE `+keyWhere+` AND period_start = $5`, append(keyArgs(lk.k), start, s.spent[lk], MaxLedgerNano)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue // the budget period has no ledger (a record replayed after its rows were removed)
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
