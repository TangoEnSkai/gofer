package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/gatherers/github"
	"github.com/TangoEnSkai/gofer/internal/launchd"
	"github.com/TangoEnSkai/gofer/internal/notify"
	"github.com/TangoEnSkai/gofer/internal/routine"
	"github.com/TangoEnSkai/gofer/internal/runs"
	"github.com/TangoEnSkai/gofer/internal/tools/gh"
)

// routineEnv is what `gofer routine` and doctor's routine checks touch
// outside the process, besides what app already injects (API key, model,
// limiter). Zero fields use the real system.
type routineEnv struct {
	gh       gh.Runner       // nil: the gh binary
	notifier notify.Notifier // nil: osascript
	launchd  launchd.Manager // zero: ~/Library/LaunchAgents and launchctl
	runsDir  string          // "": runs.DefaultDir()
	// executable returns the gofer path for launchd; nil means
	// launchd.StableExecutable.
	executable func() (string, error)
	// keychain looks up the API key in the Keychain only, as a launchd job
	// has no GEMINI_API_KEY; nil means the real Keychain.
	keychain func(context.Context) (credentials.Credential, error)
	now      func() time.Time // nil: time.Now
}

// routineEnvKey is the context key under which tests supply a *routineEnv.
type routineEnvKey struct{}

// routineEnvFrom returns the routineEnv in ctx, or the real system's, with
// defaults filled in.
func routineEnvFrom(ctx context.Context) routineEnv {
	var e routineEnv
	if p, ok := ctx.Value(routineEnvKey{}).(*routineEnv); ok && p != nil {
		e = *p
	}
	if e.notifier == nil {
		e.notifier = notify.OSAScript{}
	}
	if e.executable == nil {
		e.executable = launchd.StableExecutable
	}
	if e.keychain == nil {
		e.keychain = credentials.Resolver{Getenv: func(string) string { return "" }}.Resolve
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e
}

// store returns the run history store.
func (e routineEnv) store() (runs.Store, error) {
	dir := e.runsDir
	if dir == "" {
		var err error
		if dir, err = runs.DefaultDir(); err != nil {
			return runs.Store{}, err
		}
	}
	return runs.Store{Dir: dir}, nil
}

// registry returns the registered gatherers.
func (e routineEnv) registry() (*routine.Registry, error) {
	var reg routine.Registry
	if err := reg.Register(github.New(gh.New(e.gh))); err != nil {
		return nil, err
	}
	return &reg, nil
}

func newRoutineCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routine",
		Short: "Schedule and run routines (gather with gh, judge with one model call)",
		Long: "A routine is a YAML spec in $XDG_CONFIG_HOME/gofer/routines (default\n" +
			"~/.config/gofer/routines). `add` schedules it as a launchd agent; each run gathers\n" +
			"data in Go, makes one model call to judge it, saves a digest and a run record\n" +
			"under ~/.local/state/gofer/runs/<name>/, and may post a macOS notification.",
		Example: "  gofer routine add pr-digest --from-bundled\n" +
			"  gofer routine run pr-digest --dry-run\n" +
			"  gofer routine show pr-digest",
		Args: noArgs,
	}
	cmd.AddCommand(
		newRoutineAddCmd(a),
		newRoutineListCmd(),
		newRoutineRemoveCmd(),
		newRoutineRunCmd(a),
		newRoutineLogsCmd(),
		newRoutineShowCmd(),
	)
	return cmd
}

// noArgs and oneName validate positional arguments as usage errors.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	return nil
}

func oneName(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return usageError(fmt.Errorf("%s takes one routine name, got %d arguments", cmd.CommandPath(), len(args)))
	}
	if err := routine.ValidateName(args[0]); err != nil {
		return usageError(err)
	}
	return nil
}

func newRoutineAddCmd(a *app) *cobra.Command {
	var fromBundled, force bool
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Schedule a routine with launchd",
		Long: "Add validates <name>.yaml in the routines directory and installs a launchd agent\n" +
			"that runs `gofer routine run <name>` on its schedule. With --from-bundled it\n" +
			"first copies the spec shipped with gofer. Adding again updates the agent.",
		Example: "  gofer routine add pr-digest --from-bundled",
		Args:    oneName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.routineAdd(cmd, args[0], fromBundled, force)
		},
	}
	cmd.Flags().BoolVar(&fromBundled, "from-bundled", false, "copy the spec bundled with gofer into the routines directory first")
	cmd.Flags().BoolVar(&force, "force", false, "with --from-bundled, overwrite an existing spec that differs from the bundled one")
	return cmd
}

func (a *app) routineAdd(cmd *cobra.Command, name string, fromBundled, force bool) error {
	ctx, out, errOut := cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	env := routineEnvFrom(ctx)
	dir, err := routine.DefaultDir()
	if err != nil {
		return err
	}
	if fromBundled {
		path, copied, err := copyBundled(dir, name, force)
		if err != nil {
			return err
		}
		if copied {
			fmt.Fprintf(out, "copied the bundled spec to %s\n", path)
		}
	}
	spec, err := loadSpec(dir, name)
	if err != nil {
		return err
	}
	reg, err := env.registry()
	if err != nil {
		return err
	}
	g, ok := reg.Get(spec.Gatherer)
	if !ok {
		return fmt.Errorf("routine %s: unknown gatherer %q (registered: %s)", name, spec.Gatherer, strings.Join(reg.Names(), ", "))
	}
	if err := g.Validate(spec.With); err != nil {
		return fmt.Errorf("routine %s: invalid with: %w", name, err)
	}
	schedule, err := launchd.ParseSchedule(spec.Schedule)
	if err != nil {
		return fmt.Errorf("routine %s: %w", name, err)
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	logPath, err := launchd.DefaultLogPath(name)
	if err != nil {
		return err
	}
	job := launchd.Job{
		Name:       name,
		Executable: exe,
		Args:       a.jobArgs(name),
		Schedule:   schedule,
		LogPath:    logPath,
		Env:        xdgEnv(),
	}
	if err := env.launchd.Install(ctx, job); err != nil {
		// Do not leave a plist that launchd would load at the next login.
		if uerr := env.launchd.Uninstall(ctx, name); uerr != nil {
			return errors.Join(err, fmt.Errorf("remove the half-installed agent: %w", uerr))
		}
		return err
	}
	plist, err := env.launchd.PlistPath(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scheduled %s (%s, local time) as %s\n", name, spec.Schedule, launchd.Label(name))
	fmt.Fprintf(out, "  agent: %s\n  runs:  %s\n  log:   %s\n", plist, strings.Join(append([]string{exe}, job.Args...), " "), logPath)

	if launchd.VersionedPath(exe) {
		fmt.Fprintf(errOut, "gofer: warning: %s is a versioned path that the next upgrade removes; add the routine again from the package manager's symlink (e.g. /opt/homebrew/bin/gofer)\n", exe)
	}
	if _, err := env.keychain(ctx); err != nil {
		fmt.Fprintf(errOut, "gofer: warning: scheduled runs read the API key from the Keychain, and it has none (%v); store it with: security add-generic-password -s %s -a %s -w\n",
			err, credentials.KeychainService, credentials.KeychainAccount)
	}
	return nil
}

// jobArgs are the arguments launchd passes to gofer. An explicit --config is
// kept, as an absolute path.
func (a *app) jobArgs(name string) []string {
	var args []string
	if a.configPath != "" {
		path, err := filepath.Abs(a.configPath)
		if err != nil {
			path = a.configPath
		}
		args = append(args, "--config", path)
	}
	return append(args, "routine", "run", name)
}

// xdgEnv passes the XDG base directories gofer resolves paths from, so the
// launchd job finds the same routines and writes the same history as the
// shell that added it.
func xdgEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if v := os.Getenv(k); filepath.IsAbs(v) {
			env[k] = v
		}
	}
	return env
}

// copyBundled writes the bundled spec of name to dir. An existing different
// spec is kept unless force is set. It reports whether it wrote the file.
func copyBundled(dir, name string, force bool) (path string, copied bool, err error) {
	data, err := fs.ReadFile(routine.Bundled, "bundled/"+name+".yaml")
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, usageError(fmt.Errorf("no bundled routine %q (bundled: %s)", name, strings.Join(bundledNames(), ", ")))
	}
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, name+".yaml")
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, data):
		return path, false, nil
	case err == nil && !force:
		return "", false, fmt.Errorf("%s exists and differs from the bundled spec; drop --from-bundled to keep it, or add --force to overwrite it", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func bundledNames() []string {
	specs, _ := routine.LoadFS(routine.Bundled, "bundled")
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

// loadSpec loads a spec and, when it does not exist but a bundled one does,
// says how to add it.
func loadSpec(dir, name string) (routine.Spec, error) {
	spec, err := routine.Load(dir, name)
	if errors.Is(err, fs.ErrNotExist) {
		if _, berr := fs.Stat(routine.Bundled, "bundled/"+name+".yaml"); berr == nil {
			return spec, fmt.Errorf("%w; add the bundled routine with: gofer routine add %s --from-bundled", err, name)
		}
	}
	return spec, err
}

func newRoutineListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List routines with their launchd state and last run",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return routineList(cmd)
		},
	}
}

func routineList(cmd *cobra.Command) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	env := routineEnvFrom(ctx)
	dir, err := routine.DefaultDir()
	if err != nil {
		return err
	}
	specs, loadErr := routine.LoadDir(dir)
	store, err := env.store()
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Fprintf(out, "no routines in %s\nadd one with: gofer routine add <name> --from-bundled (bundled: %s)\n", dir, strings.Join(bundledNames(), ", "))
		return loadErr
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCHEDULE\tNOTIFY\tLAUNCHD\tLAST RUN")
	for _, s := range specs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Schedule, s.Notify, launchdState(ctx, env.launchd, s.Name), lastRun(store, s.Name))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return loadErr
}

// launchdState describes a routine's agent in a few words.
func launchdState(ctx context.Context, m launchd.Manager, name string) string {
	st, err := m.Status(ctx, name)
	switch {
	case err != nil:
		return "error: " + firstLine(err.Error())
	case st.Loaded && st.LastExitCode != nil:
		return fmt.Sprintf("loaded, last exit %d", *st.LastExitCode)
	case st.Loaded:
		return "loaded"
	case st.Installed:
		return "installed, not loaded"
	}
	return "not scheduled"
}

// lastRun describes a routine's newest run record.
func lastRun(store runs.Store, name string) string {
	recs, err := store.List(name, 1)
	switch {
	case err != nil:
		return "error: " + firstLine(err.Error())
	case len(recs) == 0:
		return "never"
	}
	return recs[0].Status + " " + recs[0].StartedAt.Local().Format("2006-01-02 15:04")
}

func newRoutineRemoveCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Unschedule a routine",
		Long:  "Remove unloads the routine's launchd agent and deletes its plist. The spec and\nrun history are kept; --purge also deletes the spec.",
		Args:  oneName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return routineRemove(cmd, args[0], purge)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the routine's spec")
	return cmd
}

func routineRemove(cmd *cobra.Command, name string, purge bool) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	env := routineEnvFrom(ctx)
	if err := env.launchd.Uninstall(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "unscheduled %s\n", name)
	dir, err := routine.DefaultDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".yaml")
	switch {
	case !fileExists(path):
	case purge:
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted %s\n", path)
	default:
		fmt.Fprintf(out, "kept %s (delete it with --purge)\n", path)
	}
	return nil
}

func newRoutineLogsCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show a routine's recent runs",
		Args:  oneName,
		RunE: func(cmd *cobra.Command, args []string) error {
			return routineLogs(cmd, args[0], n)
		},
	}
	cmd.Flags().IntVarP(&n, "number", "n", 5, "number of runs to show; 0 shows all")
	return cmd
}

func routineLogs(cmd *cobra.Command, name string, n int) error {
	out := cmd.OutOrStdout()
	if n < 0 {
		return usageError(fmt.Errorf("-n must not be negative, got %d", n))
	}
	store, err := routineEnvFrom(cmd.Context()).store()
	if err != nil {
		return err
	}
	recs, err := store.List(name, n)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Fprintf(out, "no runs recorded for %s\n", name)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tTIME\tCALLS\tTOKENS\tRESULT")
	for _, r := range recs {
		result := notify.Summary(r.Counts)
		if r.Status == runs.StatusFailed {
			result = firstLine(r.Error)
		} else if len(r.ItemErrors) > 0 {
			result += fmt.Sprintf(" (%d item errors)", len(r.ItemErrors))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.Status, r.Duration().Round(100*time.Millisecond), r.ModelCalls, r.Tokens.Total, result)
	}
	return tw.Flush()
}

func newRoutineShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print a routine's latest digest",
		Args:  oneName,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := routineEnvFrom(cmd.Context()).store()
			if err != nil {
				return err
			}
			_, digest, err := store.Latest(args[0])
			if errors.Is(err, runs.ErrNoRuns) {
				return fmt.Errorf("no runs recorded for %s yet; run it with: gofer routine run %s", args[0], args[0])
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), digest)
			return err
		},
	}
}

// firstLine returns the first line of s, shortened for a table cell or a
// notification.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:159]) + "…"
	}
	return s
}
