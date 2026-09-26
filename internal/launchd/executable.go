package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// installHint tells the user how to get a binary launchd can keep running.
const installHint = "install it with `go install github.com/TangoEnSkai/gofer/cmd/gofer@latest` and run the installed gofer"

// StableExecutable returns the absolute, symlink-resolved path of the running
// gofer binary, for Job.Executable. It refuses a binary in a Go build cache or
// a temporary directory (go run, go test), which would vanish and leave the
// agent pointing at nothing.
//
// A package manager may resolve to a versioned path (for example a Homebrew
// Cellar directory) that changes on upgrade; re-adding the routine fixes that.
func StableExecutable() (string, error) {
	return stableExecutable(os.Executable, os.TempDir())
}

// stableExecutable is StableExecutable with the lookup and temp dir injected.
func stableExecutable(executable func() (string, error), tempDir string) (string, error) {
	path, err := executable()
	if err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	if reason := unstableReason(path, tempDir); reason != "" {
		return "", fmt.Errorf("launchd: gofer is running from %s, %s; %s", path, reason, installHint)
	}
	return path, nil
}

// unstableReason explains why path is not a stable place for a binary, or
// returns "" when it is.
func unstableReason(path, tempDir string) string {
	if strings.Contains(path, string(filepath.Separator)+"go-build") {
		return "a Go build cache (go run or go test)"
	}
	dirs := []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	if tempDir != "" {
		dirs = append(dirs, tempDir)
		if resolved, err := filepath.EvalSymlinks(tempDir); err == nil {
			dirs = append(dirs, resolved)
		}
	}
	for _, dir := range dirs {
		if within(path, dir) {
			return "a temporary directory"
		}
	}
	return ""
}

// within reports whether path is strictly inside dir. The filesystem root
// never counts, so a TMPDIR of "/" cannot reject every path.
func within(path, dir string) bool {
	dir = filepath.Clean(dir)
	if dir == string(filepath.Separator) || dir == "." {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
