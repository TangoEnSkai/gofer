package launchd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnstableReason(t *testing.T) {
	tests := []struct {
		path, tempDir string
		want          string
	}{
		{"/Users/me/go/bin/gofer", "/var/folders/ab/cd/T", ""},
		{"/opt/homebrew/Cellar/gofer/0.1.0/bin/gofer", "/var/folders/ab/cd/T", ""},
		{"/usr/local/bin/gofer", "", ""},
		{"/private/var/folders/ab/cd/T/go-build123/b001/exe/gofer", "/var/folders/ab/cd/T", "Go build cache"},
		{"/Users/me/Library/Caches/go-build/ab/gofer", "", "Go build cache"},
		{"/tmp/gofer", "", "temporary directory"},
		{"/private/tmp/x/gofer", "", "temporary directory"},
		{"/var/folders/ab/cd/T/gofer", "", "temporary directory"},
		{"/private/var/folders/ab/cd/X/gofer", "", "temporary directory"},
		{"/scratch/tmp/gofer", "/scratch/tmp", "temporary directory"},
		{"/scratch/tmp/gofer", "/scratch/tmp/", "temporary directory"},
		// Prefix matches must be whole path components.
		{"/tmpfiles/gofer", "", ""},
		{"/scratch/tmpx/gofer", "/scratch/tmp", ""},
		// A TMPDIR of "/" must not reject everything.
		{"/Users/me/go/bin/gofer", "/", ""},
	}
	for _, tt := range tests {
		got := unstableReason(tt.path, tt.tempDir)
		ok := strings.Contains(got, tt.want)
		if tt.want == "" {
			ok = got == ""
		}
		if !ok {
			t.Errorf("unstableReason(%q, %q) = %q, want %q", tt.path, tt.tempDir, got, tt.want)
		}
	}
}

// The plist gets the path as invoked, so a Homebrew symlink such as
// /opt/homebrew/bin/gofer keeps working after `brew upgrade` moves the Cellar
// directory it points to.
func TestStableExecutableKeepsSymlinks(t *testing.T) {
	if _, err := filepath.EvalSymlinks("/bin/ls"); err != nil {
		t.Skip("no /bin/ls to link to")
	}
	link := filepath.Join(t.TempDir(), "gofer")
	if err := os.Symlink("/bin/ls", link); err != nil {
		t.Fatal(err)
	}
	got, err := stableExecutable(func() (string, error) { return link, nil }, "/nonexistent-tmp")
	if err != nil {
		t.Fatalf("stableExecutable: %v", err)
	}
	if got != link {
		t.Errorf("stableExecutable = %q, want the unresolved %q", got, link)
	}
}

// Only the resolved path decides whether the binary is stable: a symlink to
// a go run build is refused, wherever the symlink lives.
func TestStableExecutableChecksResolvedPath(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "go-build123", "b001", "exe")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(build, "gofer")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "gofer")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	_, err := stableExecutable(func() (string, error) { return link, nil }, "/nonexistent-tmp")
	if err == nil || !strings.Contains(err.Error(), "Go build cache") {
		t.Errorf("stableExecutable(%q) error = %v, want a Go build cache refusal", link, err)
	}
}

func TestVersionedPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/opt/homebrew/Cellar/gofer/0.1.0/bin/gofer": true,
		"/usr/local/Cellar/gofer/0.1.0/bin/gofer":    true,
		"/opt/homebrew/bin/gofer":                    false,
		"/Users/me/go/bin/gofer":                     false,
		"/Users/me/Cellars/gofer":                    false,
	} {
		if got := VersionedPath(path); got != want {
			t.Errorf("VersionedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestStableExecutableRejectsTempBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gofer")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := stableExecutable(func() (string, error) { return exe, nil }, dir)
	if err == nil || !strings.Contains(err.Error(), "go install") {
		t.Errorf("stableExecutable(%q) error = %v, want a go install hint", exe, err)
	}
}

func TestStableExecutableLookupErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := stableExecutable(func() (string, error) { return "", boom }, ""); !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := stableExecutable(func() (string, error) { return missing, nil }, ""); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want not exist", err)
	}
}

// The test binary itself is built by go test into a temporary go-build
// directory, which is exactly what StableExecutable must refuse.
func TestStableExecutableRefusesGoTestBinary(t *testing.T) {
	path, err := StableExecutable()
	if err == nil {
		t.Skipf("test binary %s is not in a temp or build dir", path)
	}
	if !strings.Contains(err.Error(), "go install") {
		t.Errorf("error = %v, want a go install hint", err)
	}
}
