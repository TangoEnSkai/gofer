# ADR-0001: Value-first roadmap with scenario demos

- Status: Accepted
- Date: 2026-09-26

## Context

The first plan ordered milestones by technical layer: agent core → safety →
parallel & routines → hybrid → TUI. Under that order gofer is not useful for
real chores until M3, which conflicts with the top tie-breaker (time to daily
value). It also defers validation of the riskiest dependency, ADK for Go v2,
until a lot of code already depends on it.

## Decision

1. Add a time-boxed **M0 Spike** that validates ADK v2 before any feature work.
2. Re-cut milestones as **vertical slices**, each ending in a runnable scenario
   (see [toil-tasks.md](../toil-tasks.md)):
   - M1 First Errand — an interactive agent that can answer "summarize my open PRs"
   - M2 Daily Driver (v0.1.0) — S1 runs unattended every weekday
   - M3 Safe Writes (v0.2.0) — S2
   - M4 Hybrid (v0.3.0) — S3
   - M5 Polish (v1.0.0)
3. Plan **rolling-wave**: only the current and next milestone get task issues;
   later milestones stay described in their epics until they come up. This avoids
   writing detailed tickets that the spike or earlier milestones will invalidate.

## Consequences

- Daily use starts at v0.1.0 instead of after the safety layer.
- Scenario demos give an objective "done" for every milestone.
- Epics (#1–#8) are unchanged; they are feature areas that span milestones.
