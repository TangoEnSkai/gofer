# ADR-0005: Adopt ADK `workflow` for routines; gather errors are data

- Status: Accepted
- Date: 2026-09-26
- Evidence: [spike](../spikes/adk-v2.md), `internal/adkcontract/workflow_test.go`

## Context

ADR-0002 left open whether gather-then-judge routines and parallel gather would
be built on ADK's `workflow` package or on a hand-written worker pool. The M0
spike showed that `workflow` supports the exact shape (function nodes →
`ParallelWorker` with bounded concurrency → agent node) with per-item retry.
It also showed that `ParallelWorker` is **fail-fast**: one item's final error
aborts the run before the judge step.

## Decision

1. Build routines and parallel gather on ADK `workflow` + `workflowagent`.
2. Gather functions **never return Go errors for per-item failures**. They
   handle transient retries themselves and return a result that carries the
   outcome (`target`, `ok`, `data`, `error`). The judge step receives failures
   as data and reports them.
3. Go errors from gather steps are reserved for conditions where continuing is
   pointless (e.g. `gh` not authenticated), which should fail the run loudly.

## Consequences

- No custom scheduler, retry, or fan-in code to maintain; HITL and persistence
  come with the engine if later needed.
- Partial failures degrade a digest instead of killing it.
- gofer's routine behaviour now depends on `workflow` semantics; the contract
  tests pin them (including fail-fast) so an upgrade that changes them is caught.
