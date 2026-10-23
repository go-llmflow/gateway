// llmflow-gateway is an OpenAI-compatible gateway in front of vLLM with
// fair-share scheduling, reservation with borrowing, virtual keys,
// budgets and GPU-hour accounting.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/go-llmflow/gateway/config"
	"github.com/spf13/cobra"
)

// osExit allows tests to override os.Exit.
var osExit = os.Exit

func main() {
	if err := RootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		osExit(1)
	}
}

// RootCmd returns the top-level command.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "llmflow-gateway",
		Short:        "OpenAI-compatible gateway with fair-share scheduling in front of vLLM",
		SilenceUsage: true,
	}
	root.AddCommand(versionCmd(), checkConfigCmd(), serveCmd(), exportCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version())
			return nil
		},
	}
}

func checkConfigCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "check-config",
		Short: "Validate a configuration file and the secret files it names",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			if _, err := config.ReadSecret(c.MasterKeyFile); err != nil {
				return fmt.Errorf("master_key_file: %w", err)
			}
			for _, g := range c.ModelGroups {
				if _, err := config.ReadSecret(g.APIKeyFile); err != nil {
					return fmt.Errorf("model group %q api_key_file: %w", g.Name, err)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d teams, %d pools, %d model groups\n",
				len(c.Teams), len(c.Pools), len(c.ModelGroups))
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (YAML)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
