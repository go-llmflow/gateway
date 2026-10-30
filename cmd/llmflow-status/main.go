// llmflow-status prints a person's soft-quota windows on one line, for Claude
// Code's statusLine (code.claude.com/docs/en/statusline.md): it reads the
// status line's JSON on stdin, asks the gateway's GET /v1/me/usage with the
// credential Claude Code itself uses (ANTHROPIC_AUTH_TOKEN, else
// ANTHROPIC_API_KEY) and prints
//
//	session 85% · 15:45 │ week 40% · Mon 00:00
//
// coloured at the gateway's thresholds. The answer is cached for --ttl so a
// status line that refreshes every few seconds does not call the gateway each
// time.
//
// It never blocks Claude Code and never talks: on any error, a timeout, an
// unreachable gateway or a gateway without profiles it prints nothing and
// exits 0 (--verbose says why on stderr). It never prints the credential, and
// the cache holds no secret: the answer, and a file name derived from a hash.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// osExit allows tests to override os.Exit.
var osExit = os.Exit

func main() { osExit(execute(RootCmd())) }

// execute runs a command. The status line must never fail loudly: every
// error is swallowed by RunE itself; what reaches here is a usage error (a
// bad flag), printed once on the error stream.
func execute(cmd *cobra.Command) int {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		return 2
	}
	return 0
}

// version is set by the release build (-ldflags "-X main.version=v0.19.0").
var version = "dev"
