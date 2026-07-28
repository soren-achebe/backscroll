---
description: >-
  Give your AI coding agent your real terminal history over MCP: setup for
  Claude Code, Claude Desktop, Cursor, VS Code, Windsurf, Zed, Codex CLI,
  and Gemini CLI — read-only, secrets redacted by default.
---

# Let your AI agent read your terminal history

backscroll ships a built-in [Model Context Protocol](https://modelcontextprotocol.io)
server: `backscroll mcp`. It's a zero-dependency stdio server compiled into
the same binary you already installed — no npm, no Python, no sidecar.

Why you'd want this: coding agents constantly need to know *what a command
actually printed*. Without it they guess, or worse, **re-run things** — and
"just re-run it" is exactly wrong for the interesting cases: the flaky test,
the 40-minute build, the migration you only get to run once, the deploy
that failed last night. With backscroll recording your shells, the agent
can query ground truth instead:

- *"Why did the deploy fail last night?"* → `search_output "deploy"` with
  failures-only filter, then read the real error.
- *"Did the output of `make test` change since it last passed?"* →
  `diff_output` against the previous run of the same command.
- *"What warnings did that 40-minute build print?"* → `get_output` on a
  command that finished hours ago, scrollback long gone.

## The tools

| Tool | What it does |
|---|---|
| `search_output` | Full-text search over commands, outputs, and notes; grep&nbsp;-C-style context via `context_lines` |
| `list_commands` | Browse recent history with the same filters as the CLI (`exit=fail`, `since`, `cwd`, …) |
| `get_output` | Full recorded output of any command (`-1` = your last one), with metadata |
| `diff_output` | Unified diff between two runs — by default vs. the previous run of the same command line |

All four are **read-only** — the server never executes anything, it only
reads the local SQLite database. Every tool declares MCP annotations
(`readOnlyHint`, `idempotentHint`) so clients can skip confirmation
prompts safely.

**Secrets are masked by default.** Everything handed to the client passes
through the same redaction patterns as `backscroll redact` (built-in
token-format patterns plus your `~/.config/backscroll/redact`), on top of
the ignore patterns that keep matching commands out of the database
entirely. `--no-redact` opts out. Nothing leaves the machine except what
your client asks for over stdio.

## Setup

First make sure backscroll is actually [recording your
shells](index.md#install) — the server is only as useful as the history
behind it. Then register it with your client. One gotcha up front:

!!! note "GUI apps may not see your `PATH`"
    Desktop clients (Claude Desktop, Cursor, Windsurf, Zed) are often
    launched without your shell's `PATH`. If the server fails to start,
    replace `"backscroll"` with the absolute path — `~/.local/bin/backscroll`
    for the script install, `/opt/homebrew/bin/backscroll` for Homebrew
    (`command -v backscroll` tells you).

### Claude Code

```sh
claude mcp add backscroll -- backscroll mcp
```

### Codex CLI

```sh
codex mcp add backscroll -- backscroll mcp
```

or in `~/.codex/config.toml`:

```toml
[mcp_servers.backscroll]
command = "backscroll"
args = ["mcp"]
```

### Claude Desktop

Grab the `backscroll-<version>.mcpb` bundle from the
[latest release](https://github.com/soren-achebe/backscroll/releases/latest)
and open it with Claude Desktop (Settings → Extensions) — it carries the
binaries for macOS and Linux. Or configure it manually in
`claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "backscroll": { "command": "backscroll", "args": ["mcp"] }
  }
}
```

### Cursor

`~/.cursor/mcp.json` (or `.cursor/mcp.json` in a project):

```json
{
  "mcpServers": {
    "backscroll": { "command": "backscroll", "args": ["mcp"] }
  }
}
```

### Windsurf

`~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "backscroll": { "command": "backscroll", "args": ["mcp"] }
  }
}
```

### VS Code (Copilot agent mode)

```sh
code --add-mcp '{"name":"backscroll","command":"backscroll","args":["mcp"]}'
```

or `.vscode/mcp.json`:

```json
{
  "servers": {
    "backscroll": { "type": "stdio", "command": "backscroll", "args": ["mcp"] }
  }
}
```

VS Code's own shell integration is a nice pairing here: backscroll
[records its OSC 633 marks](osc133.md) with no snippet at all, so terminals
inside VS Code are covered automatically.

### Zed

In `settings.json`:

```json
{
  "context_servers": {
    "backscroll": {
      "source": "custom",
      "command": "backscroll",
      "args": ["mcp"]
    }
  }
}
```

### Gemini CLI

`~/.gemini/settings.json`:

```json
{
  "mcpServers": {
    "backscroll": { "command": "backscroll", "args": ["mcp"] }
  }
}
```

### Anything else

Any MCP client that can spawn a stdio server just needs
`backscroll mcp` as the command. backscroll is listed in the
[official MCP Registry](https://registry.modelcontextprotocol.io/?search=backscroll)
as `io.github.soren-achebe/backscroll`, and the repo includes a
[Dockerfile](https://github.com/soren-achebe/backscroll/blob/main/Dockerfile)
for containerized setups (mount your database read-only; recording itself
still wants the native binary wrapped around your real shell).

## Try it without a client

The [MCP inspector](https://github.com/modelcontextprotocol/inspector)
exercises the server directly:

```console
$ npx @modelcontextprotocol/inspector --cli backscroll mcp \
    --method tools/call --tool-name list_commands \
    --tool-arg exit=fail --tool-arg since=1d
{
  "content": [
    {
      "type": "text",
      "text": "1 command(s), most recent first:\n\nid 312 · 2026-07-28 10:41:50 · cwd /tmp · exit 1 · took 5ms · 36B\n$ ./deploy.sh staging\n\nUse get_output with an id to read a full output."
    }
  ]
}
```

If it errors, run `backscroll doctor` — the usual cause is an empty
database because no shell is being recorded yet (`doctor` will also offer
to [import your existing shell history](https://github.com/soren-achebe/backscroll#bring-your-existing-history)
so the agent has something to search on day one).

## Design notes

- **Never re-executes.** The server has no tool that runs commands, by
  design. Pair it with whatever execution tool your agent already has; use
  backscroll for what already happened.
- **Redact-before-serve ordering** is pinned by tests: FTS match
  highlighting happens *after* redaction, so a highlight span can never
  split a secret past the redaction patterns.
- **Output caps**: `get_output` truncates head+tail around a gap marker at
  a configurable `max_bytes`, so one giant build log can't blow the
  agent's context window; `search_output` with `context_lines` often
  avoids the full fetch entirely.

The server-side implementation is ~one file of dep-free JSON-RPC; the
interesting recording machinery it sits on is covered in
[Anatomy of a PTY recorder](how-it-records.md).
