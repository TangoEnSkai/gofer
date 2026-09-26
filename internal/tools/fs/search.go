package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	maxGlobResults    = 500
	defaultGrepLimit  = 200
	maxMatchLen       = 500     // bytes of line text kept per grep match
	maxGrepFileSize   = 1 << 20 // the Go fallback skips larger files
	maxRipgrepLineLen = 1 << 20 // scanner limit for one line of ripgrep output
)

type globArgs struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern such as **/*.go or cmd/*/main.go, relative to path."`
	Path    string `json:"path,omitempty" jsonschema:"Directory to search, relative to the workspace root. Defaults to the root."`
}

type globResult struct {
	Files []string `json:"files"`
	Note  string   `json:"note,omitempty"`
}

func (w *workspace) glob(ctx context.Context, in globArgs) (globResult, error) {
	if in.Pattern == "" {
		return globResult{}, errors.New("pattern is required")
	}
	if strings.HasPrefix(in.Pattern, "/") || !doublestar.ValidatePattern(in.Pattern) {
		return globResult{}, fmt.Errorf("invalid glob pattern %q; use a relative pattern such as **/*.go", in.Pattern)
	}
	dir, err := w.resolve(in.Path)
	if err != nil {
		return globResult{}, err
	}
	if fi, err := os.Stat(dir); err != nil {
		return globResult{}, w.pathErr(dir, err)
	} else if !fi.IsDir() {
		return globResult{}, fmt.Errorf("%s is not a directory", w.rel(dir))
	}

	type hit struct {
		path string
		mod  time.Time
	}
	var hits []hit
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if p == dir {
				return err
			}
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != dir {
				return filepath.SkipDir
			}
			return nil
		}
		r, err := filepath.Rel(dir, p)
		if err != nil || !doublestar.MatchUnvalidated(in.Pattern, filepath.ToSlash(r)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		hits = append(hits, hit{w.rel(p), info.ModTime()})
		return nil
	})
	if err != nil {
		return globResult{}, w.pathErr(dir, err)
	}

	slices.SortFunc(hits, func(a, b hit) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.path, b.path)
	})
	res := globResult{Files: []string{}}
	for _, h := range hits[:min(len(hits), maxGlobResults)] {
		res.Files = append(res.Files, h.path)
	}
	switch {
	case len(hits) == 0:
		res.Note = "No files matched."
	case len(hits) > maxGlobResults:
		res.Note = fmt.Sprintf("Showing the %d most recently modified of %d matches; narrow the pattern or path.",
			maxGlobResults, len(hits))
	}
	return res, nil
}

type grepArgs struct {
	Pattern    string `json:"pattern" jsonschema:"Regular expression to search for (RE2 syntax)."`
	Path       string `json:"path,omitempty" jsonschema:"File or directory to search, relative to the workspace root. Defaults to the root."`
	Glob       string `json:"glob,omitempty" jsonschema:"Only search files matching this glob. Without a slash it matches file names (*.go); with one it matches paths from the workspace root (docs/**/*.md)."`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"Match case-insensitively."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Maximum number of matching lines to return. Defaults to 200."`
}

type grepResult struct {
	Matches []string `json:"matches"`
	Note    string   `json:"note,omitempty"`
}

func (w *workspace) grep(ctx context.Context, in grepArgs) (grepResult, error) {
	if in.Pattern == "" {
		return grepResult{}, errors.New("pattern is required")
	}
	if in.MaxResults < 0 {
		return grepResult{}, errors.New("max_results must not be negative")
	}
	limit := in.MaxResults
	if limit == 0 {
		limit = defaultGrepLimit
	}
	// Both backends accept the same syntax, so validate once for consistent errors.
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return grepResult{}, fmt.Errorf("invalid regular expression: %v", err)
	}
	if in.Glob != "" && !doublestar.ValidatePattern(in.Glob) {
		return grepResult{}, fmt.Errorf("invalid glob %q", in.Glob)
	}
	target, err := w.resolve(in.Path)
	if err != nil {
		return grepResult{}, err
	}
	if _, err := os.Stat(target); err != nil {
		return grepResult{}, w.pathErr(target, err)
	}

	var matches []string
	var truncated bool
	if w.rg != "" {
		matches, truncated, err = w.grepRipgrep(ctx, in, target, limit)
	} else {
		matches, truncated, err = w.grepGo(ctx, re, in.Glob, target, limit)
	}
	if err != nil {
		return grepResult{}, err
	}

	res := grepResult{Matches: matches}
	switch {
	case len(matches) == 0:
		res.Matches = []string{}
		res.Note = "No matches."
	case truncated:
		res.Note = fmt.Sprintf("Showing the first %d matches; narrow the pattern, path, or glob, or raise max_results.", limit)
	}
	return res, nil
}

// grepRipgrep runs ripgrep from the root, without a shell, and reads at most
// limit matches. It reports whether more matches were available.
func (w *workspace) grepRipgrep(ctx context.Context, in grepArgs, target string, limit int) ([]string, bool, error) {
	args := []string{
		"--no-config", "--no-heading", "--with-filename", "--line-number", "--color", "never",
		"--sort", "path", "--hidden", "--glob", "!.git",
		"--max-columns", fmt.Sprint(maxMatchLen), "--max-columns-preview",
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	rel := filepath.FromSlash(w.rel(target))
	args = append(args, "--regexp", in.Pattern, "--", rel)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.rg, args...)
	cmd.Dir = w.root
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start ripgrep: %v", err)
	}

	var matches []string
	truncated := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, maxRipgrepLineLen)
	for sc.Scan() {
		if len(matches) == limit {
			truncated = true
			cancel() // stop ripgrep; its exit status no longer matters
			break
		}
		matches = append(matches, strings.TrimPrefix(sc.Text(), "./"))
	}
	scanErr := sc.Err()
	err = cmd.Wait()
	switch {
	case truncated:
		return matches, true, nil
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case scanErr != nil:
		return nil, false, fmt.Errorf("read ripgrep output: %v", scanErr)
	case err == nil:
		return matches, false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, false, nil // no matches
	}
	if len(matches) > 0 {
		return matches, false, nil // partial results, e.g. some files were unreadable
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return nil, false, fmt.Errorf("ripgrep failed: %s", msg)
}

// grepGo is the fallback when ripgrep is not installed. It skips .git,
// non-regular files, binary files, and files larger than maxGrepFileSize.
func (w *workspace) grepGo(ctx context.Context, re *regexp.Regexp, glob, target string, limit int) ([]string, bool, error) {
	var matches []string
	truncated := false
	err := filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if p == target {
				return err
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != target {
				return filepath.SkipDir
			}
			return nil
		}
		rel := w.rel(p)
		// Like ripgrep: an explicitly named file is searched regardless of
		// the glob, and a glob without a slash matches the base name.
		if p != target && glob != "" && !globMatch(glob, rel) {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxGrepFileSize {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), sniffLen)], 0) >= 0 {
			return nil
		}
		n := 0
		for line := range bytes.Lines(data) {
			n++
			line = bytes.TrimRight(line, "\r\n")
			if !re.Match(line) {
				continue
			}
			if len(matches) == limit {
				truncated = true
				return filepath.SkipAll
			}
			text, _ := truncate(string(line), maxMatchLen)
			matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, n, text))
		}
		return nil
	})
	if err != nil {
		return nil, false, w.pathErr(target, err)
	}
	return matches, truncated, nil
}

func globMatch(glob, rel string) bool {
	if !strings.Contains(glob, "/") {
		rel = rel[strings.LastIndex(rel, "/")+1:]
	}
	return doublestar.MatchUnvalidated(glob, rel)
}
