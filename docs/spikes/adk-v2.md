# Spike: ADK for Go v2 building blocks

- Issue: #17 · Date: 2026-09-26
- Version under test: `google.golang.org/adk/v2` **v2.4.0**, `google.golang.org/genai` v1.70.0
- Evidence: [`internal/adkcontract`](../../internal/adkcontract) (runs in CI with `-race`; stable over 20 repeated runs)

## Verdict

**Go.** Every capability gofer's roadmap depends on works from plain Go code,
without the ADK web UI or launcher. ADK `workflow` is adopted for routines and
parallel gather ([ADR-0005](../decisions/0005-adopt-adk-workflow.md)), with one
design rule forced by its fail-fast semantics (Q4c).

| # | Question | Result | Test |
|---|---|---|---|
| Q1 | Agent loop against a scripted fake model | ✅ | `TestAgentLoopWithScriptedModel` |
| Q2 | Tool confirmation pause → resume from Go | ✅ approve and reject | `TestToolConfirmationPauseAndResume` |
| Q3 | SQLite sessions (pure Go) survive restart | ✅ | `TestSQLiteSessionResume` |
| Q4 | `workflow`: gather → one judge call, bounded parallelism, retry | ✅ with caveat | `TestWorkflow*` |
| Q5 | Real Gemini tool calling, free-tier 429 behaviour | ⏳ needs `GEMINI_API_KEY` (#28) | `TestLiveGeminiToolCalling` (opt-in) |
| Q6 | Context compaction | ✅ | `TestSlidingWindowCompaction` |

## Findings

### Q1 — Scripted model
`model.LLM` is a two-method interface (`Name`, `GenerateContent` returning an
`iter.Seq2`). [`internal/llmtest`](../../internal/llmtest) implements a scripted,
concurrency-safe fake that records requests. It is reusable by all M1+ tests,
so **CI never needs an API key**. Unexpected extra model calls fail the test.

### Q2 — Tool confirmation
`functiontool.Config.RequireConfirmationProvider func(Args) bool` decides per
call. When it returns true the tool does **not** run; the runner emits a
function call named `adk_request_confirmation` (`toolconfirmation.FunctionCallName`)
and the invocation ends. Resuming = sending a `FunctionResponse` with that
call's ID and `{"confirmed": true|false}`. On rejection the model is still
called once more and sees the rejection.

Implications:
- The M2/M3 permission engine plugs in as the provider function — no fork of
  ADK needed.
- Unattended runs must never block on confirmation: if one is requested, the
  routine runner auto-rejects and records it (defence in depth on top of
  ADR-0003).

### Q3 — SQLite sessions
`session/database.NewSessionService(sqlite.Open(path))` with
`github.com/glebarez/sqlite` (pure Go, no cgo) + `database.AutoMigrate`. A new
service instance on the same file sees all prior events and the next model
request contains the earlier turn. `--resume` in #13 is straightforward.

### Q4 — Workflow
- `workflow.Chain(Start, split, ParallelWorker(fetch), format, AgentNode(judge))`
  produces exactly **one** model call over the gathered data — the
  gather-then-judge shape of ADR-0002 maps directly.
- `NewParallelWorker(name, node, maxConcurrency, cfg)` bounds concurrency
  (observed max in-flight = limit).
- Per-item retry via `NodeConfig.RetryConfig` on the worker retries only the
  failing item.
- **Caveat (Q4c): fail-fast.** A non-retryable error from any item — or a
  retryable one that exhausts its attempts — aborts the whole run and the judge
  is never called. For S1, one inaccessible repo would sink the whole digest.

  **Rule:** gather functions never return Go errors for per-item failures. They
  retry transient errors internally and return a result value that carries the
  error (`{target, ok, data, error}`); the judge sees failures as data and
  reports them. Worker-level `RetryConfig` is not used for gather steps.

### Q5 — Real Gemini (pending)
Blocked on an API key; tracked in #28. Found while preparing it:
`genai` **disables HTTP retries by default** (`HTTPOptions.RetryOptions == nil`
→ single attempt) and ADK's `model/gemini` adds none. Setting
`RetryOptions: &genai.HTTPRetryOptions{}` enables exponential backoff with
jitter on 408/429/5xx. #22 therefore shrinks to: enable `RetryOptions` + a
proactive requests-per-minute limiter so parallel runs do not create 429 storms.

### Q6 — Compaction
`runner.Config.Compaction = &compaction.Config{CompactionInterval: N, Summarizer: s}`
replaces older turns with a summary in subsequent prompts. The summarizer is
pluggable, so gofer can summarise with a cheaper model (e.g. Flash-Lite).

Caveat from the ADK docs: *tail-retention* compaction (`TokenThreshold`)
appends summaries without passing through plugins. **Secret redaction in M3
must happen at tool-output time (after-tool callback), not in a plugin**, or
compacted summaries could carry unredacted secrets.

## Other observations

- The ADK console launcher (`cmd/launcher/console`) exists, but gofer keeps its
  own thin CLI/REPL (headless JSON output and the M5 TUI need control over
  rendering). Its `hitl.go` is a useful reference for confirmation prompts.
- v2 is actively changing (e.g. `session.NewEvent` now takes a `context.Context`;
  tool and callback contexts were merged). The contract tests are the upgrade
  gate: bump the pinned version in a dedicated PR and fix what they flag.

## Follow-ups

- #28 — run Q5 once `GEMINI_API_KEY` is available.
- #20 — build routines on `workflow` with the Q4c rule.
- #22 — rescoped per Q5.
