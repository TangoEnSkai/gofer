# Representative toil tasks

These scenarios define gofer's scope. Every milestone ends with one of them
running end to end, and they are the basis of the eval suite. A feature that
does not serve a scenario goes to the backlog.

## Scenarios

### S1 — Morning PR digest (M2, v0.1.0)

| | |
|---|---|
| Trigger | Scheduled, weekdays 09:00 (launchd) |
| Input | All open PRs authored by me across GitHub |
| Gather | `gh search prs --author @me --state open`, then per PR: checks, review decision, latest comments, last activity |
| Judge | Classify each PR: needs my action (failing CI, changes requested, question asked) · waiting on maintainer · at risk of stale-bot closure · ready to merge. Write a short digest, most urgent first. |
| Output | Digest in run history + macOS notification |
| Writes | None (read-only) |
| Replaces | The per-PR scheduled "PR watch" tasks currently run by a paid agent |

### S2 — Multi-repo chores (M3, v0.2.0)

| | |
|---|---|
| Trigger | Manual (`gofer run ...`) or scheduled |
| Examples | Sync every fork under `oss/` with upstream and report branches that need a rebase · apply the same small change (Go version bump, CI action update, typo) to N repos and open PRs |
| Gather | `git`/`gh` state per repo, in parallel |
| Act | Per repo, in an isolated git worktree: make the change, run checks, commit, open PR |
| Writes | Local worktrees, remote branches and PRs — requires permission profile + sandbox |

### S3 — Hybrid hand-off (M4, v0.3.0)

| | |
|---|---|
| Inbound | A paid agent (Claude Code, Codex) calls `gofer mcp serve` tools to offload chores, e.g. "run S1 now", "check CI on these 5 PRs" |
| Outbound | gofer hands a task it judges too heavy (e.g. a real bug fix found during S2) to `claude -p` / `codex exec` in the task's worktree, with confirmation |

## Backlog candidates

- Draft English replies to PR review comments
- Write PR bodies / commit messages in the Context / What / Why / Completion Criteria format
- Issue hygiene: epics without children, issues missing milestone or labels, PRs not linked to an issue
- Dependency and Go toolchain bump checks

## Out of scope

- Work on employer code or data. The Gemini free tier may use prompts to
  improve Google products; such directories belong on the deny list.
