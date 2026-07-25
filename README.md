# backscroll

[![CI](https://github.com/soren-achebe/backscroll/actions/workflows/ci.yml/badge.svg)](https://github.com/soren-achebe/backscroll/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/soren-achebe/backscroll)](https://github.com/soren-achebe/backscroll/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/soren-achebe/backscroll.svg)](https://pkg.go.dev/github.com/soren-achebe/backscroll)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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
- Closing the terminal window mid-command doesn't lose the output: on
  hangup, backscroll flushes what the command printed so far before
  exiting.

## Install

Homebrew (macOS):

```sh
brew install soren-achebe/tap/backscroll
```

Debian/Ubuntu and Fedora packages (`.deb` / `.rpm`) are attached to each
[release](https://github.com/soren-achebe/backscroll/releases).

With Go:

```sh
go install github.com/soren-achebe/backscroll@latest
```

Or grab a static binary (linux/darwin × amd64/arm64) from
[releases](https://github.com/soren-achebe/backscroll/releases):

```sh
curl -sL https://github.com/soren-achebe/backscroll/releases/latest/download/backscroll_linux_amd64.tar.gz \
  | tar xz backscroll
sudo install backscroll /usr/local/bin/
```

Release tarballs include a man page (`man/backscroll.1`; source is
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

   > `backscroll run` starts a plain interactive shell — so bash reads
   > `~/.bashrc` and picks up the snippet. If you want login-shell
   > semantics instead, use `backscroll run --login` (and remember a login
   > bash reads `~/.bash_profile`, *not* `~/.bashrc`).

## Use

| command | what it does |
|---|---|
| `backscroll show` | full output of the last command |
| `backscroll show -3` | third-most-recent command |
| `backscroll show 3141` | by id · `--raw` keeps colors |
| `backscroll search <text>` | full-text search commands + outputs |
| `backscroll pick` | fuzzy-pick a command (fzf) with live output preview |
| **Ctrl-X Ctrl-P** at the prompt | pick a past command and insert it at your cursor (current line becomes the query) |
| `backscroll list -n 50` | recent commands with exit/duration/size |
| `... --exit fail --since 2h` | list/search filters: failures only, last 2 hours |
| `... --cwd .` | only commands run in this directory (or beneath it) |
| `backscroll diff 3141` | what changed vs. the **previous run of the same command** |
| `backscroll diff -2 -1` | unified diff of any two stored outputs (`-U n` context) |
| `backscroll export -1` | command + output as a markdown block, ready to paste into an issue (`--details` folds it) |
| `backscroll export 3141 --format cast` | asciicast v2 — replay with `asciinema play` |
| `backscroll export -1 --format json` | structured record for scripting |
| `backscroll sync init ~/Sync/bks` | cross-machine sync through any shared folder — encrypted, serverless ([details](#cross-machine-sync)) |
| `... --host laptop` / `--host local` | list/search/pick filter: only that machine's history |
| `backscroll stats` | how much is stored |
| `backscroll prune --older 30d` | forget old entries |
| `backscroll delete <id>` | forget one entry (that `curl -H "Authorization: ..."`) |
| `backscroll redact <id\|-N>` | permanently mask tokens/keys/passwords in a stored entry (`--dry-run` previews) |
| `backscroll off` / `on` | pause / resume recording in this session |
| `backscroll doctor` | check that everything is wired up |

The **Ctrl-X Ctrl-P** binding comes with the `backscroll init
<bash|zsh|fish>` snippet (needs [fzf](https://github.com/junegunn/fzf)):
it opens the picker over everything you've recorded — whatever you'd
already typed becomes the initial query — and inserts the selected
command back at your prompt, like Ctrl-R but you pick by *what the
command printed*, not just what you typed. Set `BACKSCROLL_NO_BIND=1`
before the snippet to opt out.

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
- **Popup search (tmux ≥ 3.2 + [fzf](https://github.com/junegunn/fzf)):**
  `backscroll init tmux >> ~/.tmux.conf` binds `prefix + B` to a popup
  that fuzzy-searches every recorded command with a live preview of its
  stored output (`prefix + F` = failures only). Any pane, any time —
  enter pages through the full output, `q` back to work.
- **Over SSH:** backscroll records on whichever machine the shell runs.
  Install it on the remote host and add the rc snippet there; use
  [sync](#cross-machine-sync) if you want the histories merged.

## Cross-machine sync

`backscroll search "connection refused"` — across your laptop, your desktop,
and that build box you SSH into:

```console
laptop$ backscroll sync init ~/Sync/backscroll   # any shared folder:
                                                 # Syncthing, Dropbox, rsync…
laptop$ backscroll sync export
desktop$ # copy ~/.config/backscroll/sync.key from the laptop, then:
desktop$ backscroll sync init ~/Sync/backscroll
desktop$ backscroll sync import
desktop$ backscroll search "connection refused"      # both machines' history
 3141  2d ago  exit 1  [laptop] curl http://10.0.0.7:8080/health
       …connection refused…
desktop$ backscroll list --host laptop               # or filter by machine
```

No server, no account: each machine appends its own **end-to-end encrypted**
log (XChaCha20-Poly1305, shared key file you copy once) to the folder and
imports the others'. Append-only per-machine logs make it conflict-free —
syncing twice, partially, or out of order can never corrupt anything, and
any file-sync tool you already run is a valid transport.

Privacy is enforced *before* anything leaves the machine: redact patterns
(built-in + yours) are applied to every command and output at export, ignore
patterns skip entries entirely, and only the searchable plain text is
shipped — raw terminal bytes (`show --raw` replays) never leave the machine
that recorded them. `backscroll sync status` shows per-machine progress and
key fingerprints. Design notes: [docs/sync-design.md](docs/sync-design.md).

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
  history file. Same for `~/.config/backscroll/sync.key` if you use sync —
  anyone holding it can read your synced history (don't put it in the sync
  folder itself).
- Don't run it on shared accounts.

## Status

Early but working: bash, zsh, and fish on Linux and macOS, with
ignore-patterns, session pause (`off`/`on`), output diffing, the fzf picker
(`pick`, Ctrl-X Ctrl-P, tmux popups), encrypted cross-machine sync, and a
`doctor` command.
Issues and PRs welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
