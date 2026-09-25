# gofer

> *gofer* (n.) — an assistant who runs errands. Also: **Go**, and a nod to the gopher.

**gofer** is a lightweight, Gemini-powered agent CLI for macOS, built on
[Google ADK for Go](https://github.com/google/adk-go). It is designed to remove
developer *toil*: the many small, repetitive, annoying tasks that are too trivial
for a heavyweight coding agent but still eat your day.

It is **not** trying to replace full-featured coding agents. Instead, it works
alongside them:

- **Fast & cheap** — defaults to Gemini Flash-class models.
- **Parallel** — fan out many small tasks at once, each isolated in its own git worktree.
- **Routines** — schedule recurring chores with launchd.
- **Hybrid** — delegate heavy work to `claude -p` / `codex exec`, and expose itself
  as an MCP server so other agents can hand chores *to* gofer.

## Status

🚧 Early development. See [docs/design.md](docs/design.md) and the
[milestones](https://github.com/TangoEnSkai/gofer/milestones).

## Install (once released)

```sh
go install github.com/TangoEnSkai/gofer/cmd/gofer@latest
```

## Configuration

### API key

gofer uses the Gemini API via [Google AI Studio](https://aistudio.google.com/apikey).
The key is looked up in this order: `GEMINI_API_KEY`, `GOOGLE_API_KEY`, then the
macOS Keychain. The Keychain is recommended because scheduled routines run under
launchd, which does not load your shell profile:

```sh
security add-generic-password -s gofer -a gemini -w   # prompts for the key
gofer doctor                                          # shows where the key was found, never the key
```

### Config file

Optional, at `~/.config/gofer/config.toml` (or `$XDG_CONFIG_HOME/gofer/config.toml`).
Unknown keys are rejected so that a typo cannot silently disable a setting.

```toml
model = "gemini-flash-latest"

# Directories whose contents must never be sent to the model.
deny_dirs = ["~/work"]

[limits]
requests_per_minute = 10
```

> [!WARNING]
> The Gemini API free tier may use your prompts and responses to improve Google
> products. Do not send confidential code or data while on the free tier.

> [!CAUTION]
> gofer can run shell commands and edit files. Review its permission settings and
> keep the sandbox enabled.

## License

[Apache License 2.0](LICENSE)
