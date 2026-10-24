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
	osExit(execute(RootCmd()))
}

// execute runs a command and prints its error exactly once, through the
// command's error stream. Cobra's own "Error:" line is silenced; without
// that every error appeared twice.
func execute(cmd *cobra.Command) int {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		return 1
	}
	return 0
}

// RootCmd returns the top-level command.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "llmflow-gateway",
		Short:         "OpenAI-compatible gateway with fair-share scheduling in front of vLLM",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(versionCmd(), checkConfigCmd(), serveCmd(), exportCmd(), auditCmd())
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
			// A configuration error lists every diagnostic as
			// file:line:column: summary: detail, one per line.
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			if _, err := readSecrets(c); err != nil {
				return err
			}
			teams := 0
			for _, o := range c.Orgs {
				teams += len(o.Teams)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d orgs, %d teams, %d pools, %d models\n",
				len(c.Orgs), teams, len(c.Pools), len(c.ModelGroups))
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
