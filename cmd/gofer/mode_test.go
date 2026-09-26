package main

import (
	"testing"

	goferapp "github.com/TangoEnSkai/gofer/internal/app"
)

func TestDetectMode(t *testing.T) {
	const (
		interactive = goferapp.Interactive
		headless    = goferapp.Headless
	)
	tests := []struct {
		name   string
		prompt bool
		term   terminal
		want   goferapp.Mode
	}{
		{"terminal", false, terminal{stdinTTY: true, stdoutTTY: true}, interactive},
		{"terminal, stderr redirected", false, terminal{stdinTTY: true, stdoutTTY: true, stderrTTY: false}, interactive},
		{"-p on a terminal", true, terminal{stdinTTY: true, stdoutTTY: true, stderrTTY: true}, headless},
		{"piped stdin", false, terminal{stdinPiped: true, stdoutTTY: true}, headless},
		{"piped stdin with -p", true, terminal{stdinPiped: true}, headless},
		{"stdout redirected", false, terminal{stdinTTY: true}, headless},
		{"stdin from /dev/null", false, terminal{stdoutTTY: true}, headless},
		{"no terminal at all", false, terminal{}, headless},
	}
	for _, tt := range tests {
		if got := detectMode(tt.prompt, tt.term); got != tt.want {
			t.Errorf("%s: detectMode(%v, %+v) = %v, want %v", tt.name, tt.prompt, tt.term, got, tt.want)
		}
	}
}
