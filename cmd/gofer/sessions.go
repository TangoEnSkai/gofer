package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/adk/v2/session"

	"github.com/TangoEnSkai/gofer/internal/sessions"
)

// maxListedMessage caps, in runes, the first message shown by sessions list.
const maxListedMessage = 60

// openSessions opens the session store and prunes sessions older than
// sessions.Retention. Pruning is best effort: a failure is only reported.
func openSessions(ctx context.Context, stderr io.Writer) (*sessions.Store, error) {
	path, err := sessions.DefaultPath()
	if err != nil {
		return nil, err
	}
	store, err := sessions.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := store.Prune(ctx, sessions.Retention); err != nil {
		fmt.Fprintln(stderr, "gofer: warning: pruning old sessions:", err)
	}
	return store, nil
}

// checkResumeFlags rejects contradictory --continue, --resume, and
// --force-workdir values.
func (a *app) checkResumeFlags(cmd *cobra.Command) error {
	switch {
	case cmd.Flags().Changed("resume") && a.resume == "":
		return usageError(errors.New("--resume needs a session ID; see gofer sessions list"))
	case a.continueLast && a.resume != "":
		return usageError(errors.New("--continue and --resume cannot be used together"))
	case a.forceWorkdir && a.resume == "":
		return usageError(errors.New("--force-workdir only applies to --resume"))
	}
	return nil
}

// resumeTarget returns the ID of the session that --continue or --resume
// selects for workdir, or "" to start a new session.
func (a *app) resumeTarget(ctx context.Context, store *sessions.Store, workdir string, stderr io.Writer) (string, error) {
	if !a.continueLast && a.resume == "" {
		return "", nil
	}
	dir, err := sessions.Workdir(workdir)
	if err != nil {
		return "", err
	}
	if a.continueLast {
		info, ok, err := store.Latest(ctx, dir)
		if err != nil {
			return "", err
		}
		if !ok {
			fmt.Fprintf(stderr, "gofer: no earlier session in %s; starting a new one\n", dir)
			return "", nil
		}
		return info.ID, nil
	}
	info, err := store.Find(ctx, a.resume)
	if errors.Is(err, session.ErrNotFound) {
		return "", usageError(fmt.Errorf("no session %s; see gofer sessions list --all", a.resume))
	}
	if err != nil {
		return "", err
	}
	if info.Workdir != dir && !a.forceWorkdir {
		return "", usageError(fmt.Errorf("session %s was started in %s, not %s: run gofer there, or pass --force-workdir to resume it here", info.ID, info.Workdir, dir))
	}
	return info.ID, nil
}

func newSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List or remove saved sessions",
		Long: "gofer saves every session to $XDG_STATE_HOME/gofer/sessions.db (default\n" +
			"~/.local/state/gofer/sessions.db) and deletes sessions not used for 30 days.\n" +
			"Resume one with gofer --continue (the latest in the current directory) or\n" +
			"gofer --resume <id>.",
	}
	var all bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the sessions of the current directory, newest first",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listSessions(cmd.Context(), all, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	list.Flags().BoolVar(&all, "all", false, "list the sessions of every directory")
	rm := &cobra.Command{
		Use:   "rm <id>...",
		Short: "Delete sessions and their history",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, ids []string) error {
			return removeSessions(cmd.Context(), ids, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.AddCommand(list, rm)
	return cmd
}

// usageArgs makes the argument errors of v usage errors (exit code 2).
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return usageError(err)
		}
		return nil
	}
}

// listSessions prints the sessions of the current directory, or of every
// directory with all, as a table.
func listSessions(ctx context.Context, all bool, stdout, stderr io.Writer) error {
	store, err := openSessions(ctx, stderr)
	if err != nil {
		return err
	}
	defer store.Close()
	dir := ""
	if !all {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		if dir, err = sessions.Workdir(wd); err != nil {
			return err
		}
	}
	infos, err := store.List(ctx, dir)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		if all {
			fmt.Fprintln(stderr, "No saved sessions.")
		} else {
			fmt.Fprintf(stderr, "No saved sessions in %s. Use --all to list every directory.\n", dir)
		}
		return nil
	}
	now := time.Now()
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPDATED\tWORKDIR\tFIRST MESSAGE")
	for _, s := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, ago(now.Sub(s.Updated)), tildePath(s.Workdir, home), oneLine(s.FirstMessage, maxListedMessage))
	}
	return tw.Flush()
}

// removeSessions deletes the sessions with ids. A missing ID is a usage
// error, reported after the others are deleted.
func removeSessions(ctx context.Context, ids []string, stdout, stderr io.Writer) error {
	store, err := openSessions(ctx, stderr)
	if err != nil {
		return err
	}
	defer store.Close()
	var missing []string
	for _, id := range ids {
		err := store.Delete(ctx, id)
		if errors.Is(err, session.ErrNotFound) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted session %s.\n", id)
	}
	if len(missing) > 0 {
		return usageError(fmt.Errorf("no session %s; see gofer sessions list --all", strings.Join(missing, ", ")))
	}
	return nil
}

// ago formats d as a coarse age, such as "5m ago".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// tildePath abbreviates a path inside home with "~".
func tildePath(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}

// oneLine collapses whitespace in s and shortens it to at most n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
