package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/TangoEnSkai/gofer/internal/config"
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
			"gh is logged in. It exits non-zero when the config is invalid or no key is found.",
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

	if cred, err := a.resolveCredential(ctx); err != nil {
		row("credentials", "none: "+err.Error())
		row("", keyHint)
		problems = append(problems, err.Error())
	} else {
		row("credentials", cred.Source)
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

	if len(problems) > 0 {
		return fmt.Errorf("doctor: %s", strings.Join(problems, "; "))
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
