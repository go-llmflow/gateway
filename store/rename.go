package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Renaming users. An account is local@domain: the users stored before scoped
// names (v0.29.0) are bare names, and every table that keys something by the
// person's name has to follow, or their budgets, spend and keys stay under a
// name nobody logs in with any more.

// UserRename is one user's new name. RecordsOnly marks a person who was never a
// stored user (a JWT-only caller): only their spend records carry the name, and
// only those are renamed.
type UserRename struct {
	From, To    string
	RecordsOnly bool
}

// MaxRecordOnlyUsers bounds the names RecordOnlyUsers returns: more is an error,
// not a truncated plan.
const MaxRecordOnlyUsers = 100000

// RecordOnlyUsers lists the bare names (no "@") that spend records carry and
// that are not stored users, sorted. PostgreSQL answers it with one grouped
// query; the records are not loaded.
func (m *Memory) RecordOnlyUsers(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.usersInit()
	seen := map[string]bool{}
	for i := range m.records {
		n := m.records[i].User
		if n == "" || strings.Contains(n, "@") || seen[n] {
			continue
		}
		if _, stored := m.u.users[n]; !stored {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	if len(out) > MaxRecordOnlyUsers {
		return nil, errTooManyRecordOnly
	}
	return out, nil
}

var errTooManyRecordOnly = fmt.Errorf("store: more than %d record-only user names", MaxRecordOnlyUsers)

// RecordOnlyUsers: see Memory.RecordOnlyUsers.
func (p *Postgres) RecordOnlyUsers(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT u.user_name FROM llmflow_usage u
		WHERE u.user_name <> '' AND position('@' in u.user_name) = 0
		AND NOT EXISTS (SELECT 1 FROM llmflow_users s WHERE s.name = u.user_name)
		GROUP BY u.user_name ORDER BY u.user_name LIMIT $1`, MaxRecordOnlyUsers+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > MaxRecordOnlyUsers {
		return nil, errTooManyRecordOnly
	}
	return out, nil
}

// userScoped reports whether a budget scope's Name is a person's.
func userScoped(k ScopeKind) bool { return k == ScopeUser || k.Profile() }

// RenameUsers renames stored users, and everything keyed by their name, in one
// transaction. A rename whose From is not a stored user is skipped (so a
// second run does nothing). If any To is already in use (a stored user, a
// link, a budget, ledger or profile row, a warning mark) or two renames share a
// To, nothing is changed and the colliding renames are returned.
func (m *Memory) RenameUsers(_ context.Context, rs []UserRename) ([]UserRename, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.usersInit()
	var todo, coll []UserRename
	targets := map[string]bool{}
	var present map[string]bool // names the records carry, read once for the record-only renames
	for _, r := range rs {
		if r.From == r.To {
			continue
		}
		_, stored := m.u.users[r.From]
		if r.RecordsOnly {
			if stored { // the stored user's own rename covers the records
				continue
			}
			if present == nil {
				present = map[string]bool{}
				for i := range m.records {
					present[m.records[i].User] = true
				}
			}
			if !present[r.From] {
				continue
			}
		} else if !stored {
			continue
		}
		if targets[r.To] || m.userNameTakenLocked(r.To) {
			coll = append(coll, r)
			continue
		}
		targets[r.To] = true
		todo = append(todo, r)
	}
	if len(coll) > 0 {
		return coll, nil
	}
	for _, r := range todo {
		if r.RecordsOnly {
			m.renameKeyedLocked(r.From, r.To)
			continue
		}
		m.renameLocked(r.From, r.To)
	}
	return nil, nil
}

func (m *Memory) userNameTakenLocked(n string) bool {
	if _, ok := m.u.users[n]; ok {
		return true
	}
	if _, ok := m.u.idps[n]; ok {
		return true
	}
	for k := range m.budgets {
		if userScoped(k.Kind) && k.Name == n {
			return true
		}
	}
	for k := range m.ledger {
		if userScoped(k.k.Kind) && k.k.Name == n {
			return true
		}
	}
	for k := range m.arming {
		if userScoped(k.Kind) && k.Name == n {
			return true
		}
	}
	for _, a := range m.prof.hist {
		if a.User == n {
			return true
		}
	}
	for k := range m.warn.marks {
		if k[0] == n {
			return true
		}
	}
	return false
}

func (m *Memory) renameLocked(from, to string) {
	u := m.u.users[from]
	delete(m.u.users, from)
	u.Name = to
	m.u.users[to] = u
	for _, t := range m.u.tokens {
		if t.User == from {
			t.User = to
		}
	}
	bs := make(map[Binding]Provenance, len(m.u.bindings))
	for b, p := range m.u.bindings {
		if b.User == from {
			b.User = to
		}
		bs[b] = p
	}
	m.u.bindings = bs
	if l, ok := m.u.idps[from]; ok {
		delete(m.u.idps, from)
		m.u.idps[to] = l
	}
	for _, k := range m.keys {
		if k.Owner == from {
			k.Owner = to
		}
	}
	m.renameKeyedLocked(from, to)
}

// renameKeyedLocked renames what is keyed by a person's name without a stored
// user row: spend records, budgets, the ledger, profile assignments, warnings.
func (m *Memory) renameKeyedLocked(from, to string) {
	for i := range m.records {
		if m.records[i].User == from {
			m.records[i].User = to
		}
	}
	bk := func(k BudgetKey) BudgetKey {
		if userScoped(k.Kind) && k.Name == from {
			k.Name = to
		}
		return k
	}
	budgets := make(map[BudgetKey]*memBudget, len(m.budgets))
	for k, v := range m.budgets {
		nk := bk(k)
		v.b.BudgetKey = nk
		budgets[nk] = v
	}
	m.budgets = budgets
	ledger := make(map[ledgerKey]*memLedger, len(m.ledger))
	for k, v := range m.ledger {
		k.k = bk(k.k)
		ledger[k] = v
	}
	m.ledger = ledger
	arming := make(map[BudgetKey]map[string]int64, len(m.arming))
	for k, v := range m.arming {
		arming[bk(k)] = v
	}
	m.arming = arming
	for i := range m.prof.hist {
		if m.prof.hist[i].User == from {
			m.prof.hist[i].User = to
		}
	}
	marks := make(map[[3]string]*memMark, len(m.warn.marks))
	for k, v := range m.warn.marks {
		if k[0] == from {
			k[0] = to
		}
		marks[k] = v
	}
	m.warn.marks = marks
	for i := range m.quota.events {
		if m.quota.events[i].user == from {
			m.quota.events[i].user = to
		}
	}
}

// userBudgetKinds are the budget scopes whose Name is a person.
const userBudgetKinds = `('user', 'profile_week', 'profile_session', 'profile_overflow')`

// RenameUsers: see Memory.RenameUsers. One PostgreSQL transaction.
func (p *Postgres) RenameUsers(ctx context.Context, rs []UserRename) ([]UserRename, error) {
	var coll []UserRename
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var todo, recOnly []UserRename
		targets := map[string]bool{}
		for _, r := range rs {
			if r.From == r.To {
				continue
			}
			if r.RecordsOnly {
				recOnly = append(recOnly, r)
				continue
			}
			var has bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_users WHERE name = $1)`, r.From).Scan(&has); err != nil {
				return err
			}
			if !has {
				continue
			}
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_users WHERE name = $1)
				OR EXISTS (SELECT 1 FROM llmflow_user_idps WHERE username = $1)
				OR EXISTS (SELECT 1 FROM llmflow_budgets_v2 WHERE name = $1 AND kind IN `+userBudgetKinds+`)
				OR EXISTS (SELECT 1 FROM llmflow_budget_ledger WHERE name = $1 AND kind IN `+userBudgetKinds+`)
				OR EXISTS (SELECT 1 FROM llmflow_budget_grants WHERE name = $1 AND kind IN `+userBudgetKinds+`)
				OR EXISTS (SELECT 1 FROM llmflow_budget_arming WHERE name = $1 AND kind IN `+userBudgetKinds+`)
				OR EXISTS (SELECT 1 FROM llmflow_budget_buckets WHERE name = $1 AND kind IN `+userBudgetKinds+`)
				OR EXISTS (SELECT 1 FROM llmflow_profile_assignments WHERE user_name = $1)
				OR EXISTS (SELECT 1 FROM llmflow_warn_marks WHERE user_name = $1)`, r.To).Scan(&taken); err != nil {
				return err
			}
			if taken || targets[r.To] {
				coll = append(coll, r)
				continue
			}
			targets[r.To] = true
			todo = append(todo, r)
		}
		// Record-only names: set-based, three queries whatever their number.
		var recTodo []UserRename
		if len(recOnly) > 0 {
			var from, to []string
			for _, r := range recOnly {
				from, to = append(from, r.From), append(to, r.To)
			}
			stored, err := nameSet(ctx, tx, `SELECT name FROM llmflow_users WHERE name = ANY($1)`, from)
			if err != nil {
				return err
			}
			present, err := nameSet(ctx, tx, `SELECT user_name FROM llmflow_usage WHERE user_name = ANY($1) GROUP BY user_name`, from)
			if err != nil {
				return err
			}
			taken, err := nameSet(ctx, tx, `SELECT n FROM (
				SELECT name AS n FROM llmflow_users WHERE name = ANY($1)
				UNION SELECT username FROM llmflow_user_idps WHERE username = ANY($1)
				UNION SELECT name FROM llmflow_budgets_v2 WHERE name = ANY($1) AND kind IN `+userBudgetKinds+`
				UNION SELECT name FROM llmflow_budget_ledger WHERE name = ANY($1) AND kind IN `+userBudgetKinds+`
				UNION SELECT name FROM llmflow_budget_grants WHERE name = ANY($1) AND kind IN `+userBudgetKinds+`
				UNION SELECT name FROM llmflow_budget_arming WHERE name = ANY($1) AND kind IN `+userBudgetKinds+`
				UNION SELECT name FROM llmflow_budget_buckets WHERE name = ANY($1) AND kind IN `+userBudgetKinds+`
				UNION SELECT user_name FROM llmflow_profile_assignments WHERE user_name = ANY($1)
				UNION SELECT user_name FROM llmflow_warn_marks WHERE user_name = ANY($1)) t`, to)
			if err != nil {
				return err
			}
			for _, r := range recOnly {
				if stored[r.From] || !present[r.From] {
					continue
				}
				if taken[r.To] || targets[r.To] {
					coll = append(coll, r)
					continue
				}
				targets[r.To] = true
				recTodo = append(recTodo, r)
			}
		}
		if len(coll) > 0 {
			return nil // nothing is changed; the transaction commits nothing
		}
		if len(recTodo) > 0 {
			var from, to []string
			for _, r := range recTodo {
				from, to = append(from, r.From), append(to, r.To)
			}
			// Everything keyed by the name but a stored user's own rows, set-based.
			for _, q := range []string{
				`UPDATE llmflow_usage SET user_name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE user_name = m.f`,
				`UPDATE llmflow_budgets_v2 SET name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE name = m.f AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_ledger SET name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE name = m.f AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_grants SET name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE name = m.f AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_arming SET name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE name = m.f AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_buckets SET name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE name = m.f AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_profile_assignments SET user_name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE user_name = m.f`,
				`UPDATE llmflow_warn_marks SET user_name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE user_name = m.f`,
				`UPDATE llmflow_warn_events SET user_name = m.t FROM unnest($1::text[], $2::text[]) AS m(f, t) WHERE user_name = m.f`,
			} {
				if _, err := tx.Exec(ctx, q, from, to); err != nil {
					return err
				}
			}
		}
		for _, r := range todo {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_users (name, created_at, disabled, origin, disabled_by)
				SELECT $2, created_at, disabled, origin, disabled_by FROM llmflow_users WHERE name = $1`, r.From, r.To); err != nil {
				return err
			}
			for _, q := range []string{
				`UPDATE llmflow_user_tokens SET username = $2 WHERE username = $1`,
				`UPDATE llmflow_bindings SET username = $2 WHERE username = $1`,
				`UPDATE llmflow_user_idps SET username = $2 WHERE username = $1`,
				`UPDATE llmflow_keys SET owner = $2 WHERE owner = $1`,
				`UPDATE llmflow_usage SET user_name = $2 WHERE user_name = $1`,
				`UPDATE llmflow_budgets_v2 SET name = $2 WHERE name = $1 AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_ledger SET name = $2 WHERE name = $1 AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_grants SET name = $2 WHERE name = $1 AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_arming SET name = $2 WHERE name = $1 AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_budget_buckets SET name = $2 WHERE name = $1 AND kind IN ` + userBudgetKinds,
				`UPDATE llmflow_profile_assignments SET user_name = $2 WHERE user_name = $1`,
				`UPDATE llmflow_warn_marks SET user_name = $2 WHERE user_name = $1`,
				`UPDATE llmflow_warn_events SET user_name = $2 WHERE user_name = $1`,
			} {
				if _, err := tx.Exec(ctx, q, r.From, r.To); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM llmflow_users WHERE name = $1`, r.From); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return coll, nil
}

// nameSet runs a query that returns one name per row for a list of names.
func nameSet(ctx context.Context, tx pgx.Tx, q string, names []string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, q, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}
