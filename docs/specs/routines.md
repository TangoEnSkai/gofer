# Spec: Routines (M2, v0.1.0)

- Issues: #20 runner · #21 fan-out · #22 limiter · #23 launchd · #24 history & notifications · #25 S1
- Decisions: [ADR-0002](../decisions/0002-gather-then-judge.md),
  [ADR-0003](../decisions/0003-read-only-routines-first.md),
  [ADR-0005](../decisions/0005-adopt-adk-workflow.md),
  [ADR-0007](../decisions/0007-gatherers-are-go-code.md)

## 1. Shape of a routine run

```
launchd ──> gofer routine run pr-digest
              │
              ▼
   ┌───────────── ADK workflow ─────────────────────────────────┐
   │ Start → list (Go) → ParallelWorker(detail (Go)) → signals   │
   │        (Go) → render prompt (Go) → judge (LLM, JSON schema) │
   └──────────────────────────────┬──────────────────────────────┘
                                  ▼
              deliver (Go): render Markdown → runs/<name>/<ts>.md,
              latest.md → macOS notification → run record
```

- Everything except `judge` is deterministic Go and testable without a model.
- **One model call per run.** The judge has **no tools** in v0.1.0.
- Per-item failures flow as data (ADR-0005); the digest lists them.

## 2. Routine spec (YAML)

Location: `$XDG_CONFIG_HOME/gofer/routines/<name>.yaml` (default
`~/.config/gofer/routines/`). Bundled routines are embedded in the binary and
installed with `gofer routine add <name>`.

```yaml
name: pr-digest                 # [a-z0-9-]+, unique
description: Morning digest of my open PRs
gatherer: github.my_open_prs    # registered Go gatherer (ADR-0007)
with:                           # gatherer parameters, validated by the gatherer
  limit: 50
  stale_after_days: 14
schedule: "0 9 * * 1-5"         # 5-field cron subset, local time
model: gemini-flash-latest      # optional override
prompt: |                       # optional extra guidance appended to the judge instruction
  Prioritise PRs in databricks/* repos.
notify: always                  # always | on-failure | on-action | never
```

Validation is strict (unknown keys are errors). The spec never contains
secrets, commands, or templates.

## 3. Gatherers (Go)

```go
type Gatherer interface {
	Name() string                                        // "github.my_open_prs"
	Validate(with map[string]any) error
	// Build returns the deterministic part of the workflow: from Start to a node
	// whose output is the judge input (string) and the judge's output schema.
	Build(with map[string]any, deps Deps) (Pipeline, error)
}

type Pipeline struct {
	Edges        []workflow.Edge   // Start → ... → last
	Last         workflow.Node     // output: string (judge prompt)
	Instruction  string            // judge system instruction
	OutputSchema *genai.Schema     // judge must answer in this shape
	Render       func(judged json.RawMessage, gathered any) (Digest, error)
}
```

The runner appends `AgentNode(judge)` after `Last` and runs the workflow via
`workflowagent` + an in-memory session.

### `github.my_open_prs` (S1)

1. **list** — `gh.Client.SearchMyOpenPRs(limit)` (#19). Hard error if `gh` is
   missing or unauthenticated (pointless to continue).
2. **detail** — `ParallelWorker`, `maxConcurrency` 4: per PR,
   `PRView` + `PRChecks`. Transient failures retried inside the function
   (3 attempts, backoff); final failure → `{ok:false, error}`.
3. **signals** — deterministic facts per PR, so the model does not have to
   derive them:

   | Signal | Source |
   |---|---|
   | `ci` = failing / pending / passing / none | check buckets |
   | `review` = changes_requested / approved / review_required / none | `reviewDecision` |
   | `mergeable` | `mergeable` |
   | `days_since_activity` | `updatedAt` |
   | `last_comment_by_me` | latest comment/review author == viewer |
   | `stale_label` | labels matching `stale`, `lifecycle/stale`, `no-activity` |
   | `draft` | `isDraft` |

   Plus the latest 3 comments/reviews, each trimmed to 500 chars.
4. **render prompt** — compact JSON of signals + comments; PR text is quoted
   as data inside a delimited block with an instruction that it is untrusted
   content (prompt-injection hygiene).
5. **judge** — output schema:

   ```json
   {"headline": "string (<= 80 chars)",
    "items": [{"url": "string", "category": "needs_action|waiting_on_maintainer|stale_risk|ready_to_merge|fyi",
               "reason": "string", "next_action": "string"}]}
   ```

   `Render` validates that every `url` exists in the gathered set (drops and
   logs hallucinated ones) and that every gathered PR appears once (missing →
   `fyi` with "not classified").

## 4. Rate limiting (#22)

- Gemini model built with `genai.HTTPOptions.RetryOptions` (spike Q5 finding).
- `ratelimit.Wrap(llm, rpm)` — token bucket (`golang.org/x/time/rate`),
  `Limits.RequestsPerMinute` from config (default 10; revisit after #28).
- The limiter is shared process-wide, so parallel judges (future) and
  interactive use cannot exceed it together.

## 5. launchd (#23)

- `gofer routine add <name> [--from-bundled]` → writes the YAML (if bundled),
  then `~/Library/LaunchAgents/dev.gofer.<name>.plist` and runs
  `launchctl bootstrap gui/$UID <plist>`. `remove` → `bootout` + delete.
  `list` → specs + loaded state + last run.
- Plist:
  - `ProgramArguments`: absolute, symlink-resolved path of the running gofer
    binary + `routine run <name>`. Refuse when the binary lives in a temp/build
    cache dir (`go run`), with a hint to `go install`.
  - `StartCalendarInterval`: cron subset → array of dicts. Supported: numbers,
    `*`, lists `a,b`, ranges `a-b`; `*/n` steps expanded. Other syntax is a
    validation error. If the Mac is asleep at the scheduled time, launchd runs
    the job once on wake (coalesced) — acceptable for digests.
  - `EnvironmentVariables.PATH`: `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`.
  - `StandardOutPath` / `StandardErrorPath`: `~/.local/state/gofer/logs/<name>.log`.
  - No secrets: the key comes from Keychain (ADR-0004).
- `gofer doctor` adds: binary path stable, each plist loaded, Keychain key
  readable, `gh auth status` OK.

## 6. Run history & notifications (#24)

- Directory: `~/.local/state/gofer/runs/<name>/`
  - `<RFC3339-ts>.md` — rendered digest
  - `<RFC3339-ts>.json` — run record: status, duration, model, token usage,
    item counts per category, per-item errors, gofer version
  - `latest.md` — copy of the newest digest
- Keep the newest 60 runs per routine.
- `gofer routine logs <name> [-n 5]`, `gofer routine show <name>` (prints `latest.md`).
- Notification via `osascript -e 'display notification ...'` (no extra
  dependency). Title `gofer · <name>`, subtitle = headline, body = counts
  (`3 need action · 1 stale risk`). `notify` policy: `on-action` fires only
  when any item is `needs_action` or `stale_risk`.

## 7. S1 acceptance (#25)

- `gofer routine add pr-digest --from-bundled` on the owner's Mac, and it runs
  at 09:00 on weekdays for three consecutive working days without manual help.
- Digest correctly flags at least: a PR with failing CI, one with changes
  requested, one untouched for ≥ `stale_after_days`.
- Measured: model calls per run = 1, wall time, tokens; recorded in the run
  records and summarised in the v0.1.0 release notes.
- The existing paid-agent scheduled PR-watch tasks can be switched off.

## 8. Test plan

- Cron → `StartCalendarInterval` table tests; plist golden file.
- Gatherer: fake `gh.Runner` fixtures → signals golden output; partial failure
  → item with error; unauthenticated gh → hard error.
- Judge: `llmtest.Scripted` returns JSON; `Render` drops hallucinated URLs and
  fills unclassified PRs.
- Runner end-to-end with fakes: history files written, notification command
  invoked with the right text (injectable notifier), exactly one model call.
- Limiter: N concurrent calls with rpm=60 observe ≥ 1s spacing (fake clock).
