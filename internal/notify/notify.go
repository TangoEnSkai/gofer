// Package notify posts macOS notifications for routine runs and decides, by
// the routine's notify policy, whether a run warrants one.
package notify

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Notification is the text of a notification.
type Notification struct {
	Title    string
	Subtitle string
	Body     string
}

// Notifier posts notifications.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Noop discards notifications. Use it in tests and for the never policy.
type Noop struct{}

// Notify does nothing.
func (Noop) Notify(context.Context, Notification) error { return nil }

// osascriptPath is absolute so a binary earlier in PATH cannot stand in for it.
const osascriptPath = "/usr/bin/osascript"

// script shows a notification from its arguments. The text is passed only as
// argv, never spliced into the script, so a headline written by the model
// cannot inject AppleScript.
var script = []string{
	"-e", "on run argv",
	"-e", "display notification (item 3 of argv) with title (item 1 of argv) subtitle (item 2 of argv)",
	"-e", "end run",
}

// Maximum lengths in runes; macOS truncates long notifications anyway.
const (
	maxTitle    = 64
	maxSubtitle = 120
	maxBody     = 240
)

// OSAScript posts notifications with osascript(1). The zero value runs the
// real command; tests replace Run.
type OSAScript struct {
	// Run runs a command. Nil runs it with os/exec.
	Run func(ctx context.Context, name string, args ...string) error
}

// Notify shows n in Notification Center. Control characters are replaced by
// spaces and each field is truncated.
func (o OSAScript) Notify(ctx context.Context, n Notification) error {
	run := o.Run
	if run == nil {
		run = runCommand
	}
	// "--" ends osascript's options, so text starting with "-" stays an argument.
	args := append(slices.Clone(script), "--",
		clean(n.Title, maxTitle),
		clean(n.Subtitle, maxSubtitle),
		clean(n.Body, maxBody),
	)
	if err := run(ctx, osascriptPath, args...); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// clean makes s a single line of valid UTF-8 of at most limit runes.
func clean(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > limit {
		s = strings.TrimSpace(string(r[:limit-1])) + "…"
	}
	return s
}

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Policy is a routine's notify setting.
type Policy string

// Notify policies.
const (
	Always    Policy = "always"     // every run
	OnFailure Policy = "on-failure" // failed runs
	OnAction  Policy = "on-action"  // failed runs and runs with needs_action or stale_risk items
	Never     Policy = "never"
)

// ParsePolicy returns the policy named s.
func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(s); p {
	case Always, OnFailure, OnAction, Never:
		return p, nil
	}
	return "", fmt.Errorf("notify: invalid policy %q (want always, on-failure, on-action, or never)", s)
}

// statusFailed is the run status (runs.StatusFailed) that counts as a failure.
const statusFailed = "failed"

// ShouldNotify reports whether a run with status and per-category item counts
// warrants a notification under p. A "partial" run is not a failure.
func ShouldNotify(p Policy, status string, counts map[string]int) bool {
	failed := status == statusFailed
	switch p {
	case Always:
		return true
	case OnFailure:
		return failed
	case OnAction:
		return failed || counts["needs_action"] > 0 || counts["stale_risk"] > 0
	}
	return false
}

// categories lists the known item categories in display order with their
// singular and plural labels.
var categories = []struct{ key, one, many string }{
	{"needs_action", "needs action", "need action"},
	{"stale_risk", "stale risk", "stale risks"},
	{"waiting_on_maintainer", "waiting on maintainer", "waiting on maintainer"},
	{"ready_to_merge", "ready to merge", "ready to merge"},
	{"fyi", "FYI", "FYI"},
}

// Summary renders non-zero counts as, for example,
// "3 need action · 1 stale risk", in a fixed category order with unknown
// categories last, sorted by name. It returns "no items" when all are zero.
func Summary(counts map[string]int) string {
	var parts []string
	add := func(n int, one, many string) {
		label := many
		if n == 1 {
			label = one
		}
		parts = append(parts, strconv.Itoa(n)+" "+label)
	}
	known := make(map[string]bool, len(categories))
	for _, c := range categories {
		known[c.key] = true
		if n := counts[c.key]; n > 0 {
			add(n, c.one, c.many)
		}
	}
	var other []string
	for k, n := range counts {
		if !known[k] && n > 0 {
			other = append(other, k)
		}
	}
	slices.Sort(other)
	for _, k := range other {
		label := strings.ReplaceAll(k, "_", " ")
		add(counts[k], label, label)
	}
	if len(parts) == 0 {
		return "no items"
	}
	return strings.Join(parts, " · ")
}
