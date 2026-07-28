# Shell history files: a field guide

*Notes from implementing `backscroll import`
([backscroll](https://github.com/soren-achebe/backscroll) seeds its database
from the history you already have). Six tools, six on-disk formats, and no
two agree on how to encode a newline. Everything below was pinned against
the real tools — atuin 18.17, nushell 0.114, PSReadLine on pwsh 7.5,
zsh 5.9, fish 4.8, bash 5.2 — with byte-level fixtures in
[`internal/histimport`](../internal/histimport). Companion pieces:
[osc133.md](osc133.md) (shell-integration marks),
[how-it-records.md](how-it-records.md) (the recorder itself).*

## The short version

| Source | File | Format | Timestamps | Exit / cwd / host | Multiline encoding |
|---|---|---|---|---|---|
| bash | `~/.bash_history` | plain lines (+ optional `#<epoch>` markers) | only with `HISTTIMEFORMAT` | — | raw lines under one marker |
| zsh | `~/.zsh_history` | plain or `: <start>:<elapsed>;cmd`, **metafied bytes** | only with `EXTENDED_HISTORY` | — | `\` + newline |
| fish | `~/.local/share/fish/fish_history` | yaml-ish blocks | always | — | `\n` escape in `cmd:` value |
| nushell (default) | `history.txt` | plain lines | — | — | literal `<\n>` |
| nushell (sqlite) | `history.sqlite3` | SQLite | epoch **ms** | exit (nullable), cwd, FQDN host | real newlines |
| PSReadLine (pwsh) | `ConsoleHost_history.txt` | plain lines | — | — | trailing backtick |
| atuin | `history.db` | SQLite | epoch **ns** | exit, cwd, `host:user` | real newlines |

If you only remember one thing: **every plain-text format has an ambiguity
that the shell itself cannot round-trip**, and the right importer behavior
is to decode exactly the way the shell decodes, ambiguity included.

## bash: `~/.bash_history`

The baseline format is one command per line, nothing else. Two features
complicate it:

**Timestamps.** With `HISTTIMEFORMAT` set, bash writes a comment line
`#1785174961` (epoch seconds) *before* each entry. Detection has to be
heuristic: `#` + digits only, value in a plausible epoch range
(1,000,000,000–9,999,999,999 ≈ years 2001–2286). A command that
is literally `#1785174961` is indistinguishable from a marker — every
importer (including bash's own `history -r`) shares this ambiguity; don't
try to be cleverer than the shell.

**Multiline entries.** With `shopt -s lithist` (or via timestamped mode),
a multi-line command is written as raw lines under one marker. The naive
rule — "everything until the next `#epoch` marker is one entry" — is
wrong in practice, because history files are *mixed*: a session run
without `HISTTIMEFORMAT` appends plain unmarkered lines to the same file.
On a real mixed file the naive rule gloms entire later sessions into the
last marked entry (we hit this live). The rule that matches observed
files: a line continues the previous entry **only while the accumulated
text is syntactically incomplete** — inside an unbalanced quote or ending
in an unescaped backslash. That keeps genuine `lithist` heredocs and
quoted blocks whole and lets complete plain lines stand alone.

## zsh: `~/.zsh_history`

Two things to know, one famous:

**Metafication.** zsh does not write raw bytes ≥ 0x80. Any such byte `b`
is written as `0x83` (Meta) followed by `b ^ 0x20`. If you `cat` a
zsh history file containing UTF-8 and see mojibake, this is why. Decoding
is trivial (scan for 0x83, XOR the next byte with 0x20) but mandatory —
skip it and every non-ASCII command imports corrupted. This is the single
most common bug in third-party zsh history readers.

**Extended format.** With `setopt EXTENDED_HISTORY` each entry is

```
: 1785174961:0;echo hi
```

— `: <start-epoch>:<elapsed-seconds>;<command>`. Without the option it's
just the command. Files mix both freely (option toggled over the years),
so parse per-line: if it doesn't match the `: N:N;` shape, the whole line
is the command. Elapsed is whole seconds, so you get durations too —
zsh is the only plain-text format that records them.

**Multiline.** A backslash as the last byte of a line means the newline
is part of the command: join with `\n`, drop the backslash. (Metafication
applies after joining in zsh's writer; unmetafy per joined entry.)

## fish: `~/.local/share/fish/fish_history`

Looks like YAML, is not YAML — do not reach for a YAML parser (real files
contain values a strict parser rejects). Blocks are:

```yaml
- cmd: git commit -m 'wip'
  when: 1785174961
  paths:
    - src/main.rs
```

`when` is always present (fish has always timestamped history — the only
plain-text format where you never lose ordering). `paths` records files
the command touched; fine to ignore. In the `cmd:` value fish escapes
backslash as `\\` and newline as `\n`; unknown escapes should be kept
verbatim (fish's own reader does). That's the whole grammar.

## nushell: two backends

**Plaintext (`history.txt`, the default).** One command per line, and
reedline writes an embedded newline as the three literal bytes `<\n>`.
A command whose *text* contains `<\n>` is therefore ambiguous in the file
— reedline decodes it as a newline on read-back, so match that. No
timestamps, no metadata.

**SQLite (`history.sqlite3`, opt-in).** The rich one. Schema (nushell
0.114):

```sql
CREATE TABLE history (
  id integer primary key autoincrement,
  command_line text not null,
  start_timestamp integer,   -- epoch MILLIseconds
  session_id integer,
  hostname text,             -- full FQDN
  cwd text,
  duration_ms integer,
  exit_status integer,       -- NULLable!
  more_info text
) strict;
```

Gotchas we pinned live: `start_timestamp` is epoch **milliseconds** (atuin
uses nanoseconds; guess wrong and every command ran in 1970 or 58,000 AD).
`exit_status` is nullable — nu's own `exit` command leaves a row with NULL
exit; treat "no exit" as distinct from 0. `hostname` is the full
`gethostname()` FQDN (`box.example.com`) where most tools store short
names — trim to the first label if you're merging sources. The DB runs in
WAL mode; open it `mode=ro` and reading works fine while a live nu session
holds it open.

## PSReadLine: `ConsoleHost_history.txt`

One command per line, no timestamps, and the strangest multiline encoding
of the lot: a multiline command is written as consecutive lines that each
end in a backtick **except the last**. The backtick is a marker, not
content — strip it, keep the newline. Backticks anywhere else in a line
are verbatim. Consequence: a single-line command whose text genuinely
ends in a backtick is ambiguous, and PSReadLine itself reads it back as a
continuation — match that, don't outsmart it.

Locating the file: `%APPDATA%\Microsoft\Windows\PowerShell\PSReadLine\`
on Windows, `~/.local/share/powershell/PSReadLine/` elsewhere — and pwsh
honors `XDG_DATA_HOME` on non-Windows platforms, so probe that first.

## atuin: `history.db`

SQLite throughout. The columns an importer needs
(atuin 18.17):

- `timestamp`, `duration`: epoch **nanoseconds** / nanoseconds.
- `exit`: −1 means "not recorded" (killed session), not "exit −1".
- `hostname`: the string `host:user` — split on the first `:`.
- `cwd`: the literal string `"unknown"` when unknown, not NULL.
- `deleted_at`: soft deletes; filter `IS NULL` or you resurrect what the
  user purged.

`rowid` is a stable per-file ordering key. Because atuin syncs across
machines, one import can cover a user's whole fleet — it's the richest
seed source by far.

## Lessons that generalize

**Idempotent re-import needs a stable key.** Line numbers aren't one —
history files get trimmed from the front (`HISTSIZE`), so positions
shift. For timestamped sources, the timestamp (plus a per-second
collision counter) is stable across trims. For timestamp-less sources
the only stable key is a hash of the command text — which collapses
duplicate commands into one entry. For seed data with no attached output
that's arguably a feature (a deduplicated picker), but it's forced, not
chosen.

**Decode like the shell, ambiguity included.** Every plain-text format
above has at least one input it can't round-trip (`#epoch` command in
bash, `<\n>` in nu, trailing backtick in pwsh). The shell's own reader
resolves each ambiguity in a specific direction; an importer that
resolves it the *other* way is wrong on every file the shell ever
rewrites, which is most of them.

**Pin against real tools, not documentation.** Almost nothing above is
documented. The fixtures in our test suite were produced by driving the
actual shells (over a PTY where needed — reedline won't even start
without a terminal that answers `DSR 6n`) and committing the resulting
bytes. Formats drift; byte fixtures notice.

**Watch the units.** Seconds (zsh, fish, bash), milliseconds (nushell),
nanoseconds (atuin) — three units spanning nine orders of magnitude, and
nothing in the files says which is which. Guess wrong and every import
"ran" in 1970, silently.

---

*If you want your history not just imported but recorded with full output
from here on out, that's what [backscroll](https://github.com/soren-achebe/backscroll)
does — `backscroll import` is the on-ramp.*
