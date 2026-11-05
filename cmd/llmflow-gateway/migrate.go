package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/gateway"
	"github.com/go-llmflow/gateway/store"
	"github.com/spf13/cobra"
)

// maxAuditedRenames bounds the names written into one audit entry.
const maxAuditedRenames = 1000

func auditMigration(ctx context.Context, st store.Store, cmd *cobra.Command, action, domain string, plan []store.UserRename, extra map[string]any) error {
	actor := "cli"
	if u, err := user.Current(); err == nil && u.Username != "" {
		actor = "cli:" + u.Username
	}
	host, _ := os.Hostname()
	listed := plan
	if len(listed) > maxAuditedRenames {
		listed = listed[:maxAuditedRenames]
	}
	pairs := make([][2]string, len(listed))
	for i, r := range listed {
		pairs[i] = [2]string{r.From, r.To}
	}
	recordOnly := 0
	for _, r := range plan {
		if r.RecordsOnly {
			recordOnly++
		}
	}
	after := map[string]any{"domain": domain, "users": len(plan), "record_only_users": recordOnly, "renames": pairs, "truncated": len(listed) < len(plan), "host": host}
	for k, v := range extra {
		after[k] = v
	}
	b, _ := json.Marshal(after)
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := audit.New(st, nil, nil).Append(actx, store.AuditEntry{Kind: "system", Actor: actor, Roles: "[]",
		Action: action, Target: "users", Before: "null", After: string(b)})
	return err
}

func migrateUsersCmd() *cobra.Command {
	var path string
	var apply bool
	cmd := &cobra.Command{
		Use:   "migrate-users",
		Short: "Rename the stored unscoped users to name@domain, and everything keyed by their name",
		Long: `An account is local@domain. The users stored before scoped names are bare
names; this renames each to name@<domain> (the domain of the sso, jwt or scim
block that carries one) together with its budgets, the ledger, spend records,
keys, admin tokens, roles, identity links, profile assignments and warning
marks, in one transaction. A person who was never a stored user (a JWT-only
caller) has only spend records under the bare name: those records are renamed
too, so one person has one history. The audit log chain is not rewritten: its
entries keep the names they were written with. It is idempotent (a second run changes nothing) and
refuses to run when a new name is already in use, listing the collisions. Without
--apply it only prints the plan. A run is written to the audit log before it
starts and after it ends.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			domain, err := c.LegacyDomain()
			if err != nil {
				return err
			}
			if domain == "" {
				return errors.New("no domain is configured on the sso, jwt or scim blocks: nothing scopes the unscoped names")
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			st, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer st.Close()
			out := cmd.OutOrStdout()
			plan, collisions, err := gateway.MigrateUsers(ctx, st, domain, false)
			if err != nil {
				return err
			}
			for _, r := range plan {
				fmt.Fprintf(out, "%s -> %s\n", r.From, r.To)
			}
			for _, r := range collisions {
				fmt.Fprintf(cmd.ErrOrStderr(), "collision: %s -> %s: %s already exists\n", r.From, r.To, r.To)
			}
			if len(collisions) > 0 {
				return fmt.Errorf("%d collisions: nothing was changed", len(collisions))
			}
			if len(plan) == 0 {
				fmt.Fprintln(out, "nothing to migrate")
				return nil
			}
			if !apply {
				fmt.Fprintf(out, "%d users would be renamed to name@%s: run again with --apply\n", len(plan), domain)
				return nil
			}
			// A migration does not start without its audit entry.
			if err := auditMigration(ctx, st, cmd, "cli "+cmd.CommandPath()+" start", domain, plan, nil); err != nil {
				return fmt.Errorf("the migration needs its audit entry: %w", err)
			}
			_, collisions, err = gateway.MigrateUsers(ctx, st, domain, true)
			if err != nil {
				return err
			}
			if len(collisions) > 0 {
				for _, r := range collisions {
					fmt.Fprintf(cmd.ErrOrStderr(), "collision: %s -> %s\n", r.From, r.To)
				}
				_ = auditMigration(ctx, st, cmd, "cli "+cmd.CommandPath()+" refused", domain, collisions, map[string]any{"refused": true})
				return fmt.Errorf("%d collisions: nothing was changed", len(collisions))
			}
			if err := auditMigration(ctx, st, cmd, "cli "+cmd.CommandPath()+" done", domain, plan, map[string]any{"renamed": len(plan)}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: the end of the migration could not be audited: %v\n", err)
			}
			fmt.Fprintf(out, "renamed %d users to name@%s\n", len(plan), domain)
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	cmd.Flags().BoolVar(&apply, "apply", false, "make the renames (default: print the plan)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
