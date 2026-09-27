package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/agent"
	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/config"
)

const replHelp = `Type a request and press Enter. Commands:
  /help          show this help
  /clear         start a new session
  /session       print the session ID and how to resume it
  /model [name]  print the model, or switch to name from the next turn
  /exit          quit (or press Ctrl-D)
Ctrl-C cancels the running turn.`

// interactive runs the REPL (docs/specs/cli-modes.md §4).
func (a *app) interactive(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
	store, err := openSessions(ctx, stderr)
	if err != nil {
		return err
	}
	defer store.Close()
	ga, err := a.build(ctx, cfg, goferapp.Interactive, store.Service)
	if err != nil {
		return err
	}
	resumed, err := a.resumeTarget(ctx, store, ga.Workdir, stderr)
	if err != nil {
		return err
	}
	sigs, stop := a.interrupts()
	defer stop()
	r := newREPL(ga, a.stdin, stdout, stderr)
	r.sessionID, r.saved = resumed, resumed != ""
	return r.run(ctx, sigs)
}

// repl is a line-based chat loop. Tool calls that need confirmation are
// asked about on the same input.
type repl struct {
	app    *goferapp.App
	in     *bufio.Scanner
	out    io.Writer // shared with the interrupt handler, so writes are serialized
	errOut io.Writer

	sessionID string
	// saved reports whether the session is in the store. A new session is
	// saved with its first message, so that an unused one leaves no trace.
	saved  bool
	always map[string]bool // approvalKey of calls approved for the session
	eof    bool            // input is exhausted

	midLine  bool // out does not end with a newline
	streamed bool // text of the current model response was printed as it streamed

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the running turn; nil when idle
}

func newREPL(ga *goferapp.App, in io.Reader, out, errOut io.Writer) *repl {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	return &repl{
		app:    ga,
		in:     sc,
		out:    &syncWriter{w: out},
		errOut: errOut,
		always: make(map[string]bool),
	}
}

// run reads and handles lines until /exit or the end of input. Interrupts
// cancel the running turn; when idle they print a hint.
func (r *repl) run(ctx context.Context, sigs <-chan os.Signal) error {
	resumed := r.sessionID != ""
	if !resumed {
		r.sessionID = uuid.NewString()
	}

	done := make(chan struct{})
	defer close(done)
	go r.handleInterrupts(sigs, done)

	fmt.Fprintf(r.out, "gofer %s · %s · %s\n", version, r.app.ModelName(), r.app.Workdir)
	if resumed {
		fmt.Fprintf(r.out, "Resumed session %s.\n", r.sessionID)
	}
	fmt.Fprintln(r.out, "Type /help for commands, /exit to quit.")
	for !r.eof {
		fmt.Fprint(r.out, "> ")
		if !r.in.Scan() {
			fmt.Fprintln(r.out)
			break
		}
		line := strings.TrimSpace(r.in.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "/"):
			if !r.command(ctx, line) {
				return nil
			}
		default:
			r.turn(ctx, line)
		}
	}
	return r.in.Err()
}

func (r *repl) handleInterrupts(sigs <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-sigs:
			r.mu.Lock()
			if r.cancel != nil {
				r.cancel()
			} else {
				fmt.Fprint(r.out, "\n(type /exit or press Ctrl-D to quit)\n> ")
			}
			r.mu.Unlock()
		}
	}
}

// command runs a slash command and reports whether the REPL continues.
func (r *repl) command(ctx context.Context, line string) bool {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "/exit":
		return false
	case "/help":
		fmt.Fprintln(r.out, replHelp)
	case "/clear":
		prev, saved := r.sessionID, r.saved
		r.sessionID, r.saved = uuid.NewString(), false
		fmt.Fprintf(r.out, "Started a new session: %s\n", r.sessionID)
		if saved {
			fmt.Fprintf(r.out, "Resume the previous one with: gofer --resume %s\n", prev)
		}
	case "/session":
		if r.saved {
			fmt.Fprintf(r.out, "%s\nResume it with: gofer --resume %s\n", r.sessionID, r.sessionID)
		} else {
			fmt.Fprintf(r.out, "%s (saved with your first message)\n", r.sessionID)
		}
	case "/model":
		if arg == "" {
			fmt.Fprintln(r.out, r.app.ModelName())
			break
		}
		if err := r.app.SetModel(ctx, arg); err != nil {
			fmt.Fprintln(r.errOut, "error:", err)
			break
		}
		fmt.Fprintf(r.out, "The next turn uses %s.\n", r.app.ModelName())
	default:
		fmt.Fprintf(r.out, "Unknown command %s. Type /help for commands.\n", name)
	}
	return true
}

// turn sends prompt and answers the confirmations it pauses on, until the
// model is done or the turn is canceled.
func (r *repl) turn(ctx context.Context, prompt string) {
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancel = nil
		r.mu.Unlock()
		cancel()
	}()

	if !r.saved {
		if _, err := r.app.CreateSession(ctx, r.sessionID, prompt); err != nil {
			fmt.Fprintln(r.errOut, "error:", err)
			return
		}
		r.saved = true
	}
	res, err := agent.Ask(ctx, r.app.Runner, goferapp.UserID, r.sessionID, prompt, r.onEvent, agent.Streaming())
	err = r.compacted(err)
	for err == nil && len(res.PendingConfirmations) > 0 {
		// ADK resumes the model on the first answer it gets, so all of a
		// turn's pending confirmations are answered in one message.
		decisions := r.confirm(ctx, res.PendingConfirmations)
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		res, err = agent.ConfirmAll(ctx, r.app.Runner, goferapp.UserID, r.sessionID, decisions, r.onEvent, agent.Streaming())
		err = r.compacted(err)
	}
	r.endLine()
	switch {
	case ctx.Err() != nil:
		fmt.Fprintln(r.out, "(turn canceled)")
	case err != nil:
		fmt.Fprintln(r.errOut, "error:", err)
	}
}

// compacted reports a failed context compaction as a warning and drops it:
// the turn itself is complete and saved, only its history was not shortened.
func (r *repl) compacted(err error) error {
	if errors.Is(err, compaction.ErrCompaction) {
		r.endLine()
		fmt.Fprintln(r.errOut, "warning:", err)
		return nil
	}
	return err
}

// onEvent prints model text as it streams and tool calls as one-line
// summaries.
func (r *repl) onEvent(ev *session.Event) {
	if ev.Content == nil || ev.Content.Role != genai.RoleModel {
		return
	}
	if ev.Partial {
		for _, p := range ev.Content.Parts {
			if p.Text != "" && !p.Thought {
				r.print(p.Text)
				r.streamed = true
			}
		}
		return
	}
	for _, p := range ev.Content.Parts {
		switch {
		case p.FunctionCall != nil && p.FunctionCall.Name != toolconfirmation.FunctionCallName:
			r.endLine()
			fmt.Fprintf(r.out, "▸ %s\n", callLine(p.FunctionCall))
		case p.Text != "" && !p.Thought && !r.streamed:
			r.print(p.Text)
		}
	}
	r.streamed = false
}

// confirm asks about each pending confirmation, unless an identical call was
// approved with "always", and returns the decisions.
func (r *repl) confirm(ctx context.Context, pending []*genai.FunctionCall) []agent.Decision {
	decisions := make([]agent.Decision, 0, len(pending))
	for _, call := range pending {
		d := agent.Decision{Call: call}
		orig, err := toolconfirmation.OriginalCallFrom(call)
		switch {
		case err != nil:
			fmt.Fprintln(r.errOut, "error: rejecting a confirmation request:", err)
		case r.always[approvalKey(orig)]:
			fmt.Fprintf(r.out, "▸ approved (always): %s\n", callLine(orig))
			d.Approved = true
		case ctx.Err() == nil:
			d.Approved = r.ask(orig)
		}
		decisions = append(decisions, d)
	}
	return decisions
}

// ask shows call in full and reads y, n, or a. The end of input rejects.
func (r *repl) ask(call *genai.FunctionCall) bool {
	r.endLine()
	fmt.Fprintf(r.out, "? %s\n", describeCall(call))
	for !r.eof {
		fmt.Fprint(r.out, "Allow? [y]es / [n]o / [a]lways: ")
		if !r.in.Scan() {
			r.eof = true
			fmt.Fprintln(r.out)
			break
		}
		switch strings.ToLower(strings.TrimSpace(r.in.Text())) {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		case "a", "always":
			r.always[approvalKey(call)] = true
			return true
		}
		fmt.Fprintln(r.out, "Please answer y, n, or a.")
	}
	return false
}

func (r *repl) print(s string) {
	fmt.Fprint(r.out, s)
	r.midLine = !strings.HasSuffix(s, "\n")
}

// endLine ends a line of streamed text.
func (r *repl) endLine() {
	if r.midLine {
		fmt.Fprintln(r.out)
		r.midLine = false
	}
}

// syncWriter serializes writes to w.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
