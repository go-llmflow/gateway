package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/export"
	"github.com/go-llmflow/gateway/gateway"
	"github.com/go-llmflow/gateway/secret"
	"github.com/go-llmflow/gateway/store"
	"github.com/spf13/cobra"
)

// resolver returns the secret resolver of a configuration: files, and the
// OpenBao/Vault server when a vault block is present.
func resolver(c *config.Config) (*secret.Resolver, error) {
	if c.Vault == nil {
		return secret.New(nil)
	}
	file, _ := secret.New(nil)
	tok, err := file.Resolve(context.Background(), c.Vault.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("vault token_file: %w", err)
	}
	return secret.New(&secret.VaultConfig{Address: c.Vault.Address, Token: tok, CAFile: c.Vault.CAFile,
		Namespace: c.Vault.Namespace, CacheTTL: c.Vault.CacheTTL})
}

// openStore opens the configured store.
func openStore(ctx context.Context, c *config.Config) (store.Store, error) {
	if c.Store.Kind != "postgres" {
		return store.NewMemory(), nil
	}
	res, err := resolver(c)
	if err != nil {
		return nil, err
	}
	dsn, err := res.Resolve(ctx, c.Store.DSNFile)
	if err != nil {
		return nil, fmt.Errorf("store.dsn_file: %w", err)
	}
	return store.OpenPostgres(ctx, dsn)
}

// readSecrets resolves every secret reference the configuration names.
func readSecrets(c *config.Config) (gateway.Secrets, error) {
	var s gateway.Secrets
	res, err := resolver(c)
	if err != nil {
		return s, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get := func(name, ref string, dst *string) error {
		if ref == "" {
			return nil
		}
		v, err := res.Resolve(ctx, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*dst = v
		return nil
	}
	if err := get("master_key_file", c.MasterKeyFile, &s.MasterKey); err != nil {
		return s, err
	}
	if err := get("metrics_token_file", c.MetricsTokenFile, &s.MetricsToken); err != nil {
		return s, err
	}
	if err := get("cache_salt_key_file", c.CacheSaltKeyFile, &s.CacheSaltKey); err != nil {
		return s, err
	}
	if err := get("alerts webhook_token_file", c.Alerts.WebhookTokenFile, &s.WebhookToken); err != nil {
		return s, err
	}
	if c.SSO != nil {
		if err := get("sso client_secret_file", c.SSO.ClientSecretFile, &s.SSOClientSecret); err != nil {
			return s, err
		}
	}
	if c.SCIM != nil {
		if err := get("scim token_file", c.SCIM.TokenFile, &s.SCIMToken); err != nil {
			return s, err
		}
	}
	if c.Alerts.SMTP != nil {
		if err := get("alerts smtp password_file", c.Alerts.SMTP.PasswordFile, &s.SMTPPassword); err != nil {
			return s, err
		}
	}
	if err := get("audit signing_key_file", c.Audit.SigningKeyFile, &s.AuditKey); err != nil {
		return s, err
	}
	s.ExportCredentials = map[string]export.Credentials{}
	for _, e := range c.Exports {
		var cr export.Credentials
		if err := get("export "+e.Name+" access_key_file", e.AccessKeyFile, &cr.AccessKey); err != nil {
			return s, err
		}
		if err := get("export "+e.Name+" secret_key_file", e.SecretKeyFile, &cr.SecretKey); err != nil {
			return s, err
		}
		s.ExportCredentials[e.Name] = cr
	}
	s.UpstreamKeys = map[string]string{}
	for _, f := range c.KeyFiles() {
		var k string
		if err := get("key_file", f, &k); err != nil {
			return s, err
		}
		s.UpstreamKeys[f] = k
	}
	return s, nil
}

// signalContext is replaceable by tests.
var signalContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func serveCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			secrets, err := readSecrets(c)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			ctx, stop := signalContext()
			defer stop()
			st, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer st.Close()
			opts := []gateway.Option{gateway.WithLogger(log)}
			if c.KeyRotation != nil {
				res, err := resolver(c)
				if err != nil {
					return err
				}
				opts = append(opts, gateway.WithSecretWriter(res))
			}
			srv, err := gateway.New(c, st, secrets, opts...)
			if err != nil {
				return err
			}
			if err := srv.Start(ctx); err != nil {
				return err
			}
			hs := srv.HTTPServer()
			ln, err := net.Listen("tcp", c.Listen)
			if err != nil {
				srv.Close(context.Background())
				return err
			}
			log.Info("listening", "addr", ln.Addr().String())
			fmt.Fprintf(cmd.OutOrStdout(), "listening on %s\n", ln.Addr())
			errc := make(chan error, 1)
			go func() { errc <- hs.Serve(ln) }()
			select {
			case err = <-errc:
			case <-ctx.Done():
				sctx, cancel := context.WithTimeout(context.Background(), c.ShutdownGrace)
				err = hs.Shutdown(sctx)
				cancel()
			}
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			srv.Close(cctx)
			cancel()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func exportCmd() *cobra.Command {
	var path, from, to, groupBy, format string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export GPU-hours and tokens for a period, grouped (for reconciliation with Slurm sreport)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			f, err := gateway.ParseDay(from)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			t, err := gateway.ParseDay(to)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			if !f.Before(t) {
				return errors.New("--from must be before --to")
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
			dims, err := gateway.ParseGroupBy(groupBy)
			if err != nil {
				return fmt.Errorf("--group-by: %w", err)
			}
			rows, err := st.Report(ctx, f, t, dims)
			if err != nil {
				return err
			}
			return gateway.WriteReport(cmd.OutOrStdout(), format, f, t, dims, rows)
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	cmd.Flags().StringVar(&from, "from", "", "start (RFC 3339 or YYYY-MM-DD, inclusive)")
	cmd.Flags().StringVar(&to, "to", "", "end (RFC 3339 or YYYY-MM-DD, exclusive)")
	cmd.Flags().StringVar(&groupBy, "group-by", "team,pool", "dimensions: org, team, project, key, model, pool, tag")
	cmd.Flags().StringVar(&format, "format", gateway.FormatCSV, "csv, json or parsable2 ('|'-delimited like sreport --parsable2)")
	for _, f := range []string{"config", "from", "to"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}
