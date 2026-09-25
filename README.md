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

gofer uses the Gemini API via [Google AI Studio](https://aistudio.google.com/apikey):

```sh
export GEMINI_API_KEY=...
```

> [!WARNING]
> The Gemini API free tier may use your prompts and responses to improve Google
> products. Do not send confidential code or data while on the free tier.

> [!CAUTION]
> gofer can run shell commands and edit files. Review its permission settings and
> keep the sandbox enabled.

## License

[Apache License 2.0](LICENSE)
