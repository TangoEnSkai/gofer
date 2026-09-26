// Package launchd schedules gofer routines as per-user launchd agents: it turns
// a cron subset into StartCalendarInterval entries, renders the agent plist,
// and loads or unloads it with launchctl (docs/specs/routines.md §5).
package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// LabelPrefix is prepended to a routine name to form its launchd label.
const LabelPrefix = "dev.gofer."

// DefaultPATH is the PATH a job gets unless Job.Env sets one. launchd does not
// read the user's shell profile, so tools such as gh would otherwise be missing.
const DefaultPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateName reports whether name is a valid routine name: lowercase
// letters, digits, and hyphens, not starting with a hyphen (it becomes a
// command-line argument).
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid routine name %q: use lowercase letters, digits, and hyphens", name)
	}
	return nil
}

// Label returns the launchd label for a routine, such as "dev.gofer.pr-digest".
func Label(name string) string {
	return LabelPrefix + name
}

// DefaultLogPath returns $XDG_STATE_HOME/gofer/logs/<name>.log, or
// ~/.local/state/gofer/logs/<name>.log when XDG_STATE_HOME is unset or relative.
func DefaultLogPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("launchd: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "gofer", "logs", name+".log"), nil
}

// Job is a routine scheduled with launchd. It must not carry secrets: the
// plist is a plain file, and the API key comes from the Keychain (ADR-0004).
type Job struct {
	// Name is the routine name; see ValidateName.
	Name string
	// Executable is the absolute path of the gofer binary; see
	// StableExecutable.
	Executable string
	// Args follow Executable in ProgramArguments, e.g. "routine", "run", Name.
	Args []string
	// Schedule is when the job runs; see ParseSchedule. It must not be empty.
	Schedule []CalendarInterval
	// LogPath receives stdout and stderr; see DefaultLogPath.
	LogPath string
	// Env adds environment variables. PATH defaults to DefaultPATH.
	Env map[string]string
}

// Label returns the job's launchd label.
func (j Job) Label() string { return Label(j.Name) }

// Validate checks the job before it is rendered or installed.
func (j Job) Validate() error {
	if err := ValidateName(j.Name); err != nil {
		return err
	}
	if !filepath.IsAbs(j.Executable) {
		return fmt.Errorf("launchd job %s: executable %q is not an absolute path", j.Name, j.Executable)
	}
	if !filepath.IsAbs(j.LogPath) {
		return fmt.Errorf("launchd job %s: log path %q is not an absolute path", j.Name, j.LogPath)
	}
	if len(j.Schedule) == 0 {
		return fmt.Errorf("launchd job %s: empty schedule", j.Name)
	}
	for _, ci := range j.Schedule {
		if err := ci.validate(); err != nil {
			return fmt.Errorf("launchd job %s: %w", j.Name, err)
		}
	}
	for k := range j.Env {
		if k == "" || strings.Contains(k, "=") {
			return fmt.Errorf("launchd job %s: invalid environment variable name %q", j.Name, k)
		}
	}
	for _, s := range j.texts() {
		if !xmlSafe(s) {
			return fmt.Errorf("launchd job %s: %q contains characters a plist cannot hold", j.Name, s)
		}
	}
	return nil
}

// texts returns every free-form string the plist will contain.
func (j Job) texts() []string {
	out := append([]string{j.Executable, j.LogPath}, j.Args...)
	for k, v := range j.Env {
		out = append(out, k, v)
	}
	return out
}

// env returns the job's environment with the PATH default applied.
func (j Job) env() map[string]string {
	env := map[string]string{"PATH": DefaultPATH}
	for k, v := range j.Env {
		env[k] = v
	}
	return env
}

// Plist renders the job as an XML property list for ~/Library/LaunchAgents.
// Every string is XML-escaped, and map keys are sorted so the output is
// deterministic.
func (j Job) Plist() ([]byte, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	w := plistWriter{b: &b, depth: 1}

	w.key("Label")
	w.str(j.Label())

	w.key("ProgramArguments")
	w.open("array")
	w.str(j.Executable)
	for _, a := range j.Args {
		w.str(a)
	}
	w.close("array")

	w.key("StartCalendarInterval")
	w.open("array")
	for _, ci := range j.Schedule {
		w.open("dict")
		for _, f := range ci.fields() {
			w.key(f.key)
			w.integer(*f.val)
		}
		w.close("dict")
	}
	w.close("array")

	w.key("EnvironmentVariables")
	w.open("dict")
	env := j.env()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		w.key(k)
		w.str(env[k])
	}
	w.close("dict")

	w.key("StandardOutPath")
	w.str(j.LogPath)
	w.key("StandardErrorPath")
	w.str(j.LogPath)

	w.key("ProcessType")
	w.str("Background")

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

// plistWriter writes tab-indented plist elements, one per line.
type plistWriter struct {
	b     *bytes.Buffer
	depth int
}

func (w *plistWriter) indent() {
	w.b.WriteString(strings.Repeat("\t", w.depth))
}

func (w *plistWriter) element(tag, text string) {
	w.indent()
	w.b.WriteString("<" + tag + ">")
	xml.EscapeText(w.b, []byte(text)) // Writes to a bytes.Buffer cannot fail.
	w.b.WriteString("</" + tag + ">\n")
}

func (w *plistWriter) key(k string)  { w.element("key", k) }
func (w *plistWriter) str(s string)  { w.element("string", s) }
func (w *plistWriter) integer(n int) { w.element("integer", strconv.Itoa(n)) }

func (w *plistWriter) open(tag string) {
	w.indent()
	w.b.WriteString("<" + tag + ">\n")
	w.depth++
}

func (w *plistWriter) close(tag string) {
	w.depth--
	w.indent()
	w.b.WriteString("</" + tag + ">\n")
}

// intervalField is one set key of a CalendarInterval, as launchd names it.
type intervalField struct {
	key      string
	val      *int
	min, max int
}

// fields returns the set fields in launchd's key order (alphabetical).
func (c CalendarInterval) fields() []intervalField {
	all := []intervalField{
		{"Day", c.Day, 1, 31},
		{"Hour", c.Hour, 0, 23},
		{"Minute", c.Minute, 0, 59},
		{"Month", c.Month, 1, 12},
		{"Weekday", c.Weekday, 0, 7},
	}
	return slices.DeleteFunc(all, func(f intervalField) bool { return f.val == nil })
}

func (c CalendarInterval) validate() error {
	for _, f := range c.fields() {
		if *f.val < f.min || *f.val > f.max {
			return fmt.Errorf("calendar interval %s %d is outside %d-%d", f.key, *f.val, f.min, f.max)
		}
	}
	return nil
}

// xmlSafe reports whether s is valid UTF-8 made only of characters XML 1.0
// allows, so that escaping it cannot lose data.
func xmlSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= utf8.MaxRune:
		default:
			return false
		}
	}
	return true
}
