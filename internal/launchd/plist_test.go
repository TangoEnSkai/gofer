package launchd

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata")

const plutil = "/usr/bin/plutil"

// prDigest is the S1 routine as `gofer routine add` will install it.
func prDigest(t *testing.T) Job {
	t.Helper()
	schedule, err := ParseSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	return Job{
		Name:       "pr-digest",
		Executable: "/Users/me/go/bin/gofer",
		Args:       []string{"routine", "run", "pr-digest"},
		Schedule:   schedule,
		LogPath:    "/Users/me/.local/state/gofer/logs/pr-digest.log",
	}
}

func TestPlistGolden(t *testing.T) {
	got, err := prDigest(t).Plist()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "pr-digest.plist")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Plist() differs from %s (run go test -update to accept):\n%s", golden, got)
	}
	checkWellFormed(t, got)
	plutilLint(t, got)
}

// tricky exercises escaping: XML metacharacters, quotes, whitespace control
// characters, non-ASCII text, and spaces in paths.
func tricky() Job {
	one, nine := 1, 9
	return Job{
		Name:       "tricky-1",
		Executable: "/Users/me/My Tools/gofer & co/<gofer>",
		Args:       []string{"routine", "run", `a "quoted" arg`, "it's", "x<y&z>w", "]]>", "line1\nline2", "tab\there", "cr\rhere", "日本語 ✓ 🚀", ""},
		Schedule:   []CalendarInterval{{Hour: &nine, Weekday: &one}, {}},
		LogPath:    "/Users/me/Library/Application Support/gofer/logs/tricky & <1>.log",
		Env:        map[string]string{"PATH": "/custom/bin:/bin", "GOFER_NOTE": `<b>"hi" & 'bye'</b>`},
	}
}

func TestPlistEscapesStrings(t *testing.T) {
	job := tricky()
	data, err := job.Plist()
	if err != nil {
		t.Fatal(err)
	}
	checkWellFormed(t, data)

	// Every string and key must decode to exactly the original text.
	var got []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	var inText bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			inText = tok.Name.Local == "string" || tok.Name.Local == "key"
			if inText {
				got = append(got, "")
			}
		case xml.CharData:
			if inText {
				got[len(got)-1] += string(tok)
			}
		case xml.EndElement:
			inText = false
		}
	}
	for _, s := range append(job.texts(), "Background", "dev.gofer.tricky-1") {
		if !slices.Contains(got, s) {
			t.Errorf("plist does not round-trip %q; decoded strings: %q", s, got)
		}
	}
	if slices.Contains(got, DefaultPATH) {
		t.Errorf("Env PATH did not replace the default: %q", got)
	}

	if _, err := os.Stat(plutil); err != nil {
		t.Skip("plutil not available")
	}
	plutilLint(t, data)
	want := map[string]any{
		"Label":            "dev.gofer.tricky-1",
		"ProgramArguments": append([]any{job.Executable}, toAny(job.Args)...),
		"StartCalendarInterval": []any{
			map[string]any{"Hour": 9.0, "Weekday": 1.0},
			map[string]any{},
		},
		"EnvironmentVariables": map[string]any{"PATH": "/custom/bin:/bin", "GOFER_NOTE": job.Env["GOFER_NOTE"]},
		"StandardOutPath":      job.LogPath,
		"StandardErrorPath":    job.LogPath,
		"ProcessType":          "Background",
	}
	if got := plutilJSON(t, data); !reflect.DeepEqual(got, want) {
		t.Errorf("plutil decoded\n  %#v\nwant\n  %#v", got, want)
	}
}

func TestPlistDefaultPATH(t *testing.T) {
	job := prDigest(t)
	job.Env = map[string]string{"GOFER_ROUTINE": "1"}
	data, err := job.Plist()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>PATH</key>\n\t\t<string>" + DefaultPATH + "</string>",
		"<key>GOFER_ROUTINE</key>\n\t\t<string>1</string>",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("plist lacks %q:\n%s", want, data)
		}
	}
}

func TestJobValidate(t *testing.T) {
	bad := func(edit func(j *Job)) Job {
		j := prDigest(t)
		edit(&j)
		return j
	}
	hour := 24
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{"empty name", bad(func(j *Job) { j.Name = "" }), "invalid routine name"},
		{"uppercase name", bad(func(j *Job) { j.Name = "PR-digest" }), "invalid routine name"},
		{"leading hyphen", bad(func(j *Job) { j.Name = "-rf" }), "invalid routine name"},
		{"underscore", bad(func(j *Job) { j.Name = "pr_digest" }), "invalid routine name"},
		{"slash", bad(func(j *Job) { j.Name = "../evil" }), "invalid routine name"},
		{"dot", bad(func(j *Job) { j.Name = "a.b" }), "invalid routine name"},
		{"relative executable", bad(func(j *Job) { j.Executable = "gofer" }), "not an absolute path"},
		{"empty executable", bad(func(j *Job) { j.Executable = "" }), "not an absolute path"},
		{"relative log", bad(func(j *Job) { j.LogPath = "logs/x.log" }), "not an absolute path"},
		{"empty schedule", bad(func(j *Job) { j.Schedule = nil }), "empty schedule"},
		{"hour out of range", bad(func(j *Job) { j.Schedule = []CalendarInterval{{Hour: &hour}} }), "Hour 24 is outside 0-23"},
		{"empty env key", bad(func(j *Job) { j.Env = map[string]string{"": "x"} }), "invalid environment variable"},
		{"env key with =", bad(func(j *Job) { j.Env = map[string]string{"A=B": "x"} }), "invalid environment variable"},
		{"NUL in arg", bad(func(j *Job) { j.Args = []string{"a\x00b"} }), "characters a plist cannot hold"},
		{"escape char in env", bad(func(j *Job) { j.Env = map[string]string{"X": "\x1b[31m"} }), "characters a plist cannot hold"},
		{"invalid UTF-8", bad(func(j *Job) { j.LogPath = "/tmp/\xff.log" }), "characters a plist cannot hold"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.job.Plist(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Plist() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLabel(t *testing.T) {
	if got := Label("pr-digest"); got != "dev.gofer.pr-digest" {
		t.Errorf("Label = %q", got)
	}
	if got := (Job{Name: "x1"}).Label(); got != "dev.gofer.x1" {
		t.Errorf("Job.Label = %q", got)
	}
}

func TestDefaultLogPath(t *testing.T) {
	t.Setenv("HOME", "/Users/me")

	t.Setenv("XDG_STATE_HOME", "")
	if got, err := DefaultLogPath("pr-digest"); err != nil || got != "/Users/me/.local/state/gofer/logs/pr-digest.log" {
		t.Errorf("DefaultLogPath = %q, %v", got, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if got, err := DefaultLogPath("pr-digest"); err != nil || got != "/Users/me/.local/state/gofer/logs/pr-digest.log" {
		t.Errorf("DefaultLogPath with relative XDG_STATE_HOME = %q, %v", got, err)
	}
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, err := DefaultLogPath("pr-digest"); err != nil || got != "/state/gofer/logs/pr-digest.log" {
		t.Errorf("DefaultLogPath with XDG_STATE_HOME = %q, %v", got, err)
	}
	if _, err := DefaultLogPath("../x"); err == nil {
		t.Error("DefaultLogPath accepted an invalid name")
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// checkWellFormed fails the test unless data parses as XML.
func checkWellFormed(t *testing.T, data []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, data)
		}
	}
}

// plutilLint runs `plutil -lint` on data, skipping when plutil is missing.
func plutilLint(t *testing.T, data []byte) {
	t.Helper()
	if _, err := os.Stat(plutil); err != nil {
		t.Log("plutil not available; skipping lint")
		return
	}
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}

// plutilJSON decodes data with plutil, independently of this package.
func plutilJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -convert json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode plutil output: %v\n%s", err, out)
	}
	return m
}
