package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/TangoEnSkai/gofer/internal/agent"
	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
)

// app holds the global flag values and the side effects commands depend on,
// so tests can replace the latter.
type app struct {
	configPath  string // --config
	model       string // --model
	prompt      string // -p, --prompt
	output      string // --output
	allowWrites bool   // --allow-writes
	// Session selection; see resumeTarget.
	continueLast bool   // -c, --continue
	resume       string // --resume
	forceWorkdir bool   // --force-workdir

	resolveCredential func(context.Context) (credentials.Credential, error)
	ghAuthStatus      func(context.Context) error
	newModel          func(ctx context.Context, name, apiKey string) (model.LLM, error)
	buildApp          func(context.Context, config.Config, goferapp.Mode, goferapp.BuildOptions) (*goferapp.App, error)
	limiter           *rate.Limiter // nil: limits.requests_per_minute from the config
	stdin             io.Reader
	term              terminal
	// interrupts delivers Ctrl-C to the REPL until stop is called.
	interrupts func() (sigs <-chan os.Signal, stop func())
}

func newRootCmd() *cobra.Command {
	return newRootCmdFor(&app{
		resolveCredential: credentials.Resolve,
		ghAuthStatus:      ghAuthStatus,
		newModel:          agent.NewGeminiModel,
		buildApp:          goferapp.Build,
		stdin:             os.Stdin,
		term:              detectTerminal(),
		interrupts:        notifyInterrupt,
	})
}

// newRootCmdFor builds the command tree around a.
func newRootCmdFor(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gofer",
		Short: "A lightweight, Gemini-powered agent for developer toil",
		Long: "Without -p, gofer starts a line-based REPL when stdin and stdout are terminals.\n" +
			"With -p, or with input piped to stdin, it runs one prompt and exits (headless\n" +
			"mode): piped input is appended to the -p prompt, or is the prompt without -p.\n" +
			"Headless runs can only read files and query GitHub unless --allow-writes is set.\n\n" +
			"Sessions are saved: --continue resumes the latest one of the current directory,\n" +
			"and --resume <id> any other (see gofer sessions list), in either mode.\n\n" +
			"Exit codes: 0 ok, 1 runtime error, 2 usage or config error (including no API\n" +
			"key and a denied directory), 3 a tool call needed a confirmation that headless\n" +
			"mode cannot give.",
		Example: "  gofer\n" +
			"  gofer --continue\n" +
			"  gofer -p \"summarize my open PRs\"\n" +
			"  git diff | gofer -p \"write a commit message for this diff\"\n" +
			"  gofer -p \"...\" --output json",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(fmt.Errorf("unknown command %q for %q (pass a prompt with -p)", args[0], cmd.CommandPath()))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.root(cmd)
		},
	}
	cmd.SetVersionTemplate("gofer {{.Version}}\n")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })

	f := cmd.PersistentFlags()
	f.StringVar(&a.configPath, "config", "", "config file (default $XDG_CONFIG_HOME/gofer/config.toml or ~/.config/gofer/config.toml)")
	f.StringVar(&a.model, "model", "", "Gemini model to use, overriding the config file")

	lf := cmd.Flags()
	lf.StringVarP(&a.prompt, "prompt", "p", "", "run this prompt once and exit (headless mode)")
	lf.StringVar(&a.output, "output", outputText, "headless output format: text, json, or stream-json")
	lf.BoolVar(&a.allowWrites, "allow-writes", false, "headless: register write_file, edit_file, and bash, running them without confirmation")
	lf.BoolVarP(&a.continueLast, "continue", "c", false, "resume the most recent session of the current directory")
	lf.StringVar(&a.resume, "resume", "", "resume the session with this ID (see gofer sessions list)")
	lf.BoolVar(&a.forceWorkdir, "force-workdir", false, "with --resume, resume a session started in another directory")

	cmd.AddCommand(newSessionsCmd())

	cmd.AddCommand(newVersionCmd(), newDoctorCmd(a))
	return cmd
}

// root runs the REPL or a headless prompt, per the detected mode.
func (a *app) root(cmd *cobra.Command) error {
	switch a.output {
	case outputText, outputJSON, outputStreamJSON:
	default:
		return usageError(fmt.Errorf("invalid --output %q: want %s, %s, or %s", a.output, outputText, outputJSON, outputStreamJSON))
	}
	if err := a.checkResumeFlags(cmd); err != nil {
		return err
	}
	hasPrompt := cmd.Flags().Changed("prompt")
	mode := detectMode(hasPrompt, a.term)
	if mode == goferapp.Interactive && (cmd.Flags().Changed("output") || a.allowWrites) {
		return usageError(errors.New("--output and --allow-writes need headless mode: pass -p or pipe input to stdin"))
	}
	if mode == goferapp.Headless && !hasPrompt && !a.term.stdinPiped {
		return usageError(errors.New(`no prompt: pass -p "..." or pipe one to stdin`))
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return usageError(err)
	}
	if mode == goferapp.Interactive {
		return a.interactive(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	return a.headless(cmd.Context(), cfg, hasPrompt, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// build wires the app for mode with sessions stored in svc. It refuses a
// denied workdir before anything else, and maps config problems to exit
// code 2.
func (a *app) build(ctx context.Context, cfg config.Config, mode goferapp.Mode, svc session.Service) (*goferapp.App, error) {
	ga, err := a.buildApp(ctx, cfg, mode, goferapp.BuildOptions{
		AllowWrites:       a.allowWrites,
		Sessions:          svc,
		Version:           version,
		ResolveCredential: a.resolveCredential,
		NewModel:          a.newModel,
		Limiter:           a.limiter,
	})
	switch {
	case errors.Is(err, goferapp.ErrDeniedDir):
		return nil, usageError(err)
	case errors.Is(err, credentials.ErrNotFound):
		return nil, usageError(fmt.Errorf("%w; %s", err, keyHint))
	}
	return ga, err
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

// notifyInterrupt relays SIGINT (Ctrl-C) instead of letting it end the
// process.
func notifyInterrupt() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}
