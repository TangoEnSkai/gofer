package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode/utf8"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/agent"
	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/config"
)

// Headless output formats (--output).
const (
	outputText       = "text"
	outputJSON       = "json"
	outputStreamJSON = "stream-json"
)

// maxStdin caps the piped input added to a headless prompt.
const maxStdin = 256 << 10

// maxDeniedRounds bounds how often a headless run answers new confirmation
// requests with a rejection before it stops.
const maxDeniedRounds = 5

// headlessResult is the --output json object, and the last stream-json event.
type headlessResult struct {
	Result              string `json:"result"`
	SessionID           string `json:"session_id"`
	ToolCalls           int    `json:"tool_calls"`
	Usage               usage  `json:"usage"`
	DeniedConfirmations int    `json:"denied_confirmations"`
	Error               string `json:"error,omitempty"`
}

// streamEvent is one --output stream-json line before the final result.
// Type is start, tool_call, tool_result, text, or denied_confirmation.
type streamEvent struct {
	Type      string         `json:"type"`
	SessionID string         `json:"session_id,omitempty"`
	Model     string         `json:"model,omitempty"`
	Tools     []string       `json:"tools,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	Response  map[string]any `json:"response,omitempty"`
	Text      string         `json:"text,omitempty"`
}

// usage is the token usage of a run, summed over model responses.
type usage struct {
	PromptTokens   int32 `json:"prompt_tokens"`
	CachedTokens   int32 `json:"cached_tokens"`
	OutputTokens   int32 `json:"output_tokens"`
	ThoughtsTokens int32 `json:"thoughts_tokens"`
	TotalTokens    int32 `json:"total_tokens"`
}

func (u *usage) add(m *genai.GenerateContentResponseUsageMetadata) {
	if m == nil {
		return
	}
	u.PromptTokens += m.PromptTokenCount
	u.CachedTokens += m.CachedContentTokenCount
	u.OutputTokens += m.CandidatesTokenCount
	u.ThoughtsTokens += m.ThoughtsTokenCount
	u.TotalTokens += m.TotalTokenCount
}

// headless runs one prompt without prompting anyone (docs/specs/cli-modes.md
// §3). A confirmation request is rejected and makes the run exit with 3.
func (a *app) headless(ctx context.Context, cfg config.Config, hasPrompt bool, stdout, stderr io.Writer) error {
	ga, err := a.build(ctx, cfg, goferapp.Headless)
	if err != nil {
		return err
	}
	prompt := a.prompt
	if a.term.stdinPiped {
		in, truncated, err := readLimited(a.stdin, maxStdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		prompt = composePrompt(prompt, hasPrompt, in, truncated)
	}
	if strings.TrimSpace(prompt) == "" {
		return usageError(errors.New("the prompt is empty"))
	}
	if a.allowWrites {
		fmt.Fprintln(stderr, "gofer: warning: --allow-writes lets the model write files and run shell commands without confirmation")
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	sid, err := ga.NewSession(ctx)
	if err != nil {
		return err
	}

	h := &headlessRun{out: stdout, format: a.output, result: headlessResult{SessionID: sid}}
	if a.term.stderrTTY && a.output == outputText {
		h.progress = stderr
	}
	h.emit(streamEvent{Type: "start", SessionID: sid, Model: ga.ModelName(), Tools: toolNames(ga)})

	res, err := agent.Ask(ctx, ga.Runner, goferapp.UserID, sid, prompt, h.onEvent)
	h.add(res)
	// Defence in depth: no headless profile registers a tool that asks for
	// confirmation, but if one does, reject it and let the model finish.
	for round := 0; err == nil && len(res.PendingConfirmations) > 0; round++ {
		decisions := make([]agent.Decision, len(res.PendingConfirmations))
		for i, call := range res.PendingConfirmations {
			decisions[i] = agent.Decision{Call: call}
			h.denied(call)
		}
		if round == maxDeniedRounds {
			break // the model keeps asking; the run ends here
		}
		res, err = agent.ConfirmAll(ctx, ga.Runner, goferapp.UserID, sid, decisions, h.onEvent)
		h.add(res)
	}
	if err != nil {
		h.result.Error = err.Error()
	}
	if werr := h.finish(); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	if n := h.result.DeniedConfirmations; n > 0 {
		msg := fmt.Sprintf("rejected %d tool call(s) that needed confirmation, which headless mode cannot ask for; run gofer interactively", n)
		if !a.allowWrites {
			msg += ", or pass --allow-writes to allow writes and shell commands"
		}
		return &codeError{exitDenied, errors.New(msg)}
	}
	return nil
}

// headlessRun collects the outcome of a headless run and writes its output.
type headlessRun struct {
	out      io.Writer
	format   string
	progress io.Writer // tool call lines; nil when silent
	result   headlessResult
	err      error // first write error
}

// add folds one agent result into the run's result.
func (h *headlessRun) add(res agent.Result) {
	if res.Text != "" {
		h.result.Result = res.Text
	}
	h.result.ToolCalls += res.ToolCalls
	h.result.Usage.add(res.Usage)
}

// denied records a confirmation request that is being rejected.
func (h *headlessRun) denied(call *genai.FunctionCall) {
	h.result.DeniedConfirmations++
	ev := streamEvent{Type: "denied_confirmation", ID: call.ID}
	line := "confirmation request"
	if orig, err := toolconfirmation.OriginalCallFrom(call); err == nil {
		ev.ID, ev.Name, ev.Args = orig.ID, orig.Name, orig.Args
		line = callLine(orig)
	}
	h.emit(ev)
	if h.progress != nil {
		fmt.Fprintf(h.progress, "▸ denied %s\n", line)
	}
}

// onEvent reports settled events as progress lines or stream-json events.
func (h *headlessRun) onEvent(ev *session.Event) {
	if ev.Partial || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		switch {
		case p.FunctionCall != nil && p.FunctionCall.Name != toolconfirmation.FunctionCallName:
			fc := p.FunctionCall
			h.emit(streamEvent{Type: "tool_call", ID: fc.ID, Name: fc.Name, Args: fc.Args})
			if h.progress != nil {
				fmt.Fprintf(h.progress, "▸ %s\n", callLine(fc))
			}
		case p.FunctionResponse != nil && p.FunctionResponse.Name != toolconfirmation.FunctionCallName:
			fr := p.FunctionResponse
			h.emit(streamEvent{Type: "tool_result", ID: fr.ID, Name: fr.Name, Response: fr.Response})
		case p.Text != "" && !p.Thought && ev.Content.Role == genai.RoleModel:
			h.emit(streamEvent{Type: "text", Text: p.Text})
		}
	}
}

// emit writes a stream-json event; other formats ignore it.
func (h *headlessRun) emit(ev streamEvent) {
	if h.format == outputStreamJSON {
		h.writeJSON(ev)
	}
}

// finish writes the final output in the chosen format.
func (h *headlessRun) finish() error {
	switch h.format {
	case outputJSON:
		h.writeJSON(h.result)
	case outputStreamJSON:
		h.writeJSON(struct {
			Type string `json:"type"`
			headlessResult
		}{"result", h.result})
	default:
		if r := h.result.Result; r != "" {
			if !strings.HasSuffix(r, "\n") {
				r += "\n"
			}
			_, h.err = io.WriteString(h.out, r)
		}
	}
	if h.err != nil {
		return fmt.Errorf("write output: %w", h.err)
	}
	return nil
}

func (h *headlessRun) writeJSON(v any) {
	if h.err != nil {
		return
	}
	enc := json.NewEncoder(h.out)
	enc.SetEscapeHTML(false)
	h.err = enc.Encode(v)
}

// readLimited reads r up to max bytes, reporting whether more followed. The
// cut falls on a UTF-8 rune boundary.
func readLimited(r io.Reader, max int) (string, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return "", false, err
	}
	if len(data) <= max {
		return string(data), false, nil
	}
	n := max
	for n > 0 && !utf8.RuneStart(data[n]) {
		n--
	}
	return string(data[:n]), true, nil
}

// composePrompt adds piped input to the -p prompt as a fenced block labelled
// stdin. Without -p, the input is the prompt.
func composePrompt(prompt string, hasPrompt bool, in string, truncated bool) string {
	note := ""
	if truncated {
		note = fmt.Sprintf("\n[gofer: stdin truncated to its first %d KiB]", maxStdin>>10)
	}
	if !hasPrompt {
		return in + note
	}
	if strings.TrimSpace(in) == "" {
		return prompt
	}
	// The fence is longer than any backtick run in the input, so the input
	// cannot close it.
	fence := strings.Repeat("`", max(3, longestRun(in, '`')+1))
	if !strings.HasSuffix(in, "\n") {
		in += "\n"
	}
	return prompt + "\n\n" + fence + "stdin\n" + in + fence + note
}

// longestRun returns the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	longest, n := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n++
			longest = max(longest, n)
		} else {
			n = 0
		}
	}
	return longest
}

// toolNames returns the names of the tools registered in ga.
func toolNames(ga *goferapp.App) []string {
	names := make([]string, len(ga.Tools))
	for i, t := range ga.Tools {
		names[i] = t.Name()
	}
	return names
}
