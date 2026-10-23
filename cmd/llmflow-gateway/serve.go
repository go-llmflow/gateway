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
	"github.com/go-llmflow/gateway/gateway"
	"github.com/go-llmflow/gateway/store"
	"github.com/spf13/cobra"
)

// openStore opens the configured store.
func openStore(ctx context.Context, c *config.Config) (store.Store, error) {
	if c.Store.Kind != "postgres" {
		return store.NewMemory(), nil
	}
	dsn, err := config.ReadSecret(c.Store.DSNFile)
	if err != nil {
		return nil, fmt.Errorf("store.dsn_file: %w", err)
	}
	return store.OpenPostgres(ctx, dsn)
}

// readSecrets reads every secret file the configuration names.
func readSecrets(c *config.Config) (gateway.Secrets, error) {
	var s gateway.Secrets
	var err error
	if s.MasterKey, err = config.ReadSecret(c.MasterKeyFile); err != nil {
		return s, fmt.Errorf("master_key_file: %w", err)
	}
	if c.MetricsTokenFile != "" {
		if s.MetricsToken, err = config.ReadSecret(c.MetricsTokenFile); err != nil {
			return s, fmt.Errorf("metrics_token_file: %w", err)
		}
	}
	if f := c.Alerts.WebhookTokenFile; f != "" {
		if s.WebhookToken, err = config.ReadSecret(f); err != nil {
			return s, fmt.Errorf("alerts webhook_token_file: %w", err)
		}
	}
	if c.Alerts.SMTP != nil && c.Alerts.SMTP.PasswordFile != "" {
		if s.SMTPPassword, err = config.ReadSecret(c.Alerts.SMTP.PasswordFile); err != nil {
			return s, fmt.Errorf("alerts smtp password_file: %w", err)
		}
	}
	s.UpstreamKeys = map[string]string{}
	for _, f := range c.KeyFiles() {
		k, err := config.ReadSecret(f)
		if err != nil {
			return s, fmt.Errorf("key_file: %w", err)
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
			srv, err := gateway.New(c, st, secrets, gateway.WithLogger(log))
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
