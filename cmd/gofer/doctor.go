package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/launchd"
	"github.com/TangoEnSkai/gofer/internal/routine"
	"github.com/spf13/cobra"
)

// keyHint tells the user how to provide an API key.
const keyHint = "set GEMINI_API_KEY, or store it in the Keychain: security add-generic-password -s gofer -a gemini -w"

func newDoctorCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check config, credentials, and tools",
		Long: "Doctor reports the config file, effective model, where the API key comes from\n" +
			"(never the key itself), whether the working directory is allowed, and whether\n" +
			"gh is logged in. For scheduled routines it checks that the key is in the\n" +
			"Keychain, that the gofer binary is at a stable path, and each routine's launchd\n" +
			"state and last exit code. It exits non-zero when the config is invalid, no key\n" +
			"is found, or routines are scheduled but the Keychain has no key.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.doctor(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// doctor writes a setup report to w and returns an error naming any problem
// that would stop gofer from running.
func (a *app) doctor(ctx context.Context, w io.Writer) error {
	var problems []string
	row := func(name, value string) { fmt.Fprintf(w, "%-12s %s\n", name, value) }

	cfg, err := a.loadConfig()
	if err != nil {
		row("config", "error: "+err.Error())
		problems = append(problems, "invalid config")
		cfg = a.override(config.Default())
	} else if path, _ := a.configFile(); fileExists(path) { // loadConfig already resolved path
		row("config", path)
	} else {
		row("config", path+" (not found; using defaults)")
	}

	model := cfg.Model
	if a.model != "" {
		model += " (--model)"
	}
	row("model", model)

	var source string
	if cred, err := a.resolveCredential(ctx); err != nil {
		row("credentials", "none: "+err.Error())
		row("", keyHint)
		problems = append(problems, err.Error())
	} else {
		row("credentials", cred.Source)
		source = cred.Source
	}

	// launchd jobs do not see the shell's GEMINI_API_KEY.
	env := routineEnvFrom(ctx)
	inKeychain := source == credentials.SourceKeychain
	if !inKeychain {
		_, err := env.keychain(ctx)
		inKeychain = err == nil
	}
	if inKeychain {
		row("keychain", "key found (scheduled routines read it)")
	} else {
		row("keychain", fmt.Sprintf("no key; scheduled routines need one: security add-generic-password -s %s -a %s -w",
			credentials.KeychainService, credentials.KeychainAccount))
	}

	if err := checkWorkdir(cfg); err != nil {
		row("workdir", err.Error())
	} else {
		row("workdir", "allowed")
	}

	switch err := a.ghAuthStatus(ctx); {
	case err == nil:
		row("gh", "logged in")
	case errors.Is(err, exec.ErrNotFound):
		row("gh", "not installed (brew install gh)")
	default:
		row("gh", "`gh auth status` failed; run it for details, or `gh auth login`")
	}

	switch exe, err := env.executable(); {
	case err != nil:
		row("binary", "warning: "+err.Error())
	case launchd.VersionedPath(exe):
		row("binary", exe+" (warning: a versioned path the next upgrade removes; run gofer from the package manager's symlink, e.g. /opt/homebrew/bin/gofer)")
	default:
		row("binary", exe)
	}

	names, err := routineNames(env.launchd)
	if err != nil {
		row("routines", "error: "+err.Error())
	} else if len(names) == 0 {
		row("routines", "none")
	}
	scheduled := false
	for _, name := range names {
		state := launchdState(ctx, env.launchd, name)
		scheduled = scheduled || state != "not scheduled"
		row("routine", name+": "+state)
	}
	if scheduled && !inKeychain {
		problems = append(problems, "routines are scheduled but the Keychain has no API key")
	}

	if len(problems) > 0 {
		return fmt.Errorf("doctor: %s", strings.Join(problems, "; "))
	}
	return nil
}

// routineNames returns, sorted, the routines that have a spec in the routines
// directory or an agent plist in m's LaunchAgents directory.
func routineNames(m launchd.Manager) ([]string, error) {
	specDir, err := routine.DefaultDir()
	if err != nil {
		return nil, err
	}
	agentDir := m.AgentsDir
	if agentDir == "" {
		if agentDir, err = launchd.DefaultAgentsDir(); err != nil {
			return nil, err
		}
	}
	var names []string
	for _, g := range []struct{ dir, prefix, suffix string }{
		{specDir, "", ".yaml"},
		{agentDir, launchd.LabelPrefix, ".plist"},
	} {
		// Glob only fails on a malformed pattern; a missing dir matches nothing.
		paths, _ := filepath.Glob(filepath.Join(g.dir, g.prefix+"*"+g.suffix))
		for _, p := range paths {
			name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), g.prefix), g.suffix)
			if routine.ValidateName(name) == nil {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
