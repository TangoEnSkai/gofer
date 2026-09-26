package main

import (
	"os"

	"golang.org/x/term"

	goferapp "github.com/TangoEnSkai/gofer/internal/app"
)

// terminal describes the standard streams, for mode detection and for
// deciding whether to print progress.
type terminal struct {
	stdinTTY   bool
	stdinPiped bool // a pipe or a regular file, whose content gofer reads
	stdoutTTY  bool
	stderrTTY  bool
}

// detectTerminal inspects the process's standard streams.
func detectTerminal() terminal {
	var piped bool
	if fi, err := os.Stdin.Stat(); err == nil {
		m := fi.Mode()
		piped = m&os.ModeNamedPipe != 0 || m.IsRegular()
	}
	return terminal{
		stdinTTY:   term.IsTerminal(int(os.Stdin.Fd())),
		stdinPiped: piped,
		stdoutTTY:  term.IsTerminal(int(os.Stdout.Fd())),
		stderrTTY:  term.IsTerminal(int(os.Stderr.Fd())),
	}
}

// detectMode picks the run mode (docs/specs/cli-modes.md §1): -p or piped
// stdin means headless; otherwise interactive when stdin and stdout are
// terminals; otherwise headless.
func detectMode(hasPrompt bool, t terminal) goferapp.Mode {
	if hasPrompt || t.stdinPiped {
		return goferapp.Headless
	}
	if t.stdinTTY && t.stdoutTTY {
		return goferapp.Interactive
	}
	return goferapp.Headless
}
