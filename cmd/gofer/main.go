// Command gofer is a lightweight, Gemini-powered agent for routine developer toil.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// Exit codes (docs/specs/cli-modes.md §3).
const (
	exitOK      = 0
	exitFailure = 1 // runtime error
	exitUsage   = 2 // usage or config error: bad flags, no API key, denied directory
	exitDenied  = 3 // a tool call needed a confirmation that could not be given
)

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
		var ce *codeError
		if errors.As(err, &ce) {
			return ce.code
		}
		return exitFailure
	}
	return exitOK
}

// codeError makes run exit with code instead of exitFailure.
type codeError struct {
	code int
	err  error
}

func (e *codeError) Error() string { return e.err.Error() }
func (e *codeError) Unwrap() error { return e.err }

// usageError marks err as a usage or config error (exit code 2).
func usageError(err error) error { return &codeError{exitUsage, err} }
