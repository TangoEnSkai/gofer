# Roadmap

Milestones are vertical slices: each one ends with a scenario from
[toil-tasks.md](toil-tasks.md) running end to end. Only the current and next
milestone have task issues ([ADR-0001](decisions/0001-value-first-roadmap.md)).

| Milestone | Goal | Done when | Release |
|---|---|---|---|
| [M0 Spike](https://github.com/TangoEnSkai/gofer/milestone/6) | Validate ADK for Go v2 | ✅ [Go](spikes/adk-v2.md): contract tests in CI; `workflow` adopted ([ADR-0005](decisions/0005-adopt-adk-workflow.md)); live check moved to #28 | — |
| [M1 First Errand](https://github.com/TangoEnSkai/gofer/milestone/1) | Interactive agent with core and `gh` read tools | `gofer -p "summarize my open PRs"` works against real Gemini | — |
| [M2 Daily Driver](https://github.com/TangoEnSkai/gofer/milestone/2) | Read-only routines, parallel gather, launchd | S1 runs unattended every weekday | v0.1.0 |
| [M3 Safe Writes](https://github.com/TangoEnSkai/gofer/milestone/3) | Permissions, sandbox, checkpoints, worktree isolation | S2 | v0.2.0 |
| [M4 Hybrid](https://github.com/TangoEnSkai/gofer/milestone/4) | MCP server mode, delegation | S3 | v0.3.0 |
| [M5 Polish](https://github.com/TangoEnSkai/gofer/milestone/5) | TUI, evals, distribution | Homebrew install + README demo | v1.0.0 |

## Dependency order

```
M0 #17 spike ─┬─> M1 #9 agent loop ─┬─> #10 file/search ─┐
              │   #18 CLI/config ───┤   #11 shell ────────┼─> #12 headless/REPL ─> #13 sessions
              │                     └─> #19 gh read ──────┘
              └─> M2 #22 rate limit ─> #20 routine runner ─> #21 fan-out ─> #23 launchd ─> #24 history ─> #25 S1
```

## Risks

| Risk | Mitigation |
|---|---|
| ADK v2 API churn | Pin the version; contract tests; upgrades in dedicated PRs |
| Free-tier rate limits | Gather-then-judge ([ADR-0002](decisions/0002-gather-then-judge.md)); limiter + backoff (#22); Flash-Lite for sub-tasks |
| Free-tier data use | Directory deny list; no employer code |
| launchd environment (no PATH, no env secrets) | Explicit PATH in plist; Keychain credentials ([ADR-0004](decisions/0004-cli-and-credentials.md)); `gofer doctor` |
| `sandbox-exec` is deprecated | Still used by major agent CLIs on macOS; fall back to permission-only mode if it breaks |
| Scope creep toward a full coding agent | Scenario gate: features that serve no scenario go to the backlog |
