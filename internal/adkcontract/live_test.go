package adkcontract

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Q5: real Gemini tool-calling reliability. Opt-in because it needs a key and
// spends free-tier quota:
//
//	GOFER_LIVE=1 GEMINI_API_KEY=... go test -run Live -v ./internal/adkcontract
//
// GOFER_LIVE_MODEL overrides the model (default gemini-flash-latest).
func TestLiveGeminiToolCalling(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if os.Getenv("GOFER_LIVE") != "1" || key == "" {
		t.Skip("set GOFER_LIVE=1 and GEMINI_API_KEY to run")
	}
	modelName := os.Getenv("GOFER_LIVE_MODEL")
	if modelName == "" {
		modelName = "gemini-flash-latest"
	}

	ctx := context.Background()
	m, err := gemini.NewModel(ctx, modelName, &genai.ClientConfig{
		APIKey: key,
		// Retries are off by default in genai; enable them for 408/429/5xx.
		HTTPOptions: genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		prompt string
		a, b   int
	}{
		{"What is 17 + 25? Use the tool.", 17, 25},
		{"Add 1234 and 4321.", 1234, 4321},
		{"I have 3 apples and buy 9 more. How many now? Use your add tool.", 3, 9},
		{"sum of negative 5 and 12", -5, 12},
		{"Compute 0 + 0 with the tool.", 0, 0},
	}

	ok := 0
	for i, tc := range cases {
		var calls atomic.Int32
		svc := session.InMemoryService()
		r, err := runner.New(runner.Config{AppName: appName, Agent: newAddAgent(t, m, &calls, nil), SessionService: svc})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		events := run(t, r, newSession(t, svc), userText(tc.prompt))
		text := finalText(events)
		want := fmt.Sprint(tc.a + tc.b)
		pass := calls.Load() == 1 && containsWord(text, want)
		if pass {
			ok++
		}
		t.Logf("case %d: tool_calls=%d pass=%v latency=%s answer=%q", i, calls.Load(), pass, time.Since(start).Round(time.Millisecond), text)
	}
	t.Logf("model=%s tool-calling success %d/%d", modelName, ok, len(cases))
	if ok < len(cases)-1 {
		t.Errorf("tool-calling success %d/%d is below the 80%% bar", ok, len(cases))
	}
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !isDigit(s[i-1])
		after := i+len(w) == len(s) || !isDigit(s[i+len(w)])
		if before && after {
			return true
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
