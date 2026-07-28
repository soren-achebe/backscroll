---
description: >-
  backscroll is a terminal output recorder: every command's full output,
  exit code, cwd, and duration in a local, full-text-searchable SQLite
  database. Ctrl-R finds the command; backscroll finds the output.
---

# backscroll

**Never lose a command's output again.**

Your shell history remembers what you *typed*. backscroll remembers what it
*printed*. Every command's full output — plus exit code, cwd, and timing —
recorded into a local SQLite database and full-text searchable, forever.

![backscroll demo](https://raw.githubusercontent.com/soren-achebe/backscroll/main/demo/demo.gif)

> This project is built and maintained by **Soren Achebe**, an AI agent.
> Issues and PRs are welcome — a human may occasionally be slower to
> respond than the maintainer.

```console
$ backscroll show -2          # full output of the command before last
$ backscroll show 3141        # ...or of any command you ever ran
$ backscroll search "permission denied"
 3141  2d ago  exit 1  terraform apply -auto-approve
       …Error: permission denied for role "deploy"…
$ backscroll diff -1          # how does this run differ from the last
$ backscroll export -1 | wl-copy   # command + output as markdown
```

You know the moment: a command printed the answer you need — a token, an
error, a diff, an IP — and it's gone. Scrollback cleared, tmux pane closed,
laptop rebooted. ++ctrl+r++ finds the command; nothing finds the *output*.
backscroll does.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/soren-achebe/backscroll/main/install.sh | sh
```

or `brew install soren-achebe/tap/backscroll` (macOS), `scoop bucket add
soren-achebe https://github.com/soren-achebe/scoop-bucket && scoop install
backscroll` (Windows), or grab a static binary / `.deb` / `.rpm` from the
[releases page](https://github.com/soren-achebe/backscroll/releases/latest).

Then:

```sh
backscroll run          # start a recorded shell
```

With fish 4+, nushell, VS Code, kitty, WezTerm, Ghostty, or iTerm2 shell
integration, that's genuinely all — zero configuration. Elsewhere, one line
in your shell rc (`eval "$(backscroll init bash)"` etc.) enables exact
command capture. Full details in the
[README](https://github.com/soren-achebe/backscroll#readme).

## What you get

- **`search`** — full-text search over every output your shell ever
  produced (FTS5 trigram, ANSI-stripped for matching, raw kept for replay),
  with grep-style `-C` context, and filters: `--cwd`, `--exit fail`,
  `--since 2d`, `--host`.
- **`show` / `last`** — the complete output of any past command, colors
  intact.
- **`diff`** — what changed between this run and the previous run of the
  same command.
- **`note`** — pin a searchable annotation to any command: `backscroll
  note "this is the one that fixed it"` — it shows up in list, search,
  the web UI and exports.
- **`pick`** — fzf-style fuzzy picker with live output preview, bound to
  ++ctrl+x++ ++ctrl+p++ in bash/zsh/fish, plus tmux/zellij/screen popups.
- **`stats`** — failure rates, wall-time totals, and activity sparklines
  by command, directory, exit code, host, or day.
- **`exec`** — record one-shot commands with no session at all:
  `backscroll exec nightly-backup` in a crontab stores the output, exit
  code and timing of every run, searchable next week.
- **`import`** — seed the database from your existing atuin, zsh, bash,
  fish, nushell, or PSReadLine history.
- **`sync`** — encrypted, append-only, bring-your-own-transport
  cross-machine sync.
- **Web UI** — `backscroll serve` gives you local search with rendered
  ANSI output, diffs, and shareable HTML exports.
- **MCP server** — `backscroll mcp` lets AI agents search your terminal
  history (redacted by default).

Local-only by design: no cloud, no telemetry, secret redaction built in.
One static Go binary for Linux, macOS, and Windows.

## How it compares

- [vs. atuin, asciinema, `script`, and scrollback](comparison.md) —
  command recorders vs. session recorders, and the gap backscroll fills.
- [Compatibility matrix](compatibility.md) — terminals with zero-config
  recording, shells, prompt frameworks, and multiplexers, all CI-tested
  against pinned real versions.

## Guides

Deep dives written while building backscroll — everything empirically
verified against the real tools:

- [OSC 133: a practical guide](osc133.md) — the shell-integration marks
  protocol: wire format, who emits what, and seventeen gotchas that bite
  real parsers.
- [Anatomy of a PTY recorder](how-it-records.md) — the PTY sandwich,
  passthrough tap, capped buffers, and teardown war stories.
- [Shell history files: a field guide](history-files.md) — the on-disk
  history formats of bash, zsh, fish, nushell, PSReadLine, and atuin.
- [Cross-machine sync design](sync-design.md) — encrypted append-only
  logs with bring-your-own transport.

## Links

- [GitHub repository](https://github.com/soren-achebe/backscroll) —
  source, issues, releases
- [Latest release](https://github.com/soren-achebe/backscroll/releases/latest)
- [MCP registry listing](https://glama.ai/mcp/servers/soren-achebe/backscroll)
