// Command gofer is a lightweight, Gemini-powered agent for routine developer toil.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(newRootCmd(), os.Args[1:], os.Stdout, os.Stderr))
}

// run executes cmd with args and returns the process exit code. Errors are
// printed once, as "gofer: <err>", to stderr.
func run(cmd *cobra.Command, args []string, stdout, stderr io.Writer) int {
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(stderr, "gofer:", err)
		return 1
	}
	return 0
}
