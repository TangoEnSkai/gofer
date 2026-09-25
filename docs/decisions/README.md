# Architecture Decision Records

Each significant decision is recorded as a short ADR: context, decision,
consequences. ADRs are immutable once accepted; a later ADR supersedes or narrows an
earlier one instead of editing it (only the status line gains a pointer).

## How decisions are made

**Hard constraints** (set by the project owner):
daily-use tool *and* portfolio piece · Go · Gemini (free tier) · macOS only ·
headless first · routines in the MVP · parallel tasks · hybrid with paid agents ·
focus on developer toil · epic/milestone workflow · public, Apache-2.0.

**Tie-breakers**, in priority order:

1. **Time to daily value** — prefer the path that makes gofer useful sooner.
2. **Unattended safety** — anything that runs without a human must not be able to do harm.
3. **Cost** — stay within the Gemini free tier; minimise model calls.
4. **Kill risk early** — validate the assumptions that would be most expensive to get wrong first.
5. **Legibility** — decisions and their reasons are written down (this directory).

**Decide vs. ask**: reversible decisions inside the constraints are made and
recorded here. Decisions that cost money, act outside this repository, are
irreversible, or conflict with a constraint go to the owner first.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-value-first-roadmap.md) | Value-first roadmap with scenario demos | Accepted |
| [0002](0002-gather-then-judge.md) | Routines gather in code, judge with the model | Accepted |
| [0003](0003-read-only-routines-first.md) | Ship read-only routines before the safety layer | Accepted |
| [0004](0004-cli-and-credentials.md) | cobra CLI and Keychain-backed credentials | Accepted |
| [0005](0005-adopt-adk-workflow.md) | Adopt ADK `workflow` for routines; gather errors are data | Accepted |
| [0006](0006-tool-profiles-by-run-mode.md) | Tool profiles by run mode | Accepted |
| [0007](0007-gatherers-are-go-code.md) | Gatherers are Go code; the judge has no tools in v0.1.0 | Accepted |
