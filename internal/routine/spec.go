// Package routine runs gather-then-judge routines: a registered Go gatherer
// collects data deterministically, then one LLM call with no tools judges it
// into a JSON answer that Go code renders. See docs/specs/routines.md and
// ADR-0002, ADR-0005, ADR-0007.
package routine

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Notify says when a routine run shows a notification.
type Notify string

// Notify policies. NotifyOnAction is the default.
const (
	NotifyAlways    Notify = "always"
	NotifyOnFailure Notify = "on-failure"
	NotifyOnAction  Notify = "on-action"
	NotifyNever     Notify = "never"
)

// MaxPromptLen caps Spec.Prompt in bytes; it is extra guidance, not a program.
const MaxPromptLen = 4096

// Spec is a routine definition, read from <name>.yaml.
//
//	name: pr-digest
//	gatherer: github.my_open_prs
//	with: {limit: 50}
//	schedule: "0 9 * * 1-5"
//	prompt: Prioritise PRs in databricks/* repos.
//	notify: on-action
type Spec struct {
	// Name matches ^[a-z0-9][a-z0-9-]*$ and the file name.
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Gatherer names a registered Go gatherer (ADR-0007).
	Gatherer string `yaml:"gatherer"`
	// With holds gatherer parameters; the gatherer validates them.
	With map[string]any `yaml:"with"`
	// Schedule is a 5-field cron expression in local time. Only presence is
	// checked here; the launchd package validates the supported subset.
	Schedule string `yaml:"schedule"`
	// Model optionally overrides the configured model.
	Model string `yaml:"model"`
	// Prompt is optional guidance appended to the judge instruction.
	Prompt string `yaml:"prompt"`
	Notify Notify `yaml:"notify"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

const maxNameLen = 64

// Parse decodes and validates one YAML routine spec. Unknown keys are errors.
func Parse(b []byte) (Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return Spec{}, errors.New("routine spec is empty")
		}
		return Spec{}, fmt.Errorf("routine spec: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("routine spec: want exactly one YAML document")
	}
	if s.Notify == "" {
		s.Notify = NotifyOnAction
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Validate checks the fields Parse can check without a gatherer registry.
func (s Spec) Validate() error {
	var errs []error
	prefix := "routine " + s.Name
	if err := ValidateName(s.Name); err != nil {
		errs = append(errs, err)
		prefix = "routine spec"
	}
	if strings.TrimSpace(s.Gatherer) == "" {
		errs = append(errs, errors.New("gatherer is required"))
	}
	if strings.TrimSpace(s.Schedule) == "" {
		errs = append(errs, errors.New("schedule is required"))
	}
	switch s.Notify {
	case NotifyAlways, NotifyOnFailure, NotifyOnAction, NotifyNever:
	default:
		errs = append(errs, fmt.Errorf("notify %q must be one of always, on-failure, on-action, never", s.Notify))
	}
	if len(s.Prompt) > MaxPromptLen {
		errs = append(errs, fmt.Errorf("prompt is %d bytes, max %d", len(s.Prompt), MaxPromptLen))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

// ValidateName reports whether name is a valid routine name. Names become
// file names and launchd labels, so they are restricted to [a-z0-9-] and may
// not start with "-".
func ValidateName(name string) error {
	if len(name) > maxNameLen || !namePattern.MatchString(name) {
		return fmt.Errorf("name %q must match [a-z0-9][a-z0-9-]* (max %d chars)", name, maxNameLen)
	}
	return nil
}

// Bundled holds the routine specs shipped with gofer, under "bundled/".
//
//go:embed bundled
var Bundled embed.FS

// DefaultDir returns $XDG_CONFIG_HOME/gofer/routines, or
// ~/.config/gofer/routines when XDG_CONFIG_HOME is unset or relative.
func DefaultDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "gofer", "routines"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("routine: %w", err)
	}
	return filepath.Join(home, ".config", "gofer", "routines"), nil
}

// Load reads the spec named name from dir/<name>.yaml.
func Load(dir, name string) (Spec, error) {
	if err := ValidateName(name); err != nil {
		return Spec{}, fmt.Errorf("routine: %w", err)
	}
	p := filepath.Join(dir, name+".yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		return Spec{}, fmt.Errorf("routine %q: %w", name, err)
	}
	s, err := parseNamed(b, name)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// LoadDir parses every *.yaml file in dir, sorted by name. A missing dir
// yields no specs. An invalid file does not hide the valid ones: LoadDir
// returns them together with an error that names each invalid file.
func LoadDir(dir string) ([]Spec, error) {
	specs, err := LoadFS(os.DirFS(dir), ".")
	if err != nil {
		err = fmt.Errorf("routines in %s: %w", dir, err)
	}
	return specs, err
}

// LoadFS is LoadDir for a file system, e.g. LoadFS(Bundled, "bundled").
func LoadFS(fsys fs.FS, dir string) ([]Spec, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []Spec
	var errs []error
	for _, e := range entries {
		file := e.Name()
		if e.IsDir() || !strings.HasSuffix(file, ".yaml") {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, file))
		if err == nil {
			var s Spec
			if s, err = parseNamed(b, strings.TrimSuffix(file, ".yaml")); err == nil {
				specs = append(specs, s)
				continue
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", file, err))
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, errors.Join(errs...)
}

// parseNamed parses b and checks that the spec's name matches its file name.
func parseNamed(b []byte, name string) (Spec, error) {
	s, err := Parse(b)
	if err != nil {
		return Spec{}, err
	}
	if s.Name != name {
		return Spec{}, fmt.Errorf("routine name %q does not match file name %q", s.Name, name+".yaml")
	}
	return s, nil
}
