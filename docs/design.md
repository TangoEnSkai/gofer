# gofer — System Design

Status: **Draft** · Owner: @TangoEnSkai

Related: [roadmap](roadmap.md) · [toil tasks](toil-tasks.md) · [decisions](decisions/README.md) ·
specs: [CLI modes & sessions](specs/cli-modes.md), [routines](specs/routines.md)

## 1. Positioning

gofer is a **lightweight errand runner for developers**. Full-featured coding
agents (Claude Code, Codex) are excellent for deep, multi-hour engineering work,
but are heavy and expensive for the long tail of small chores: checking CI across
repos, triaging issues, drafting replies, bumping versions, writing summaries.

gofer targets that long tail. It is designed to be:

| Principle | Meaning |
|---|---|
| **Light** | Single Go binary, fast startup, Flash-class models by default. |
| **Parallel** | Fan out many small tasks at once; the unit of work is a *task*, not a chat. |
| **Routine-first** | Recurring chores are first-class and can run unattended on a schedule. |
| **Hybrid** | Delegates heavy work to paid agents, and accepts chores *from* them. |
| **Safe by default** | Unattended runs require strict permissions and a sandbox. |

Non-goals: replacing a full IDE-grade coding agent, multi-platform support
(macOS only), or large autonomous refactors.

## 2. Key decisions

| Topic | Decision | Rationale |
|---|---|---|
| Language | Go | Single binary, strong process control, author's primary language. |
| Agent framework | [ADK for Go](https://github.com/google/adk-go) v2 (`google.golang.org/adk/v2`), version-pinned | Provides agent loop, sub-agents, callbacks, tool confirmation, MCP, DB sessions, compaction. v2 still has breaking changes, so upgrades land in dedicated PRs. |
| Model | Gemini, default `gemini-flash-latest`, configurable | Cheap and fast for chores; free tier available. |
| Routine shape | Gather in code, judge with the model | Fewer model calls, testable without a model ([ADR-0002](decisions/0002-gather-then-judge.md)). |
| CLI | cobra; API key from env or macOS Keychain | Many subcommands; launchd has no shell env ([ADR-0004](decisions/0004-cli-and-credentials.md)). |
| Auth | Gemini API key from Google AI Studio (`GEMINI_API_KEY`); Vertex AI optional later | Free tier, no GCP billing setup. See §7 for the data-use caveat. |
| Platform | macOS only | Enables `sandbox-exec`, launchd, APFS clones, native notifications. |
| UI | Headless + line REPL first; Bubble Tea TUI in M5 | Ship value early; TUI is polish. |
| License | Apache-2.0 | Same as ADK. |

## 3. Architecture

```
            ┌─────────────── entry points ───────────────┐
            │ gofer -p "..."   gofer (REPL)   gofer run   │
            │ gofer routine …  (launchd)   gofer mcp serve│
            └──────────────────────┬──────────────────────┘
                                   ▼
┌──────────────────────── Orchestrator ─────────────────────────┐
│ task queue · worker pool · concurrency & rate limits          │
│ per task: workspace (in place / git worktree / APFS clone)    │
│ result aggregation · cost accounting · notifications          │
└──────────────────────────────┬─────────────────────────────────┘
                               ▼  one ADK runner per task
┌──────────────────────── ADK agent ────────────────────────────┐
│ root llmagent "gofer"  (Gemini)                               │
│  instruction: system prompt + AGENTS.md + routine prompt      │
│  tools: fs · edit · search · shell · git/gh · web · MCP       │
│         delegate (claude -p / codex exec)                     │
│  sub-agents: explore (read-only), planner                     │
│  callbacks:                                                   │
│    BeforeTool  → permission engine (allow/ask/deny)           │
│    AfterTool   → truncation, redaction, checkpoint log        │
│  session: SQLite (session/database) + compaction              │
└──────────────────────────────┬─────────────────────────────────┘
                               ▼
┌──────────────────────── Execution layer ──────────────────────┐
│ sandbox-exec profile (write = workspace only, net optional)   │
│ checkpoints (pre-edit snapshots) → undo                       │
└───────────────────────────────────────────────────────────────┘
```

### Package layout (planned)

```
cmd/gofer/            CLI entry point (cobra)
internal/agent/       root agent, prompts, model construction
internal/tools/       fs, edit, search, shell, git, gh, web, delegate
internal/policy/      permission rules, profiles, confirmation bridge
internal/sandbox/     sandbox-exec profile generation and exec wrapper
internal/workspace/   in-place / worktree / APFS clone isolation
internal/orchestrator/task queue, worker pool, aggregation
internal/routine/     routine specs, launchd plist generation, run history
internal/mcpserver/   gofer-as-MCP-server
internal/config/      config loading (~/.config/gofer/config.toml)
internal/ui/          line REPL now, Bubble Tea TUI later
internal/adkcontract/ tests pinning the ADK behaviour gofer relies on
```

## 4. Mapping to ADK

| Capability | ADK building block | Notes |
|---|---|---|
| Agent loop | `agent/llmagent`, `runner` | |
| Tools | `tool/functiontool` | Typed Go functions. |
| Sub-agents | `tool/agenttool`, `SubAgents` | Explore / planner. |
| Permission prompts | `BeforeToolCallbacks`, `tool/toolconfirmation` | Bridge to REPL/TUI; auto-deny in unattended mode. |
| MCP client | `tool/mcptoolset` | |
| Sessions | `session/database` | SQLite under `~/.local/state/gofer`. |
| Compaction | `session/compaction` | LLM summarizer. |
| Skills | `tool/skilltoolset` | Candidate for reusable routine recipes. |
| Model | `model/gemini` | `model.LLM` interface allows other providers later. genai HTTP retries are off by default; gofer enables `RetryOptions` and adds a requests-per-minute limiter. |
| Routines / parallel gather | `workflow` (function, agent, parallel, retry nodes) | Adopted ([ADR-0005](decisions/0005-adopt-adk-workflow.md)); per-item failures are returned as data because `ParallelWorker` is fail-fast. |

Built outside ADK: workspace isolation, sandbox, launchd integration, rate
limiting, delegation, MCP server mode, UI.

## 5. Parallel tasks and isolation

Each task runs its own runner and session. Isolation is chosen per task:

| Task kind | Strategy | Why |
|---|---|---|
| Read-only (summaries, triage, status checks) | Run in place | No conflicts; zero setup cost. |
| Writes inside a git repo | `git worktree add` on a new branch under `~/.local/state/gofer/worktrees/` | Cheap, shares object store, natural PR flow. |
| Writes outside git | APFS clone (`cp -c -R`) | Copy-on-write, near-instant on macOS. |

Worktrees are removed after the task unless it produced commits or failed
(kept for inspection). Concurrency is bounded by a configurable limit and by the
model's rate limit (important on the free tier).

Alternatives considered: containers (heavy on macOS, not needed for chores) and
`jj` workspaces (nice, but adds a dependency).

## 6. Routines

A routine is a **gather-then-judge** pipeline described by a YAML spec
([ADR-0002](decisions/0002-gather-then-judge.md)):

```yaml
name: morning-pr-digest
schedule: "0 9 * * 1-5"      # cron syntax, translated to launchd StartCalendarInterval
workdir: ~/ws/github
profile: readonly            # only read-only tools are registered
gatherer: github.my_open_prs # registered Go gatherer, no templating (ADR-0007)
with: { limit: 50 }
prompt: |                    # optional extra guidance for the single judge step
  Prioritise PRs in databricks/* repos.
notify: always               # always | on-failure | never
```

Full specification: [specs/routines.md](specs/routines.md).

In v0.1.0 routines are **read-only by construction**: write-capable tools are
not registered for unattended runs ([ADR-0003](decisions/0003-read-only-routines-first.md)).

`gofer routine add` writes `~/Library/LaunchAgents/dev.gofer.<name>.plist`
that invokes `gofer routine run <name>`. Runs are non-interactive: any tool call
that would require `ask` is denied and reported. Output and history are stored
per run and surfaced via macOS notifications.

## 7. Security model

- **Permission engine**: rules match tool name + arguments (e.g. `bash(git status*)`),
  resolving to `allow` / `ask` / `deny`. Profiles: `readonly`, `default`, `trusted`.
- **Sandbox**: shell commands run under `sandbox-exec` with writes restricted to
  the task workspace and temp dirs; network can be disabled per profile.
- **Checkpoints**: files are snapshotted before edits to support undo.
- **Secrets**: API keys only via environment or macOS Keychain; tool output is
  redacted for common token patterns before being sent to the model or logs.
- **Free tier data use**: the Gemini API free tier may use prompts to improve
  Google products. gofer shows a one-time warning and supports a per-directory
  `deny` list so confidential repos are never sent while on the free tier.

## 8. Hybrid mode

- **Outbound (`delegate` tool)**: runs `claude -p` or `codex exec` in the task
  workspace for work beyond gofer's scope. Opt-in, disabled in routines by
  default, and always requires confirmation.
- **Inbound (`gofer mcp serve`)**: exposes tools such as `run_task`,
  `run_parallel`, `list_routines` so a heavyweight agent can offload chores.

## 9. Milestones

See [roadmap.md](roadmap.md). Milestones are vertical slices that each end
in a scenario demo: M0 Spike → M1 First Errand → M2 Daily Driver (v0.1.0) →
M3 Safe Writes (v0.2.0) → M4 Hybrid (v0.3.0) → M5 Polish (v1.0.0).

## 10. Open questions

- Whether to use ADK `skilltoolset` as the format for reusable routine recipes.
