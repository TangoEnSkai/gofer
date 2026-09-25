# ADR-0003: Ship read-only routines before the safety layer

- Status: Accepted
- Date: 2026-09-26

## Context

Scheduled routines are an MVP requirement, but unattended runs that can write
files or run arbitrary shell commands need a permission engine and a sandbox,
which are M3 work. Waiting for them would push routines out of the MVP.

## Decision

Safety is enforced by **what is registered**, not only by policy:

- **Unattended (routine) runs** in M2 only get read-only tools: file reads,
  search, and the allowlisted read-only `gh` tool. `bash`, `write_file`,
  `edit_file`, and any write-capable tool are **not registered at all**, so the
  model cannot call them regardless of prompt content.
- **Interactive runs** in M1 may use `bash`, `write_file`, and `edit_file`, but
  every call requires explicit confirmation.
- Write-capable routines arrive in M3, together with the permission engine,
  `sandbox-exec`, checkpoints, and worktree isolation.

## Consequences

- Routines ship in v0.1.0 without a sandbox and without risk of local writes.
- Prompt injection via PR/issue content can at worst influence the text of a
  digest; it cannot trigger writes.
- Read-only `gh` must be strictly allowlisted (no `gh api` with non-GET methods).
