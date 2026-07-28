---
description: >-
  How backscroll compares to atuin, hishtory, script, asciinema, and your
  terminal's scrollback — and why "records outputs" is a different job
  than "records commands".
---

# backscroll vs. atuin, asciinema, `script`, and scrollback

Short version: the existing tools split into two camps — **command
recorders** (shell history, atuin, hishtory) and **session recorders**
(`script`, asciinema). backscroll sits in the gap between them: it records
**what each command printed**, structured **per command**, and makes it
searchable. That's a different job, and if you already use atuin, the two
work fine together (we test that in CI).

| | records commands | records **outputs** | full-text searchable | per-command structure |
|---|---|---|---|---|
| shell history / [atuin](https://github.com/atuinsh/atuin) / [hishtory](https://github.com/ddworken/hishtory) | ✓ | ✗ | ✓ | ✓ |
| `script` / [asciinema](https://asciinema.org/) | ✓ | ✓ | ✗ (raw blob) | ✗ |
| terminal scrollback / `tmux capture-pane` | ✓ | until it isn't | ✗ | ✗ |
| **backscroll** | ✓ | ✓ | ✓ (SQLite FTS5) | ✓ |

## vs. atuin (and shell history in general)

[atuin](https://github.com/atuinsh/atuin) is an excellent shell history
manager: it puts your *command lines* in SQLite, syncs them, and gives you
a great ++ctrl+r++. But it stores what you **typed**, never what the
command **printed**. The daily failure mode it can't help with:

> A command printed the answer you need — a token, an error message, an
> IP, a stack trace — and it's gone. Scrollback cleared, tmux pane closed,
> laptop rebooted. ++ctrl+r++ re-finds the *command*; nothing re-finds the
> *output*.

backscroll records the output too: `backscroll show -1` replays the full
output (with colors) of any past command; `backscroll search "connection
refused"` finds every command that ever *printed* that, with exit code,
cwd, and timing.

**They are not competitors in practice.** atuin owns ++ctrl+r++ and
cross-machine command sync; backscroll owns "what did that print?". Our CI
drives real recorded sessions with atuin's actual shell integration (its
Ctrl-R TUI included) under bash, zsh, and fish, and asserts both tools
keep working. And if you do want just one tool: `backscroll import atuin`
seeds backscroll's database from your existing atuin history, so you keep
your past either way.

## vs. `script` and asciinema

`script(1)` and [asciinema](https://asciinema.org/) record the whole
session as one raw byte stream (or timed cast). Great for *replaying a
demo*; poor for *finding anything*:

- No boundaries: the recording is one blob per session, not one record
  per command. "What did `terraform apply` print last Tuesday" means
  scrubbing through recordings by hand.
- No metadata: no exit codes, no per-command cwd or duration to filter on.
- No search: escape sequences interleave the text, so even `grep` over the
  raw file is unreliable.

backscroll splits the stream at command boundaries (using [OSC 133
shell-integration marks](osc133.md)), strips ANSI for the search index,
keeps the raw bytes for faithful colored replay, and stores exit code,
cwd, duration, and host with every command. You can still get a cast when
you want one: `backscroll export --format asciicast` emits a valid
asciicast v2 file for any recorded command.

## vs. your terminal's scrollback

Scrollback is the tool everyone actually uses for this today, and it's
the least reliable: it's RAM, capped, per-pane, and gone on close, clear,
reboot, or SSH disconnect. tmux's `capture-pane` and kitty's
`show_last_command_output` prove the demand for per-command output access
— but they only reach what's still in the buffer of that one pane.
backscroll is the durable version: every pane, every session, every
machine (via [encrypted sync](sync-design.md)), searchable months later.

## What backscroll does *not* do

Honesty section. backscroll is deliberately **not**:

- **a history UI replacement** — it binds ++ctrl+x++ ++ctrl+p++ for its
  fuzzy picker and leaves ++ctrl+r++ alone. Keep atuin/fzf there.
- **a cloud service** — local-only by design; sync is offline files you
  move yourself (encrypted, redacted-before-export). Nothing phones home.
- **a session replayer for demos** — it stores per-command output, not
  keystroke timing for the whole session. For pretty demo recordings,
  asciinema is the right tool (we use it for our own demo GIF).
- **magic** — output is capped per command (defaults: first 256 KiB +
  last 1 MiB, configurable), full-screen TUI apps (vim, htop) are
  excluded via alt-screen detection, and secrets matching redaction
  patterns are maskable or permanently scrubbable.

## Trying it takes 30 seconds

```sh
curl -fsSL https://raw.githubusercontent.com/soren-achebe/backscroll/main/install.sh | sh
backscroll run
```

With fish 4+, nushell, VS Code, kitty, WezTerm, Ghostty, or iTerm2 shell
integration that's all — [zero configuration](compatibility.md). See the
[README](https://github.com/soren-achebe/backscroll#readme) for the
one-line rc setup elsewhere.
