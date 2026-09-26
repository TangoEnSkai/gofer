// Package fs provides ADK function tools that read, write, edit, and search
// files confined to a workspace root.
//
// Every path argument is resolved against the root, symlinks included, and
// anything that lands outside the root is refused. The check is not atomic
// with the file operation that follows; stronger isolation is the job of the
// M3 sandbox.
//
// Tool failures (file not found, ambiguous edit, path outside the root, ...)
// are returned as Go errors with messages written for the model. ADK's
// function-call flow turns a tool error into an ordinary function response
// {"error": "<message>"} and keeps the agent loop running, so the model sees
// the message and can recover (pinned by TestToolErrorReachesModel). Plain
// errors rather than an error field in every result type also let
// OnToolError callbacks and plugins see the failures.
package fs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// Options configures the write-capable tools.
type Options struct {
	// ConfirmWrites makes write_file and edit_file request user confirmation
	// on every call. It is the M1 interactive default (ADR-0003).
	ConfirmWrites bool
}

// ReadOnlyTools returns read_file, glob, and grep confined to root. Unattended
// routine runs register only these (ADR-0003).
func ReadOnlyTools(root string) ([]tool.Tool, error) {
	w, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return w.tools(false, Options{})
}

// Tools returns read_file, write_file, edit_file, glob, and grep confined to
// root.
func Tools(root string, opts Options) ([]tool.Tool, error) {
	w, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return w.tools(true, opts)
}

// lookRipgrep returns the path of ripgrep, or "" to use the Go grep fallback.
// Tests replace it to pick a backend.
var lookRipgrep = func() string {
	p, _ := exec.LookPath("rg")
	return p
}

// workspace is the root that all tools are confined to.
type workspace struct {
	root  string // absolute and symlink-resolved
	given string // absolute as given, so absolute paths under it are accepted
	rg    string // ripgrep binary, or "" for the Go fallback
}

func newWorkspace(root string) (*workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if fi, err := os.Stat(resolved); err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", root)
	}
	return &workspace{root: resolved, given: abs, rg: lookRipgrep()}, nil
}

func (w *workspace) tools(writes bool, opts Options) ([]tool.Tool, error) {
	var ts []tool.Tool
	var errs []error
	add := func(t tool.Tool, err error) {
		ts = append(ts, t)
		errs = append(errs, err)
	}
	add(functiontool.New(functiontool.Config{
		Name: "read_file",
		Description: "Reads a text file in the workspace. Returns lines prefixed with 1-based line numbers " +
			"and the file's total line count; use offset and limit to page through large files. Binary files are refused.",
	}, func(_ agent.Context, in readArgs) (readResult, error) { return w.readFile(in) }))
	if writes {
		add(functiontool.New(functiontool.Config{
			Name: "write_file",
			Description: "Creates or overwrites a file in the workspace with the given content, creating parent " +
				"directories as needed. Prefer edit_file for small changes to an existing file.",
			RequireConfirmation: opts.ConfirmWrites,
		}, func(_ agent.Context, in writeArgs) (writeResult, error) { return w.writeFile(in) }))
		add(functiontool.New(functiontool.Config{
			Name: "edit_file",
			Description: "Replaces an exact string in a file. old_string must match the file content exactly, " +
				"including whitespace and indentation, and must occur exactly once unless replace_all is true. " +
				"Read the file first.",
			RequireConfirmation: opts.ConfirmWrites,
		}, func(_ agent.Context, in editArgs) (editResult, error) { return w.editFile(in) }))
	}
	add(functiontool.New(functiontool.Config{
		Name: "glob",
		Description: "Finds files whose path matches a glob pattern (** matches any number of directories). " +
			"Returns paths relative to the workspace root, most recently modified first. .git is skipped.",
	}, func(ctx agent.Context, in globArgs) (globResult, error) { return w.glob(ctx, in) }))
	add(functiontool.New(functiontool.Config{
		Name: "grep",
		Description: "Searches file contents for a regular expression (RE2 syntax) and returns file:line:text matches " +
			"with paths relative to the workspace root. .git and binary files are skipped; " +
			"when ripgrep is installed, files ignored by .gitignore are skipped too.",
	}, func(ctx agent.Context, in grepArgs) (grepResult, error) { return w.grep(ctx, in) }))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return ts, nil
}

// resolve maps a path argument to an absolute, symlink-resolved path inside
// the root. Trailing components may be missing so that write_file can create
// files; the nearest existing ancestor decides where they would land.
func (w *workspace) resolve(arg string) (string, error) {
	p := arg
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	p = filepath.Clean(p)
	if !within(w.root, p) && !within(w.given, p) {
		return "", fmt.Errorf("%s: path is outside the workspace root", arg)
	}

	existing, rest := p, ""
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", w.pathErr(existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	// EvalSymlinks fails on a dangling symlink, which is refused rather than
	// followed: writing through it could create a file anywhere.
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve path (broken symlink?)", arg)
	}
	full := filepath.Join(resolved, rest)
	if !within(w.root, full) {
		return "", fmt.Errorf("%s: path resolves outside the workspace root", arg)
	}
	return full, nil
}

// rel returns p relative to the root with forward slashes, for output.
func (w *workspace) rel(p string) string {
	r, err := filepath.Rel(w.root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// pathErr rewrites an os error so the model sees a root-relative path.
func (w *workspace) pathErr(p string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %v", w.rel(p), pe.Err)
	}
	return err
}

func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
