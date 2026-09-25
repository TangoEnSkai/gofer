# ADR-0006: Tool profiles by run mode

- Status: Accepted
- Date: 2026-09-26
- Spec: [cli-modes.md](../specs/cli-modes.md)

## Context

M1 adds write and shell tools that require confirmation (ADR-0003). gofer runs
in three contexts — an interactive REPL, headless one-shots (`gofer -p`, pipes,
other agents), and unattended routines — and only the first has a human who can
answer a confirmation prompt. The permission engine that would decide this
per call arrives in M3.

## Decision

Tie the registered tool set to the run mode:

- **interactive**: all tools; writes and shell require confirmation.
- **headless**: read-only tools and the read-only `github` tool by default;
  `--allow-writes` registers write and shell tools without confirmation, as an
  explicit opt-in for scripting.
- **routine**: read-only tools only; not overridable.

A confirmation request that reaches a non-interactive mode is auto-rejected,
recorded, and makes the process exit with code 3.

## Consequences

- Headless use is safe by default, including when another agent drives gofer.
- The profile table is the single place to audit what each mode can do; M3's
  permission engine refines it instead of replacing the mode concept.
- Scripts that need writes must say so (`--allow-writes`), which is visible in
  shell history and CI configs.
