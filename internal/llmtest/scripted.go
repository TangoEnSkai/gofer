// Package llmtest provides a scripted model.LLM for tests that must not call a
// real model.
package llmtest

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Reply produces the model's response to one request.
type Reply func(req *model.LLMRequest) (*model.LLMResponse, error)

// Scripted is a model.LLM that answers requests with a fixed sequence of
// replies and records every request it receives. It is safe for concurrent use.
type Scripted struct {
	mu       sync.Mutex
	replies  []Reply
	requests []*model.LLMRequest
}

var _ model.LLM = (*Scripted)(nil)

// New returns a Scripted model that serves replies in order.
func New(replies ...Reply) *Scripted {
	return &Scripted{replies: replies}
}

// Name implements model.LLM.
func (s *Scripted) Name() string { return "scripted" }

// GenerateContent implements model.LLM. It fails the call when the script is
// exhausted so that unexpected extra model calls surface as test failures.
func (s *Scripted) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.requests = append(s.requests, req)
		n := len(s.requests)
		if n > len(s.replies) {
			s.mu.Unlock()
			yield(nil, fmt.Errorf("llmtest: unexpected model call #%d (script has %d replies)", n, len(s.replies)))
			return
		}
		reply := s.replies[n-1]
		s.mu.Unlock()

		resp, err := reply(req)
		yield(resp, err)
	}
}

// Requests returns a copy of the requests received so far.
func (s *Scripted) Requests() []*model.LLMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*model.LLMRequest(nil), s.requests...)
}

// Text replies with a plain model text message.
func Text(text string) Reply {
	return func(*model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{
			Content:      genai.NewContentFromText(text, genai.RoleModel),
			TurnComplete: true,
		}, nil
	}
}

// Call replies with a single function call.
func Call(name string, args map[string]any) Reply {
	return func(*model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
			},
			TurnComplete: true,
		}, nil
	}
}

// Transcript flattens the text, function calls, and function responses of a
// request into one string, for substring assertions.
func Transcript(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			switch {
			case p.Text != "":
				fmt.Fprintf(&b, "[%s] %s\n", c.Role, p.Text)
			case p.FunctionCall != nil:
				fmt.Fprintf(&b, "[%s] call %s %v\n", c.Role, p.FunctionCall.Name, p.FunctionCall.Args)
			case p.FunctionResponse != nil:
				fmt.Fprintf(&b, "[%s] response %s %v\n", c.Role, p.FunctionResponse.Name, p.FunctionResponse.Response)
			}
		}
	}
	return b.String()
}
