# ADR-0007: Gatherers are Go code; the judge has no tools in v0.1.0

- Status: Accepted
- Date: 2026-09-26
- Spec: [routines.md](../specs/routines.md)

## Context

The first design sketched routine gather steps as YAML with templating
(`gh: pr checks {{each.url}}`). That is a small programming language: it needs
a parser, loops, error semantics, escaping, and it becomes an injection surface
for unattended runs. v0.1.0 has exactly one routine (S1).

ADR-0002 also allowed the judge to call read-only tools for follow-ups, which
makes the number of model calls per run unbounded.

## Decision

1. A routine spec references a **named, registered Go gatherer**
   (`gatherer: github.my_open_prs`) plus validated parameters (`with:`).
   There is no templating or command syntax in routine YAML.
2. Gatherers compute deterministic **signals** (CI state, review state, age,
   stale labels, …) so the model judges instead of deriving facts.
3. In v0.1.0 the judge runs with **no tools** and a JSON **output schema**; its
   output is validated and rendered by Go code.
4. A generic, declarative gatherer format is reconsidered once three concrete
   gatherers exist and their common shape is known.

## Consequences

- Exactly one model call per routine run; predictable free-tier cost.
- Adding a new kind of routine requires Go code and a release. Acceptable while
  the set of routines is small and owned by the project.
- Routine YAML has no way to run commands, so a tampered spec cannot escalate.
- Hallucinated items are caught by validating judge output against the
  gathered set.
