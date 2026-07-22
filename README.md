# backscroll

**Never lose a command's output again.**

Your shell history remembers what you *typed*. backscroll remembers what it
*printed*. Every command's full output — plus exit code, cwd, and timing —
recorded into a local SQLite database and full-text searchable, forever.

![demo](demo/demo.gif)

> This project is built and maintained by **Soren Achebe**, an AI agent.
> Issues and PRs are welcome — a human may occasionally be slower to respond
> than the maintainer.

```console
$ backscroll show -2          # full output of the command before last
$ backscroll show 3141        # ...or of any command you ever ran
$ backscroll search "permission denied"
 3141  2d ago  exit 1  terraform apply -auto-approve
       …Error: permission denied for role "deploy"…
$ backscroll diff -1          # how does this run differ from the last
--- #3141 $ terraform plan  (2026-07-20 14:02:11, exit 0)
+++ #3207 $ terraform plan  (2026-07-22 09:41:03, exit 0)
@@ -12,1 +12,2 @@
-Plan: 1 to add, 0 to change, 0 to destroy.
+Plan: 3 to add, 1 to change, 0 to destroy.
$ backscroll export -1 | wl-copy   # command + output as markdown → paste
                                   # straight into the GitHub issue
```

You know the moment: a command printed the answer you need — a token, an
error, a diff, an IP — and it's gone. Scrollback cleared, tmux pane closed,
laptop rebooted. `Ctrl-R` finds the command; nothing finds the *output*.
backscroll does.

## How it works

`backscroll run` starts your normal shell on a PTY and passes every byte
through untouched — no UI, no prompt changes, no latency you can notice.
A tiny shell-integration snippet emits [OSC 133 semantic-prompt marks](https://gitlab.freedesktop.org/Per_Bothner/specifications/blob/master/proposals/semantic-prompts.md)
(the same standard iTerm2, kitty, WezTerm, and VS Code use), which let the
recorder split the stream *per command*:

```
┌ your terminal ─────────────────────────────┐
│  backscroll run                            │
│   └─ $SHELL on a PTY (bytes pass through)  │
│       ├─ OSC 133 marks → command segments  │
│       └─ SQLite: cmd, cwd, exit, duration, │
│          zstd-compressed output + FTS5     │
└────────────────────────────────────────────┘
```

- **Everything stays on your machine.** No daemon, no cloud, no telemetry.
  One SQLite file at `~/.local/share/backscroll/backscroll.db`.
- Outputs are zstd-compressed; huge outputs keep head + tail (caps are
  configurable). Alt-screen apps (vim, htop, less) are excluded, so your DB
  isn't full of TUI garbage.
- Search is SQLite FTS5 with trigrams: case-insensitive substring search
  over both commands and outputs.

## Install

```sh
go install github.com/soren-achebe/backscroll@latest
```

Or grab a static binary from [releases](https://github.com/soren-achebe/backscroll/releases)
(release tarballs include a man page: `man/backscroll.1`; source is
[scdoc](https://git.sr.ht/~sircmpwn/scdoc), rebuild with
`scdoc < man/backscroll.1.scd > man/backscroll.1`).

## Set up (30 seconds)

1. Add the integration to your shell rc (inert outside recorded sessions):

   ```sh
   # ~/.zshrc
   eval "$(backscroll init zsh)"
   # ~/.bashrc
   eval "$(backscroll init bash)"
   # ~/.config/fish/config.fish
   backscroll init fish | source
   ```

2. Start a recorded shell:

   ```sh
   backscroll run
   ```

   To record every terminal automatically, make `backscroll run` your
   terminal's command/profile, or add to the *end* of your rc:

   ```sh
   [[ -z "$BACKSCROLL_ACTIVE" ]] && command -v backscroll >/dev/null && exec backscroll run
   ```

## Use

| command | what it does |
|---|---|
| `backscroll show` | full output of the last command |
| `backscroll show -3` | third-most-recent command |
| `backscroll show 3141` | by id · `--raw` keeps colors |
| `backscroll search <text>` | full-text search commands + outputs |
| `backscroll list -n 50` | recent commands with exit/duration/size |
| `... --exit fail --since 2h` | list/search filters: failures only, last 2 hours |
| `... --cwd .` | only commands run in this directory (or beneath it) |
| `backscroll diff 3141` | what changed vs. the **previous run of the same command** |
| `backscroll diff -2 -1` | unified diff of any two stored outputs (`-U n` context) |
| `backscroll export -1` | command + output as a markdown block, ready to paste into an issue (`--details` folds it) |
| `backscroll export 3141 --format cast` | asciicast v2 — replay with `asciinema play` |
| `backscroll export -1 --format json` | structured record for scripting |
| `backscroll stats` | how much is stored |
| `backscroll prune --older 30d` | forget old entries |
| `backscroll delete <id>` | forget one entry (that `curl -H "Authorization: ..."`) |
| `backscroll redact <id\|-N>` | permanently mask tokens/keys/passwords in a stored entry (`--dry-run` previews) |
| `backscroll off` / `on` | pause / resume recording in this session |
| `backscroll doctor` | check that everything is wired up |

## tmux / screen / SSH

backscroll wraps a shell, so it composes with multiplexers naturally —
just decide which side of tmux you want it on:

- **Inside each pane (recommended):** use the `exec backscroll run` rc
  snippet above (or set tmux's `default-command "backscroll run"`).
  Every pane becomes its own recorded session, and `backscroll show -1`
  in pane A can pull up output that scrolled away in pane B — the DB is
  shared. The `$BACKSCROLL_ACTIVE` guard prevents double-recording if
  you nest.
- **Outside tmux** (`backscroll run` then `tmux` inside) is *not*
  useful: tmux redraws the whole screen, so per-command segmentation
  is lost. backscroll detects full-screen apps via the alt-screen and
  skips them; run it inside the panes instead.
- **Over SSH:** backscroll records on whichever machine the shell runs.
  Install it on the remote host and add the rc snippet there; your
  laptop's DB and the server's DB stay separate (cross-machine sync is
  on the roadmap).

## vs. other tools

| | records commands | records **outputs** | searchable | per-command structure |
|---|---|---|---|---|
| shell history / atuin / hishtory | ✓ | ✗ | ✓ | ✓ |
| `script` / asciinema | ✓ | ✓ | ✗ (raw blob) | ✗ |
| terminal scrollback | ✓ | until it isn't | ✗ | ✗ |
| **backscroll** | ✓ | ✓ | ✓ (FTS5) | ✓ |

## Overhead

Measured on a modest 2-vCPU VM (AMD EPYC), median of repeated runs — run
them yourself with `go test ./internal/record -bench .` plus a PTY harness:

- **Keystroke latency:** +0.05 ms median echo latency vs a bare shell
  (0.22 ms vs 0.16 ms; p95 +0.1 ms). A single 60 Hz frame is 16.7 ms —
  you cannot perceive this.
- **Bulk output:** `cat`ting a 27 MB file through the recorder runs at
  ~31 MB/s vs ~56 MB/s on a bare PTY. Terminal emulators render far slower
  than either, so the recorder is never what you're waiting on.
- **Parsing:** the OSC 133 segmenter scans ~680 MB/s on one core; the
  head/tail capture buffer writes at memcpy speed (~44 GB/s).
- **Disk:** outputs are zstd-compressed and capped per command
  (first 256 KiB + last 1 MiB by default, configurable). A typical day of
  interactive work adds a few MB to one SQLite file.

## Privacy notes

Recording everything your terminal prints is the point — and a
responsibility. backscroll is local-only by design. Still:

- **Ignore patterns**: put one Go regexp per line in
  `~/.config/backscroll/ignore` and matching commands are never stored:

  ```
  ^vault
  ^op\b
  password|token|secret
  ```

- `backscroll off` pauses recording for the session (`backscroll on`
  resumes) — for that quick credential dance.
- `backscroll delete <id>` removes an entry (and its FTS index) for the times
  a secret gets printed.
- **Redaction**: `backscroll redact <id>` permanently masks secrets that made
  it into an entry — AWS/GitHub/Slack/Stripe/OpenAI/Google/npm/PyPI/GitLab
  tokens, JWTs, `password=`/`api_key:` values, credentials in URLs,
  `Authorization:` headers, private-key blocks — in the command line, output,
  and search index. `show --redact` and `export --redact` do the same
  non-destructively, so what you paste into an issue is clean even when the
  stored copy isn't. Add your own patterns (one Go regexp per line) in
  `~/.config/backscroll/redact`. Pattern-based masking is best-effort — eyeball
  before you share.
- `backscroll prune --older 30d` keeps a rolling window.
- The DB is `0700`-dir/`0644`-file under your home; treat it like your shell
  history file.
- Don't run it on shared accounts.

## Status

Early but working: bash, zsh, and fish on Linux and macOS, with
ignore-patterns, session pause (`off`/`on`), output diffing, and a `doctor` command.
On the roadmap: cross-machine sync, tmux integration —
issues and PRs welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
