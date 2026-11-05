package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Scoped names for the users stored before v0.29.0. An account is
// local@domain; the accounts that predate it are bare names, which the
// institution that carries `domain` owns. migrate-users renames them, and
// everything keyed by their name (budgets, the ledger, spend records, keys,
// tokens, roles, links, profile assignments, warnings), in one transaction.

// PlanUserRenames is the renames that bring the stored unscoped users under
// domain: every user whose name has no "@" becomes name@domain.
func PlanUserRenames(us []store.User, domain string) []store.UserRename {
	var out []store.UserRename
	if domain == "" {
		return nil
	}
	for _, u := range us {
		if strings.Contains(u.Name, "@") {
			continue
		}
		to := u.Name + "@" + domain
		if !config.ValidUserName(to) {
			continue
		}
		out = append(out, store.UserRename{From: u.Name, To: to})
	}
	slices.SortFunc(out, func(a, b store.UserRename) int { return strings.Compare(a.From, b.From) })
	return out
}

// PlanRecordRenames is the renames of the record-only names (bare names that
// only spend records carry) under domain; a name that would not make a valid
// account name is left alone.
func PlanRecordRenames(names []string, domain string) []store.UserRename {
	var out []store.UserRename
	if domain == "" {
		return nil
	}
	for _, n := range names {
		to := n + "@" + domain
		// The synthetic person of a JWT caller with no assertable name
		// (jwt-<hash of issuer and subject>) belongs to no institution: scoping it
		// would put the people of every issuer under the legacy domain, again at
		// every run.
		if n == "" || strings.Contains(n, "@") || isSyntheticPerson(n) || !config.ValidUserName(to) {
			continue
		}
		out = append(out, store.UserRename{From: n, To: to, RecordsOnly: true})
	}
	return out
}

// MigrateUsers plans the renames and, when apply, makes them in one
// transaction. It returns the plan and the collisions: with any collision
// nothing is changed. It is idempotent: once the users are renamed, the plan is
// empty.
func MigrateUsers(ctx context.Context, st store.Store, domain string, apply bool) (plan, collisions []store.UserRename, err error) {
	us, err := st.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan = PlanUserRenames(us, domain)
	// The people who were never stored users (JWT-only callers) have only their
	// spend records under the bare name: they get the same scoped name.
	names, err := st.RecordOnlyUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan = append(plan, PlanRecordRenames(names, domain)...)
	if !apply || len(plan) == 0 {
		// A dry run still reports what would collide: a collision is one
		// more rename-in-waiting that the same transaction would refuse.
		if len(plan) > 0 && !apply {
			taken := map[string]bool{}
			for _, u := range us {
				taken[u.Name] = true
			}
			for _, r := range plan {
				if taken[r.To] {
					collisions = append(collisions, r)
				}
			}
		}
		return plan, collisions, nil
	}
	collisions, err = st.RenameUsers(ctx, plan)
	return plan, collisions, err
}

// refuseUnscopedUsers stops the gateway from starting with stored users whose
// names are bare while an institution's domain is configured: their budgets,
// spend and roles would stay under a name nobody logs in with, and a person
// logging in as name@domain would be a new person. legacy_unscoped_names = true
// keeps the read-only alias until the users are renamed.
func (s *Server) refuseUnscopedUsers(ctx context.Context) error {
	domain, err := s.cfg.LegacyDomain()
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if domain == "" || s.cfg.LegacyNames {
		return nil
	}
	us, err := s.st.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("gateway: list users: %w", err)
	}
	plan := PlanUserRenames(us, domain)
	if len(plan) == 0 {
		return nil
	}
	names := make([]string, 0, 5)
	for i, r := range plan {
		if i == 5 {
			names = append(names, "...")
			break
		}
		names = append(names, r.From)
	}
	return fmt.Errorf("gateway: %d stored users have unscoped names (%s) while domain %q is configured: run `llmflow-gateway migrate-users --config <file> --apply` to rename them to name@%s, or set legacy_unscoped_names = true to keep matching them by alias until you do", len(plan), strings.Join(names, ", "), domain, domain)
}
