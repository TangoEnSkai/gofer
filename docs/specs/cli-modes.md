# Spec: CLI run modes, tool profiles, and sessions

- Issues: #12 (headless + REPL), #13 (sessions)
- Depends on: #18 (CLI/config/credentials), #9 (agent loop), #10/#11/#19 (tools)
- Decision: [ADR-0006](../decisions/0006-tool-profiles-by-run-mode.md)

## 1. Run modes

gofer runs in exactly one of three modes. The mode decides which tools are
registered and how confirmations are handled.

| Mode | Entry | Human present | Confirmation handling |
|---|---|---|---|
| **interactive** | `gofer` (REPL) on a TTY | yes | prompt `[y]es / [n]o / [a]lways this session` |
| **headless** | `gofer -p "..."`, or stdin is not a TTY | maybe not | never prompt; decided by flags up front |
| **routine** | `gofer routine run <name>` (launchd) | no | impossible by construction (see §2) |

Mode detection: `-p` or piped stdin ⇒ headless; otherwise interactive when
stdin and stdout are TTYs; otherwise headless.

## 2. Tool profiles

| Tool | interactive | headless (default) | headless `--allow-writes` | routine |
|---|---|---|---|---|
| `read_file`, `glob`, `grep` | ✅ | ✅ | ✅ | ✅ |
| `github` (read-only) | ✅ | ✅ | ✅ | ✅ |
| `write_file`, `edit_file` | ✅ confirm | — | ✅ no confirm | — |
| `bash` | ✅ confirm | — | ✅ no confirm | — |

- Tools that are not in a profile are **not registered**, so the model cannot
  call them (ADR-0003 generalised).
- `--allow-writes` is an explicit opt-in for scripting (`gofer -p ... --allow-writes`)
  and prints a one-line warning to stderr. It is not available to routines.
- If a confirmation request ever reaches a non-interactive mode (defence in
  depth), gofer auto-rejects it, records it, and exits with code 3.
- M3 replaces this table with the permission engine; the profile names stay.

## 3. Headless mode (`gofer -p`)

```
gofer -p "summarize my open PRs"
git diff | gofer -p "write a commit message for this diff"
gofer -p "..." --output json
```

- Prompt = `-p` value; if stdin is piped, its content is appended inside a
  fenced block labelled `stdin` (capped at 256 KiB, truncation noted).
- `--output text` (default): final answer only on stdout; progress (tool
  calls, one line each) on stderr when stderr is a TTY, silent otherwise.
- `--output json`: a single JSON object on stdout:
  `{"result": "...", "session_id": "...", "tool_calls": N, "usage": {...}, "denied_confirmations": N}`.
- `--output stream-json`: one JSON event per line (for other agents / MCP later).
- Exit codes: `0` ok · `1` runtime error · `2` usage/config error (no key,
  denied directory) · `3` a confirmation was required but not allowed.
- Always refuses to run in a denied directory (`config.IsDenied`) before any
  model call.

## 4. Interactive mode (REPL)

- Line-based; `bufio` input, no TUI dependency (TUI is M5).
- Streams model text as it arrives; shows tool calls as `▸ tool(args…)` lines
  (args truncated to one line).
- Confirmation prompt shows the tool name and full arguments (for `bash`, the
  exact command; for edits, a unified diff preview of `old_string → new_string`).
  `a` approves the same tool **with identical arguments** for the rest of the
  session — not the tool in general.
- Slash commands: `/help`, `/exit` (also Ctrl-D), `/clear` (new session),
  `/session` (print id), `/model <name>` (switch for next turn).
- Ctrl-C during a turn cancels the turn (context cancel), not the process.

## 5. Composition root

A new package `internal/app` owns wiring, so `cmd/gofer` stays thin and M2's
routine runner reuses it:

```go
type Mode int // Interactive, Headless, Routine

type App struct { Agent adkagent.Agent; Runner *runner.Runner; Sessions session.Service; ... }

func Build(ctx context.Context, cfg config.Config, mode Mode, opts BuildOptions) (*App, error)
// resolves credentials, builds the Gemini model (RetryOptions on, limiter from #22 when available),
// selects tools per §2, loads project instructions from the workdir, opens the session store.
```

## 6. Sessions (#13)

- Store: SQLite at `$XDG_STATE_HOME/gofer/sessions.db`, default
  `~/.local/state/gofer/sessions.db` (`database.NewSessionService` +
  `AutoMigrate`, verified in the spike).
- Session state keys set at creation: `gofer:workdir` (absolute path),
  `gofer:mode`, `gofer:created_by_version`.
- `--continue` / `-c`: most recently updated session whose `gofer:workdir`
  equals the current directory. None found ⇒ start a new one (stderr note).
- `--resume <id>`: that session; error if its workdir differs, unless
  `--force-workdir`.
- `gofer sessions list [--all]`: id, updated, workdir, first user message
  (truncated). Default filters to the current directory.
- Retention: sessions older than 30 days are pruned on startup (configurable;
  `0` disables).
- Compaction: `CompactionInterval: 10` for interactive sessions (spike Q6); off
  for headless one-shots.
- Routine runs use an in-memory session service (runs are recorded in run
  history instead, see the routines spec).

## 7. Test plan

- Mode detection table-test (TTY / pipe / `-p` combinations).
- Profile table-test: registered tool names per mode match §2 exactly.
- Headless end-to-end with `llmtest.Scripted`: text and JSON output, stdin
  appending, exit code 3 on auto-rejected confirmation, denied-dir exit 2.
- REPL driven through an `io.Reader`/`io.Writer` pair: confirmation y/n/a,
  `/clear`, `/exit`.
- Sessions: `--continue` picks the right session per workdir; `--resume`
  workdir mismatch; prune.
