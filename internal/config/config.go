// Package config loads gofer's user configuration from a TOML file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Defaults applied when the config file omits a value.
const (
	DefaultModel             = "gemini-flash-latest"
	DefaultRequestsPerMinute = 10
)

// Config is gofer's user configuration.
//
//	model = "gemini-flash-latest"
//	deny_dirs = ["~/work"]
//
//	[limits]
//	requests_per_minute = 10
type Config struct {
	// Model is the Gemini model name.
	Model string `toml:"model"`
	// DenyDirs lists directories whose contents must never be sent to the
	// model (free-tier data-use guard). Load expands a leading "~".
	DenyDirs []string `toml:"deny_dirs"`
	Limits   Limits   `toml:"limits"`
}

// Limits bounds gofer's use of the model API.
type Limits struct {
	// RequestsPerMinute caps model requests.
	RequestsPerMinute int `toml:"requests_per_minute"`
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Model:  DefaultModel,
		Limits: Limits{RequestsPerMinute: DefaultRequestsPerMinute},
	}
}

// DefaultPath returns $XDG_CONFIG_HOME/gofer/config.toml, or
// ~/.config/gofer/config.toml when XDG_CONFIG_HOME is unset or relative.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "gofer", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return filepath.Join(home, ".config", "gofer", "config.toml"), nil
}

// Load reads the config file at path. A missing file yields Default(). A
// malformed file, an unknown key, or an invalid value is an error, so that a
// typo cannot silently disable the deny list.
func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return Config{}, fmt.Errorf("config %s: unknown key(s): %s", path, strings.Join(names, ", "))
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// normalize validates values and expands "~" in DenyDirs.
func (c *Config) normalize() error {
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("model must not be empty")
	}
	if c.Limits.RequestsPerMinute <= 0 {
		return fmt.Errorf("limits.requests_per_minute must be positive, got %d", c.Limits.RequestsPerMinute)
	}
	for i, d := range c.DenyDirs {
		p, err := expandHome(d)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("deny_dirs entry %q must be an absolute path or start with ~/", d)
		}
		c.DenyDirs[i] = filepath.Clean(p)
	}
	return nil
}

// IsDenied reports whether dir is equal to or inside any of c.DenyDirs.
// Paths are compared as cleaned absolute paths after resolving symlinks for
// the parts that exist, so /tmp and /private/tmp match on macOS. On macOS the
// comparison is also case-insensitive, like the default file system.
func (c Config) IsDenied(dir string) (bool, error) {
	if len(c.DenyDirs) == 0 {
		return false, nil
	}
	target, err := canonical(dir)
	if err != nil {
		return false, err
	}
	for _, d := range c.DenyDirs {
		root, err := canonical(d)
		if err != nil {
			return false, err
		}
		if within(target, root) {
			return true, nil
		}
	}
	return false, nil
}

// within reports whether path is root or lies inside it. Both must be clean
// absolute paths.
func within(path, root string) bool {
	if runtime.GOOS == "darwin" {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

// canonical returns p as a clean absolute path with symlinks resolved for its
// longest existing prefix; the non-existent remainder is appended as is.
func canonical(p string) (string, error) {
	p, err := expandHome(p)
	if err != nil {
		return "", err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// expandHome replaces a leading "~" or "~/" with the user's home directory.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, p[1:]), nil
}
