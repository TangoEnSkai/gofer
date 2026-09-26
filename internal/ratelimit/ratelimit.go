// Package ratelimit spaces model requests to a requests-per-minute budget, so
// gofer stays under the Gemini quota instead of leaning on 429 retries.
//
// Gemini quotas are per API key, so create one limiter per process with
// NewLimiter and pass that same limiter to every Wrap call (interactive agent,
// routine judge, summarizer). Separate limiters would each spend the full
// budget and together exceed it.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/adk/v2/model"
)

// NewLimiter returns a limiter allowing rpm requests per minute, or nil (no
// limiting) when rpm <= 0.
//
// The burst is 1, so requests are spaced evenly, one every minute/rpm. A
// token bucket with burst b admits up to rpm+b-1 requests within some
// 60-second window, so any larger burst could exceed the per-minute quota the
// limiter exists to respect.
func NewLimiter(rpm int) *rate.Limiter {
	if rpm <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(float64(rpm)/60), 1)
}

// Stats reports how much a Model was throttled.
type Stats struct {
	Waits  int           // requests delayed by the limiter
	Waited time.Duration // total delay across those requests
}

// Model is a model.LLM that waits for a limiter token before each request.
// It is safe for concurrent use.
type Model struct {
	llm     model.LLM
	limiter *rate.Limiter

	mu    sync.Mutex
	stats Stats
}

var _ model.LLM = (*Model)(nil)

// Wrap returns llm gated by l, which should be the process-wide limiter.
// A nil l means no limiting.
func Wrap(llm model.LLM, l *rate.Limiter) *Model {
	return &Model{llm: llm, limiter: l}
}

// Name implements model.LLM by delegating to the wrapped model.
func (m *Model) Name() string { return m.llm.Name() }

// GenerateContent implements model.LLM. When iterated, it waits for a token
// and then yields the wrapped model's responses unchanged, streaming partials
// included. If ctx ends first, it yields the context error and the wrapped
// model is never called.
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if err := m.wait(ctx); err != nil {
			yield(nil, err)
			return
		}
		m.llm.GenerateContent(ctx, req, stream)(yield)
	}
}

// Stats returns the throttling observed so far.
func (m *Model) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// wait blocks until the limiter grants a token or ctx ends. It reserves
// instead of calling rate.Limiter.Wait so that it knows the exact delay.
func (m *Model) wait(ctx context.Context) error {
	if m.limiter == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	r := m.limiter.ReserveN(now, 1)
	if !r.OK() {
		return errors.New("ratelimit: limiter never grants a token (zero burst)")
	}
	d := r.DelayFrom(now)
	if d == 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && dl.Sub(now) < d {
		r.CancelAt(now)
		return fmt.Errorf("ratelimit: next request slot in %s is past the deadline: %w", d, context.DeadlineExceeded)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		r.Cancel()
		return ctx.Err()
	}
	m.mu.Lock()
	m.stats.Waits++
	m.stats.Waited += d
	m.mu.Unlock()
	return nil
}
