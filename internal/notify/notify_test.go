package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeRunner records commands instead of running them, so tests never call
// the real osascript.
type fakeRunner struct {
	name string
	args []string
	err  error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) error {
	f.name, f.args = name, args
	return f.err
}

var wantScript = []string{
	"-e", "on run argv",
	"-e", "display notification (item 3 of argv) with title (item 1 of argv) subtitle (item 2 of argv)",
	"-e", "end run",
	"--",
}

func TestOSAScriptArgv(t *testing.T) {
	f := &fakeRunner{}
	n := Notification{Title: "gofer · pr-digest", Subtitle: "3 PRs need you", Body: "3 need action · 1 stale risk"}
	if err := (OSAScript{Run: f.run}).Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if f.name != "/usr/bin/osascript" {
		t.Errorf("command = %q, want /usr/bin/osascript", f.name)
	}
	want := append(slices.Clone(wantScript), n.Title, n.Subtitle, n.Body)
	if !slices.Equal(f.args, want) {
		t.Errorf("args =\n%q\nwant\n%q", f.args, want)
	}
}

func TestOSAScriptInjection(t *testing.T) {
	headlines := []string{
		`x" & (do shell script "touch /tmp/pwned") & "`,
		`"; do shell script "rm -rf ~" --`,
		`end run & do shell script "id"`,
		`-e do shell script "id"`,
		`--`,
		`item 1 of argv`,
		`\" & quoted form of "x`,
	}
	for _, h := range headlines {
		f := &fakeRunner{}
		n := Notification{Title: h, Subtitle: h, Body: h}
		if err := (OSAScript{Run: f.run}).Notify(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		// The script is fixed and the text appears verbatim, each field as one
		// argument after "--".
		want := append(slices.Clone(wantScript), h, h, h)
		if !slices.Equal(f.args, want) {
			t.Errorf("headline %q: args =\n%q\nwant\n%q", h, f.args, want)
		}
	}
}

func TestOSAScriptCleansAndTruncates(t *testing.T) {
	f := &fakeRunner{}
	n := Notification{
		Title:    strings.Repeat("t", 100),
		Subtitle: "  line1\r\nline2\x00\tend  ",
		Body:     strings.Repeat("é", 300) + "\xff",
	}
	if err := (OSAScript{Run: f.run}).Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	title, sub, body := f.args[len(wantScript)], f.args[len(wantScript)+1], f.args[len(wantScript)+2]
	if want := strings.Repeat("t", maxTitle-1) + "…"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if want := "line1  line2  end"; sub != want {
		t.Errorf("subtitle = %q, want %q", sub, want)
	}
	if utf8.RuneCountInString(body) != maxBody || !utf8.ValidString(body) || !strings.HasSuffix(body, "…") {
		t.Errorf("body = %q (%d runes), want %d valid runes ending in …", body, utf8.RuneCountInString(body), maxBody)
	}
}

func TestOSAScriptError(t *testing.T) {
	boom := errors.New("boom")
	err := (OSAScript{Run: (&fakeRunner{err: boom}).run}).Notify(context.Background(), Notification{})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapping boom", err)
	}
}

func TestNoop(t *testing.T) {
	var n Notifier = Noop{}
	if err := n.Notify(context.Background(), Notification{Title: "x"}); err != nil {
		t.Error(err)
	}
}

func TestParsePolicy(t *testing.T) {
	for _, s := range []string{"always", "on-failure", "on-action", "never"} {
		p, err := ParsePolicy(s)
		if err != nil || string(p) != s {
			t.Errorf("ParsePolicy(%q) = %q, %v", s, p, err)
		}
	}
	for _, s := range []string{"", "Always", "on_failure", "sometimes", " never"} {
		if p, err := ParsePolicy(s); err == nil {
			t.Errorf("ParsePolicy(%q) = %q, want error", s, p)
		}
	}
}

func TestShouldNotify(t *testing.T) {
	none := map[string]int{"fyi": 2, "ready_to_merge": 1}
	action := map[string]int{"needs_action": 1}
	stale := map[string]int{"stale_risk": 2, "needs_action": 0}
	tests := []struct {
		p      Policy
		status string
		counts map[string]int
		want   bool
	}{
		{Always, "ok", nil, true},
		{Always, "failed", none, true},
		{Never, "failed", action, false},
		{Never, "ok", action, false},
		{OnFailure, "failed", nil, true},
		{OnFailure, "partial", action, false},
		{OnFailure, "ok", action, false},
		{OnAction, "ok", action, true},
		{OnAction, "ok", stale, true},
		{OnAction, "partial", action, true},
		{OnAction, "failed", nil, true},
		{OnAction, "ok", none, false},
		{OnAction, "partial", none, false},
		{OnAction, "ok", nil, false},
		{Policy(""), "failed", action, false},
	}
	for _, tt := range tests {
		if got := ShouldNotify(tt.p, tt.status, tt.counts); got != tt.want {
			t.Errorf("ShouldNotify(%q, %q, %v) = %v, want %v", tt.p, tt.status, tt.counts, got, tt.want)
		}
	}
}

func TestSummary(t *testing.T) {
	tests := []struct {
		counts map[string]int
		want   string
	}{
		{map[string]int{"stale_risk": 1, "needs_action": 3}, "3 need action · 1 stale risk"},
		{
			map[string]int{"fyi": 4, "ready_to_merge": 1, "waiting_on_maintainer": 2, "stale_risk": 2, "needs_action": 1},
			"1 needs action · 2 stale risks · 2 waiting on maintainer · 1 ready to merge · 4 FYI",
		},
		{map[string]int{"zeta": 1, "fyi": 1, "alpha_beta": 2, "needs_action": 0}, "1 FYI · 2 alpha beta · 1 zeta"},
		{map[string]int{"needs_action": 0, "fyi": -1}, "no items"},
		{nil, "no items"},
	}
	for _, tt := range tests {
		// Map order is random; repeat to catch unstable output.
		for range 20 {
			if got := Summary(tt.counts); got != tt.want {
				t.Fatalf("Summary(%v) = %q, want %q", tt.counts, got, tt.want)
			}
		}
	}
}
