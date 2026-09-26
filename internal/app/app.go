// Package app is gofer's composition root: it turns a config and a run mode
// into a ready agent, runner, and session service, so that the CLI modes and
// the routine runner share one wiring (docs/specs/cli-modes.md §5).
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/time/rate"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/TangoEnSkai/gofer/internal/agent"
	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/ratelimit"
	"github.com/TangoEnSkai/gofer/internal/sessions"
	"github.com/TangoEnSkai/gofer/internal/tools/fs"
	"github.com/TangoEnSkai/gofer/internal/tools/gh"
	"github.com/TangoEnSkai/gofer/internal/tools/shell"
)

// Mode is how gofer runs; it decides which tools are registered and how
// confirmations are handled (docs/specs/cli-modes.md §1).
type Mode int

// The zero Mode is invalid so that a forgotten mode cannot select a profile.
const (
	// Interactive is the REPL: a human answers confirmation prompts.
	Interactive Mode = iota + 1
	// Headless is a one-shot run (gofer -p, piped stdin); it never prompts.
	Headless
	// Routine is an unattended scheduled run; it only gets read-only tools.
	Routine
)

func (m Mode) String() string {
	switch m {
	case Interactive:
		return "interactive"
	case Headless:
		return "headless"
	case Routine:
		return "routine"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// UserID is the ADK user that owns gofer's sessions. gofer is single-user.
const UserID = sessions.UserID

// InteractiveCompactionInterval is how many user turns of an interactive
// session pass between context compactions (docs/spikes/adk-v2.md Q6).
// Headless and routine runs are one-shot and never compact.
const InteractiveCompactionInterval = 10

// ErrDeniedDir is wrapped by errors for a workdir inside a deny_dirs entry.
var ErrDeniedDir = errors.New("it is inside a deny_dirs entry of the config")

// CheckWorkdir refuses agent work in dir when it is on the configured deny
// list, so its contents are never sent to the model.
func CheckWorkdir(cfg config.Config, dir string) error {
	denied, err := cfg.IsDenied(dir)
	if err != nil {
		return fmt.Errorf("check deny_dirs for %s: %w", dir, err)
	}
	if denied {
		return fmt.Errorf("refusing to run in %s: %w", dir, ErrDeniedDir)
	}
	return nil
}

// ToolNames returns, sorted, the names of the tools registered in mode: the
// profile table of docs/specs/cli-modes.md §2. allowWrites is --allow-writes,
// which only headless mode accepts.
func ToolNames(mode Mode, allowWrites bool) []string {
	names := []string{"read_file", "glob", "grep", gh.ToolName}
	if writable(mode, allowWrites) {
		names = append(names, "write_file", "edit_file", shell.Name)
	}
	slices.Sort(names)
	return names
}

// writable reports whether mode registers write and shell tools.
func writable(mode Mode, allowWrites bool) bool {
	return mode == Interactive || (mode == Headless && allowWrites)
}

// BuildOptions are the inputs to Build besides the config and mode. The zero
// value runs in the current directory with in-memory sessions.
type BuildOptions struct {
	// Workdir is the workspace root for tools and project instructions.
	// Empty means the current directory.
	Workdir string
	// AllowWrites registers write and shell tools without confirmation. Only
	// headless mode accepts it (--allow-writes).
	AllowWrites bool
	// Sessions stores sessions. Nil selects session.InMemoryService(); the
	// CLI passes a sessions.Store's service so sessions can be resumed.
	Sessions session.Service
	// Version is the gofer version recorded in the sessions the app creates.
	Version string
	// ResolveCredential returns the Gemini API key. Nil means
	// credentials.Resolve.
	ResolveCredential func(context.Context) (credentials.Credential, error)
	// NewModel builds a model by name. Nil means agent.NewGeminiModel; tests
	// return a scripted model.
	NewModel func(ctx context.Context, name, apiKey string) (model.LLM, error)
	// Limiter spaces model requests. Gemini quotas are per API key, so a
	// process that builds several apps should share one. Nil means
	// ratelimit.NewLimiter(cfg.Limits.RequestsPerMinute).
	Limiter *rate.Limiter
	// GitHub backs the github tool. Nil means gh.New(nil), which runs the gh
	// binary on each call.
	GitHub *gh.Client
}

// App is a wired agent ready to run turns.
type App struct {
	Mode    Mode
	Workdir string
	// Tools are the registered tools, per ToolNames.
	Tools    []tool.Tool
	Sessions session.Service
	Agent    adkagent.Agent
	Runner   *runner.Runner
	// CompactionInterval is how many user turns pass between context
	// compactions; 0 means never. Build sets it from the mode.
	CompactionInterval int

	version  string
	model    model.LLM
	newModel func(ctx context.Context, name string) (model.LLM, error)
}

// Build wires gofer for mode: it refuses a denied workdir, resolves the API
// key, builds the rate-limited model, selects tools per ToolNames, and loads
// project instructions from the workdir. It makes no model call.
func Build(ctx context.Context, cfg config.Config, mode Mode, opts BuildOptions) (*App, error) {
	switch mode {
	case Interactive, Headless, Routine:
	default:
		return nil, fmt.Errorf("app: invalid mode %v", mode)
	}
	if opts.AllowWrites && mode != Headless {
		return nil, fmt.Errorf("app: allowing writes without confirmation is not available in %v mode", mode)
	}

	dir := opts.Workdir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := CheckWorkdir(cfg, dir); err != nil {
		return nil, err
	}

	resolve := opts.ResolveCredential
	if resolve == nil {
		resolve = credentials.Resolve
	}
	cred, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	newModel := opts.NewModel
	if newModel == nil {
		newModel = agent.NewGeminiModel
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = ratelimit.NewLimiter(cfg.Limits.RequestsPerMinute)
	}

	ghc := opts.GitHub
	if ghc == nil {
		ghc = gh.New(nil)
	}
	tools, err := buildTools(dir, mode, opts.AllowWrites, ghc)
	if err != nil {
		return nil, err
	}
	svc := opts.Sessions
	if svc == nil {
		svc = session.InMemoryService()
	}
	a := &App{
		Mode:     mode,
		Workdir:  dir,
		Tools:    tools,
		Sessions: svc,
		version:  opts.Version,
		// The closure keeps the key out of App's fields and printed values.
		newModel: func(ctx context.Context, name string) (model.LLM, error) {
			m, err := newModel(ctx, name, cred.Key)
			if err != nil {
				return nil, err
			}
			return ratelimit.Wrap(m, limiter), nil
		},
	}
	if mode == Interactive {
		a.CompactionInterval = InteractiveCompactionInterval
	}
	if err := a.SetModel(ctx, cfg.Model); err != nil {
		return nil, err
	}
	return a, nil
}

// buildTools returns the tools of mode's profile. Tools that need a human
// confirm each call only in interactive mode; other modes either do not
// register them or opted out with --allow-writes.
func buildTools(root string, mode Mode, allowWrites bool, ghc *gh.Client) ([]tool.Tool, error) {
	var tools []tool.Tool
	if writable(mode, allowWrites) {
		confirm := mode == Interactive
		files, err := fs.Tools(root, fs.Options{ConfirmWrites: confirm})
		if err != nil {
			return nil, err
		}
		bash, err := shell.New(root, shell.Options{SkipConfirmation: !confirm})
		if err != nil {
			return nil, err
		}
		tools = append(files, bash)
	} else {
		files, err := fs.ReadOnlyTools(root)
		if err != nil {
			return nil, err
		}
		tools = files
	}
	github, err := gh.NewTool(ghc)
	if err != nil {
		return nil, err
	}
	tools = append(tools, github)

	// Defence in depth: registration must match the profile table exactly.
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	slices.Sort(names)
	if want := ToolNames(mode, allowWrites); !slices.Equal(names, want) {
		return nil, fmt.Errorf("app: %v tools %v do not match the profile %v", mode, names, want)
	}
	return tools, nil
}

// ModelName returns the name of the model the next turn uses.
func (a *App) ModelName() string { return a.model.Name() }

// SetModel switches to the named model for the next turn. Sessions and their
// history are kept.
func (a *App) SetModel(ctx context.Context, name string) error {
	m, err := a.newModel(ctx, name)
	if err != nil {
		return err
	}
	ag, err := agent.New(agent.Options{Model: m, Tools: a.Tools, Workdir: a.Workdir})
	if err != nil {
		return err
	}
	r, err := agent.NewRunner(ag, a.Sessions, agent.WithCompaction(a.CompactionInterval))
	if err != nil {
		return err
	}
	a.model, a.Agent, a.Runner = m, ag, r
	return nil
}

// NewSession creates a session without a first message and returns its ID.
func (a *App) NewSession(ctx context.Context) (string, error) {
	return a.CreateSession(ctx, "", "")
}

// CreateSession creates the session id, or one with a new ID when id is
// empty, and returns its ID. The session records the workdir, the mode, the
// gofer version, and firstMessage, the user's first message, so that it can
// be found and listed later (docs/specs/cli-modes.md §6).
func (a *App) CreateSession(ctx context.Context, id, firstMessage string) (string, error) {
	workdir, err := sessions.Workdir(a.Workdir)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	meta := sessions.Meta{Workdir: workdir, Mode: a.Mode.String(), Version: a.version, FirstMessage: firstMessage}
	resp, err := a.Sessions.Create(ctx, &session.CreateRequest{
		AppName:   agent.Name,
		UserID:    UserID,
		SessionID: id,
		State:     meta.State(),
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return resp.Session.ID(), nil
}
