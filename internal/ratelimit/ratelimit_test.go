package ratelimit

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// Tests run inside synctest bubbles, so time is a fake clock: elapsed times
// are exact and a 60 rpm limiter costs no wall-clock time.

func TestNewLimiter(t *testing.T) {
	l := NewLimiter(60)
	if l.Limit() != 1 || l.Burst() != 1 {
		t.Errorf("NewLimiter(60) = limit %v burst %d, want 1/s burst 1", l.Limit(), l.Burst())
	}
	for _, rpm := range []int{0, -1} {
		if l := NewLimiter(rpm); l != nil {
			t.Errorf("NewLimiter(%d) = %v, want nil", rpm, l)
		}
	}
}

func TestCallsAreSpaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Time
		stamp := func(req *model.LLMRequest) (*model.LLMResponse, error) {
			at = append(at, time.Now())
			return llmtest.Text("ok")(req)
		}
		m := Wrap(llmtest.New(stamp, stamp, stamp), NewLimiter(60))

		start := time.Now()
		for range 3 {
			if _, err := collect(m.GenerateContent(t.Context(), &model.LLMRequest{}, false)); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := time.Since(start), 2*time.Second; got != want {
			t.Errorf("3 calls at 60 rpm took %v, want %v", got, want)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap != time.Second {
				t.Errorf("gap between call %d and %d = %v, want 1s", i, i+1, gap)
			}
		}
		if got, want := m.Stats(), (Stats{Waits: 2, Waited: 2 * time.Second}); got != want {
			t.Errorf("Stats() = %+v, want %+v", got, want)
		}
	})
}

// Two wrapped models (say, the interactive agent and a routine judge) share
// one limiter, so their concurrent calls are serialized together.
func TestSharedLimiterSerializesConcurrentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const perModel = 3
		var (
			mu sync.Mutex
			at []time.Time
		)
		stamp := func(req *model.LLMRequest) (*model.LLMResponse, error) {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
			return llmtest.Text("ok")(req)
		}
		replies := slices.Repeat([]llmtest.Reply{stamp}, perModel)
		l := NewLimiter(60)
		models := []*Model{Wrap(llmtest.New(replies...), l), Wrap(llmtest.New(replies...), l)}

		start := time.Now()
		var wg sync.WaitGroup
		for _, m := range models {
			for range perModel {
				wg.Go(func() {
					if _, err := collect(m.GenerateContent(t.Context(), &model.LLMRequest{}, false)); err != nil {
						t.Error(err)
					}
				})
			}
		}
		wg.Wait()

		n := len(models) * perModel
		if got, want := time.Since(start), time.Duration(n-1)*time.Second; got != want {
			t.Errorf("%d concurrent calls at 60 rpm took %v, want %v", n, got, want)
		}
		slices.SortFunc(at, time.Time.Compare)
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < time.Second {
				t.Errorf("calls %d and %d only %v apart, want >= 1s", i, i+1, gap)
			}
		}
		var waits int
		for _, m := range models {
			waits += m.Stats().Waits
		}
		if waits != n-1 {
			t.Errorf("total waits = %d, want %d", waits, n-1)
		}
	})
}

func TestCancelWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewLimiter(60)
		l.Allow() // spend the only token so the next call must wait 1s
		inner := llmtest.New(llmtest.Text("never"))
		m := Wrap(inner, l)

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(300*time.Millisecond, cancel)
		start := time.Now()
		_, err := collect(m.GenerateContent(ctx, &model.LLMRequest{}, false))

		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if got := time.Since(start); got != 300*time.Millisecond {
			t.Errorf("returned after %v, want 300ms", got)
		}
		if n := len(inner.Requests()); n != 0 {
			t.Errorf("inner model called %d times, want 0", n)
		}
		if got := m.Stats(); got != (Stats{}) {
			t.Errorf("Stats() = %+v, want zero", got)
		}
	})
}

func TestDeadlineBeforeNextSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewLimiter(60)
		l.Allow()
		inner := llmtest.New(llmtest.Text("never"))
		m := Wrap(inner, l)

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := collect(m.GenerateContent(ctx, &model.LLMRequest{}, false))

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("returned after %v, want immediately", got)
		}
		if n := len(inner.Requests()); n != 0 {
			t.Errorf("inner model called %d times, want 0", n)
		}
	})
}

func TestAlreadyCanceled(t *testing.T) {
	inner := llmtest.New(llmtest.Text("never"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := collect(Wrap(inner, NewLimiter(60)).GenerateContent(ctx, &model.LLMRequest{}, false))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := len(inner.Requests()); n != 0 {
		t.Errorf("inner model called %d times, want 0", n)
	}
}

func TestNilLimiterPassesThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := llmtest.New(llmtest.Text("a"), llmtest.Text("b"), llmtest.Text("c"))
		m := Wrap(inner, nil)
		start := time.Now()
		for range 3 {
			if _, err := collect(m.GenerateContent(t.Context(), &model.LLMRequest{}, false)); err != nil {
				t.Fatal(err)
			}
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("3 unlimited calls took %v, want 0", got)
		}
		if got := m.Stats(); got != (Stats{}) {
			t.Errorf("Stats() = %+v, want zero", got)
		}
	})
}

func TestPassesResponsesThroughUnchanged(t *testing.T) {
	partial := &model.LLMResponse{Content: genai.NewContentFromText("Hel", genai.RoleModel), Partial: true}
	final := &model.LLMResponse{Content: genai.NewContentFromText("Hello", genai.RoleModel), TurnComplete: true}
	boom := errors.New("boom")
	inner := &streamLLM{resps: []*model.LLMResponse{partial, final}, errs: []error{nil, boom}}
	m := Wrap(inner, NewLimiter(6000))

	if got := m.Name(); got != "stream" {
		t.Errorf("Name() = %q, want %q", got, "stream")
	}

	req := &model.LLMRequest{Model: "m"}
	var resps []*model.LLMResponse
	var errs []error
	for resp, err := range m.GenerateContent(t.Context(), req, true) {
		resps = append(resps, resp)
		errs = append(errs, err)
	}
	if !slices.Equal(resps, inner.resps) || !slices.Equal(errs, inner.errs) {
		t.Errorf("got responses %v errors %v, want %v %v", resps, errs, inner.resps, inner.errs)
	}
	if inner.req != req || !inner.stream {
		t.Errorf("inner got req %p stream %v, want %p true", inner.req, inner.stream, req)
	}

	// Stopping early must reach the inner iterator.
	for range m.GenerateContent(t.Context(), req, true) {
		break
	}
	if inner.yielded != 1 {
		t.Errorf("inner yielded %d responses after early break, want 1", inner.yielded)
	}
}

// streamLLM yields a fixed sequence of responses, as a streaming model does.
type streamLLM struct {
	resps []*model.LLMResponse
	errs  []error

	req     *model.LLMRequest
	stream  bool
	yielded int
}

func (s *streamLLM) Name() string { return "stream" }

func (s *streamLLM) GenerateContent(_ context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	s.req, s.stream, s.yielded = req, stream, 0
	return func(yield func(*model.LLMResponse, error) bool) {
		for i, resp := range s.resps {
			s.yielded++
			if !yield(resp, s.errs[i]) {
				return
			}
		}
	}
}

// collect drains seq and returns its responses and the first error.
func collect(seq iter.Seq2[*model.LLMResponse, error]) ([]*model.LLMResponse, error) {
	var resps []*model.LLMResponse
	for resp, err := range seq {
		if err != nil {
			return resps, err
		}
		resps = append(resps, resp)
	}
	return resps, nil
}
