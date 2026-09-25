// Package adkcontract pins the ADK for Go behaviour that gofer relies on.
// If an ADK upgrade breaks one of these tests, gofer's assumptions need to be
// revisited before the upgrade lands. See docs/spikes/adk-v2.md.
package adkcontract

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	appName = "gofer-contract"
	userID  = "tester"
)

// run sends msg to the runner and returns every event, failing on errors.
func run(t *testing.T, r *runner.Runner, sessionID string, msg *genai.Content) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range r.Run(context.Background(), userID, sessionID, msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner error: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func userText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleUser)
}

// finalText returns the concatenated text of the last event that has text.
func finalText(events []*session.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if c := events[i].Content; c != nil {
			var s string
			for _, p := range c.Parts {
				s += p.Text
			}
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// findCall returns the first function call with the given name.
func findCall(events []*session.Event, name string) *genai.FunctionCall {
	for _, ev := range events {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == name {
				return p.FunctionCall
			}
		}
	}
	return nil
}

func newSession(t *testing.T, svc session.Service) string {
	t.Helper()
	resp, err := svc.Create(context.Background(), &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return resp.Session.ID()
}
