package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Renaming users. An account is local@domain: the users stored before scoped
// names (v0.29.0) are bare names, and every table that keys something by the
// person's name has to follow, or their budgets, spend and keys stay under a
// name nobody logs in with any more.

// UserRename is one user's new name.
type UserRename struct{ From, To string }

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
	for _, r := range rs {
		if r.From == r.To {
			continue
		}
		if _, ok := m.u.users[r.From]; !ok {
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
		var todo []UserRename
		targets := map[string]bool{}
		for _, r := range rs {
			if r.From == r.To {
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
		if len(coll) > 0 {
			return nil // nothing is changed; the transaction commits nothing
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
