package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// installHint tells the user how to get a binary launchd can keep running.
const installHint = "install it with `go install github.com/TangoEnSkai/gofer/cmd/gofer@latest` and run the installed gofer"

// StableExecutable returns the path of the running gofer binary for
// Job.Executable: absolute and as invoked, with symlinks kept. Homebrew's
// /opt/homebrew/bin/gofer is a symlink into a versioned Cellar directory; the
// symlink survives `brew upgrade`, the Cellar path does not.
//
// The symlink-resolved path is used only to refuse a binary in a Go build
// cache or a temporary directory (go run, go test), which would vanish and
// leave the agent pointing at nothing.
func StableExecutable() (string, error) {
	return stableExecutable(os.Executable, os.TempDir())
}

// stableExecutable is StableExecutable with the lookup and temp dir injected.
func stableExecutable(executable func() (string, error), tempDir string) (string, error) {
	path, err := executable()
	if err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("launchd: locate gofer binary: %w", err)
	}
	if reason := unstableReason(resolved, tempDir); reason != "" {
		return "", fmt.Errorf("launchd: gofer is running from %s, %s; %s", resolved, reason, installHint)
	}
	return path, nil
}

// VersionedPath reports whether path lies in a versioned package directory,
// such as a Homebrew Cellar, that the next upgrade removes. A job pointing
// there works until then; run gofer through the package manager's symlink
// (for example /opt/homebrew/bin/gofer) instead.
func VersionedPath(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "Cellar")
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
