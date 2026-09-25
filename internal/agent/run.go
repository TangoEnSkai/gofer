package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// NewRunner returns a runner for a that creates sessions on first use.
// A nil svc selects an in-memory session service.
func NewRunner(a adkagent.Agent, svc session.Service) (*runner.Runner, error) {
	if svc == nil {
		svc = session.InMemoryService()
	}
	return runner.New(runner.Config{
		AppName:           Name,
		Agent:             a,
		SessionService:    svc,
		AutoCreateSession: true,
	})
}

// Result summarizes one run of the agent.
type Result struct {
	// Text is the model's last text reply in the run.
	Text string
	// ToolCalls counts the tool calls the model requested in the run.
	ToolCalls int
	// PendingConfirmations are the toolconfirmation.FunctionCallName calls the
	// run paused on. Answer them with Confirm or ConfirmAll.
	PendingConfirmations []*genai.FunctionCall
	// Usage sums token usage across model responses; nil if none reported it.
	Usage *genai.GenerateContentResponseUsageMetadata
}

// Decision is the user's answer to one pending confirmation.
type Decision struct {
	Call     *genai.FunctionCall
	Approved bool
}

// Ask runs one user turn. onEvent, if non-nil, sees every event as it arrives.
func Ask(ctx context.Context, r *runner.Runner, userID, sessionID, prompt string, onEvent func(*session.Event)) (Result, error) {
	return run(ctx, r, userID, sessionID, genai.NewContentFromText(prompt, genai.RoleUser), onEvent)
}

// Confirm resumes a run paused on call, approving or rejecting the tool call
// it guards. When a run paused on several confirmations, use ConfirmAll:
// ADK resumes the model on the first answer it receives.
func Confirm(ctx context.Context, r *runner.Runner, userID, sessionID string, call *genai.FunctionCall, approved bool, onEvent func(*session.Event)) (Result, error) {
	return ConfirmAll(ctx, r, userID, sessionID, []Decision{{Call: call, Approved: approved}}, onEvent)
}

// ConfirmAll answers several pending confirmations in one message and resumes
// the run.
func ConfirmAll(ctx context.Context, r *runner.Runner, userID, sessionID string, decisions []Decision, onEvent func(*session.Event)) (Result, error) {
	if len(decisions) == 0 {
		return Result{}, errors.New("agent: no confirmation decisions")
	}
	msg := &genai.Content{Role: genai.RoleUser}
	for _, d := range decisions {
		if d.Call == nil || d.Call.Name != toolconfirmation.FunctionCallName {
			return Result{}, fmt.Errorf("agent: confirmation needs a %s call", toolconfirmation.FunctionCallName)
		}
		msg.Parts = append(msg.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID:       d.Call.ID,
			Name:     toolconfirmation.FunctionCallName,
			Response: map[string]any{"confirmed": d.Approved},
		}})
	}
	return run(ctx, r, userID, sessionID, msg, onEvent)
}

func run(ctx context.Context, r *runner.Runner, userID, sessionID string, msg *genai.Content, onEvent func(*session.Event)) (Result, error) {
	var res Result
	if r == nil {
		return res, errors.New("agent: runner is nil")
	}
	if userID == "" || sessionID == "" {
		return res, errors.New("agent: user and session IDs are required")
	}
	seen := make(map[string]bool) // function call IDs already counted
	for ev, err := range r.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			return res, err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		res.add(ev, seen)
	}
	return res, nil
}

// add folds a settled model event into res. Partial (streamed) events are
// skipped because the runner also emits their aggregate.
func (res *Result) add(ev *session.Event, seen map[string]bool) {
	if ev == nil || ev.Partial || ev.Content == nil || ev.Content.Role != genai.RoleModel {
		return
	}
	if u := ev.UsageMetadata; u != nil {
		if res.Usage == nil {
			res.Usage = &genai.GenerateContentResponseUsageMetadata{}
		}
		res.Usage.PromptTokenCount += u.PromptTokenCount
		res.Usage.CachedContentTokenCount += u.CachedContentTokenCount
		res.Usage.CandidatesTokenCount += u.CandidatesTokenCount
		res.Usage.ThoughtsTokenCount += u.ThoughtsTokenCount
		res.Usage.ToolUsePromptTokenCount += u.ToolUsePromptTokenCount
		res.Usage.TotalTokenCount += u.TotalTokenCount
	}
	var text strings.Builder
	for _, p := range ev.Content.Parts {
		if fc := p.FunctionCall; fc != nil {
			if fc.ID != "" {
				if seen[fc.ID] {
					continue
				}
				seen[fc.ID] = true
			}
			if fc.Name == toolconfirmation.FunctionCallName {
				res.PendingConfirmations = append(res.PendingConfirmations, fc)
			} else {
				res.ToolCalls++
			}
			continue
		}
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	if text.Len() > 0 {
		res.Text = text.String()
	}
}
