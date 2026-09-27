package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/TangoEnSkai/gofer/internal/notify"
	"github.com/TangoEnSkai/gofer/internal/ratelimit"
	"github.com/TangoEnSkai/gofer/internal/routine"
	"github.com/TangoEnSkai/gofer/internal/runs"
)

// runTimeout bounds one routine run, so a hung gh or model call cannot keep
// a launchd job alive until the next one starts.
const runTimeout = 15 * time.Minute

func newRoutineRunCmd(a *app) *cobra.Command {
	var dryRun, noNotify bool
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Run a routine once",
		Long: "Run gathers the routine's data, asks the model once to judge it, and saves the\n" +
			"digest and a run record, also when the run fails (exit code 1). It then posts a\n" +
			"notification per the routine's notify policy. --dry-run only gathers and prints\n" +
			"what the model would see; it needs no API key and saves nothing.",
		Example: "  gofer routine run pr-digest --dry-run\n  gofer routine run pr-digest --no-notify",
		Args:    oneName,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun {
				return routineDryRun(cmd, args[0])
			}
			return a.routineRun(cmd, args[0], noNotify)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "gather and print the judge's instruction and input; no model call")
	cmd.Flags().BoolVar(&noNotify, "no-notify", false, "do not post a notification")
	return cmd
}

// routineDryRun prints what the judge would see. It needs no API key.
func routineDryRun(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	env := routineEnvFrom(ctx)
	dir, err := routine.DefaultDir()
	if err != nil {
		return err
	}
	spec, err := loadSpec(dir, name)
	if err != nil {
		return err
	}
	reg, err := env.registry()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	res, err := routine.Run(ctx, spec, reg, nil, routine.RunOptions{DryRun: true, Deps: routine.Deps{Clock: env.now}})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "## Judge instruction\n\n%s\n\n## Judge input (%d bytes)\n\n%s\n", res.Instruction, len(res.Prompt), res.Prompt)
	fmt.Fprintf(cmd.ErrOrStderr(), "dry run of %s: gathered in %s; no model call, nothing saved\n", name, res.Duration.Round(time.Millisecond))
	return nil
}

// routineRun runs the named routine once. It always saves a run record and a
// digest, a short failure digest when the run fails, so that latest.md
// matches the newest run; then it notifies per the routine's policy.
func (a *app) routineRun(cmd *cobra.Command, name string, noNotify bool) error {
	ctx, out, errOut := cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	env := routineEnvFrom(ctx)
	store, err := env.store()
	if err != nil {
		return err
	}

	started, t0 := env.now(), time.Now()
	o := a.execRoutine(ctx, env, name)
	rec := o.record(name, started, started.Add(time.Since(t0)))
	digest := o.res.Digest.Markdown
	if o.err != nil {
		digest = failureDigest(name, started, o.err)
	}

	paths, saveErr := store.Save(rec, digest)
	if saveErr == nil {
		if err := store.Prune(name, runs.DefaultKeep); err != nil {
			fmt.Fprintf(errOut, "gofer: warning: prune run history: %v\n", err)
		}
	}
	if !noNotify {
		notifyRun(ctx, env.notifier, o.spec, name, rec, errOut)
	}

	fmt.Fprint(out, digest)
	if saveErr == nil {
		fmt.Fprintf(errOut, "%s: %s in %s · %d model call(s) · %d tokens · %s\n",
			name, rec.Status, rec.Duration().Round(100*time.Millisecond), rec.ModelCalls, rec.Tokens.Total, paths.Digest)
	}
	return errors.Join(o.err, saveErr)
}

// outcome is the result of executing a routine, successful or not.
type outcome struct {
	spec  *routine.Spec // nil when the spec could not be loaded
	model string
	res   routine.Result
	err   error
}

// execRoutine loads the config and spec, builds the rate-limited judge model,
// and runs the routine. Config and credential problems are usage errors
// (exit code 2), as for the other commands.
func (a *app) execRoutine(ctx context.Context, env routineEnv, name string) (o outcome) {
	cfg, err := a.loadConfig()
	if err != nil {
		o.err = usageError(err)
		return o
	}
	dir, err := routine.DefaultDir()
	if err != nil {
		o.err = err
		return o
	}
	spec, err := loadSpec(dir, name)
	if err != nil {
		o.err = err
		return o
	}
	o.spec = &spec
	// --model beats the spec, which beats the config file.
	o.model = cfg.Model
	if spec.Model != "" && a.model == "" {
		o.model = spec.Model
	}

	cred, err := a.resolveCredential(ctx)
	if err != nil {
		o.err = usageError(fmt.Errorf("%w; %s", err, keyHint))
		return o
	}
	m, err := a.newModel(ctx, o.model, cred.Key)
	if err != nil {
		o.err = err
		return o
	}
	// One limiter per process (ratelimit package doc); a run has one judge.
	limiter := a.limiter
	if limiter == nil {
		limiter = ratelimit.NewLimiter(cfg.Limits.RequestsPerMinute)
	}
	reg, err := env.registry()
	if err != nil {
		o.err = err
		return o
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	o.res, o.err = routine.Run(ctx, spec, reg, ratelimit.Wrap(m, limiter), routine.RunOptions{Deps: routine.Deps{Clock: env.now}})
	return o
}

// record describes the run for the history store.
func (o outcome) record(name string, started, finished time.Time) runs.Record {
	d := o.res.Digest
	rec := runs.Record{
		Routine:    name,
		StartedAt:  started,
		FinishedAt: finished,
		Status:     runs.StatusOK,
		Model:      o.model,
		ModelCalls: o.res.ModelCalls,
		Counts:     d.Counts,
		Headline:   d.Headline,
		Version:    version,
	}
	if u := o.res.Usage; u != nil {
		rec.Tokens = runs.Tokens{
			Prompt:     int(u.PromptTokenCount),
			Candidates: int(u.CandidatesTokenCount),
			Total:      int(u.TotalTokenCount),
		}
	}
	for _, e := range d.ItemErrors {
		rec.ItemErrors = append(rec.ItemErrors, runs.ItemError{Target: e.Target, Error: e.Error})
	}
	switch {
	case o.err != nil:
		rec.Status, rec.Error, rec.Headline = runs.StatusFailed, o.err.Error(), "Run failed"
	case len(rec.ItemErrors) > 0:
		rec.Status = runs.StatusPartial
	}
	return rec
}

// failureDigest is the digest saved for a failed run.
func failureDigest(name string, started time.Time, err error) string {
	return fmt.Sprintf("# %s · run failed · %s\n\n```\n%v\n```\n\nCheck the setup with `gofer doctor`; recent runs: `gofer routine logs %s`.\n",
		name, started.Local().Format("Mon 2006-01-02 15:04"), err, name)
}

// notifyRun posts a notification when the routine's policy asks for one. A
// routine whose spec could not be loaded uses the default policy, on-action,
// which notifies on failure. A notifier error is only a warning.
func notifyRun(ctx context.Context, n notify.Notifier, spec *routine.Spec, name string, rec runs.Record, errOut io.Writer) {
	policy := notify.OnAction
	if spec != nil {
		if p, err := notify.ParsePolicy(string(spec.Notify)); err == nil {
			policy = p
		}
	}
	if !notify.ShouldNotify(policy, rec.Status, rec.Counts) {
		return
	}
	msg := notify.Notification{Title: "gofer · " + name, Subtitle: rec.Headline, Body: notify.Summary(rec.Counts)}
	if rec.Status == runs.StatusFailed {
		msg.Body = firstLine(rec.Error)
	}
	if err := n.Notify(ctx, msg); err != nil {
		fmt.Fprintf(errOut, "gofer: warning: %v\n", err)
	}
}
