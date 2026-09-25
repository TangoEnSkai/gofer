# ADR-0002: Routines gather in code, judge with the model

- Status: Accepted · judge tools narrowed for v0.1.0 by [ADR-0007](0007-gatherers-are-go-code.md)
- Date: 2026-09-26

## Context

Most toil is mostly deterministic. A "summarize my open PRs" chore is dozens of
`gh` reads and one act of judgement. Running it as a free-form agent loop means
the model spends a request on every read: slower, less reliable, and it burns
through free-tier limits quickly — especially when fanned out in parallel.

ADK for Go v2 ships a `workflow` package (function nodes, agent nodes,
parallel workers, retry, persistence, human-in-the-loop) that matches this shape.

## Decision

Routines are **gather-then-judge** pipelines:

1. **Gather** — deterministic steps written in Go (e.g. `gh` queries), run in
   parallel where possible. No model calls.
2. **Judge** — one agent step that receives the gathered data and produces the
   result (digest, triage, draft). It may call read-only tools for follow-ups.

Implementation uses ADK `workflow` if the M0 spike confirms it works from a CLI;
otherwise a plain Go pipeline with the same shape.

Free-form agent loops remain the model for **interactive** use (`gofer -p`, REPL).

## Consequences

- One or a few model calls per routine run instead of one per read.
- Gather steps are unit-testable without a model; `--dry-run` can show exactly
  what the model will see.
- Adding a routine may require writing a gather step in Go, not just a prompt.
  Generic gather steps (`gh`, shell read commands) keep this cheap.
