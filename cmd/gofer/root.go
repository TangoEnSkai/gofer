package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/spf13/cobra"
)

// app holds the global flag values and the side effects commands depend on,
// so tests can replace the latter.
type app struct {
	configPath string // --config
	model      string // --model

	resolveCredential func(context.Context) (credentials.Credential, error)
	ghAuthStatus      func(context.Context) error
}

func newRootCmd() *cobra.Command {
	return newRootCmdFor(&app{
		resolveCredential: credentials.Resolve,
		ghAuthStatus:      ghAuthStatus,
	})
}

// newRootCmdFor builds the command tree around a.
func newRootCmdFor(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "gofer",
		Short:         "A lightweight, Gemini-powered agent for developer toil",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if err := checkWorkdir(cfg); err != nil {
				return err
			}
			return errors.New("interactive mode not implemented yet (#12)")
		},
	}
	cmd.SetVersionTemplate("gofer {{.Version}}\n")

	f := cmd.PersistentFlags()
	f.StringVar(&a.configPath, "config", "", "config file (default $XDG_CONFIG_HOME/gofer/config.toml or ~/.config/gofer/config.toml)")
	f.StringVar(&a.model, "model", "", "Gemini model to use, overriding the config file")

	cmd.AddCommand(newVersionCmd(), newDoctorCmd(a))
	return cmd
}

// configFile returns the --config path, or the default path when unset.
func (a *app) configFile() (string, error) {
	if a.configPath != "" {
		return a.configPath, nil
	}
	return config.DefaultPath()
}

// loadConfig loads the config file and applies flag overrides. A missing
// default file yields defaults, but a missing --config file is an error.
func (a *app) loadConfig() (config.Config, error) {
	path, err := a.configFile()
	if err != nil {
		return config.Config{}, err
	}
	if a.configPath != "" {
		if _, err := os.Stat(path); err != nil {
			return config.Config{}, fmt.Errorf("config: %w", err)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	return a.override(cfg), nil
}

// override applies flag values on top of cfg.
func (a *app) override(cfg config.Config) config.Config {
	if a.model != "" {
		cfg.Model = a.model
	}
	return cfg
}

// ghAuthStatus reports whether the gh CLI is installed and logged in. Its
// output is discarded so that no token detail is ever echoed.
func ghAuthStatus(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "gh", "auth", "status").Run()
}
