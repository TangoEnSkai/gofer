# ADR-0004: cobra CLI and Keychain-backed credentials

- Status: Accepted
- Date: 2026-09-26

## Context

gofer will grow several subcommands (`routine`, `mcp serve`, `doctor`, `run`)
with shared flags. Routines run under launchd, which does not load the user's
shell profile, so environment variables such as `GEMINI_API_KEY` are absent.

## Decision

- Use **cobra** for the CLI. It is the de facto standard for multi-command Go
  CLIs and generates shell completions.
- Resolve the API key in order: `GEMINI_API_KEY` → `GOOGLE_API_KEY` → macOS
  Keychain generic password with service `gofer`
  (`security add-generic-password -s gofer -a gemini -w`).
- Never write the key to config files, plists, logs, or session storage.

## Consequences

- launchd jobs work without secrets in plist files.
- `gofer doctor` must report which source the key came from (never the value).
