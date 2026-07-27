# Anatomy of a PTY recorder

*How [backscroll](https://github.com/soren-achebe/backscroll) turns a live
terminal session into per-command records without the shell, the terminal,
or the user noticing. This is the recorder-side companion to
[OSC 133: a practical guide](osc133.md), which covers the protocol; this
doc covers everything around the protocol — the PTY plumbing, the capture
pipeline, and the failure modes that only show up when you run it for real.*

## Constraints first

Three invariants drive the whole design:

1. **Zero intrusion.** The byte stream between shell and terminal passes
   through *unmodified*. No injected output, no rewritten escape
   sequences, no prompt decoration. If backscroll dies, your session looks
   exactly like it did without it. Anything less and you can't trust it
   under vim, fzf, ssh, or a full-screen TUI.
2. **Terminal-agnostic.** No dependency on which terminal emulator you
   use, no plugin per terminal. Everything happens on the PTY, below the
   emulator.
3. **Local-only and durable.** One SQLite file on your disk. Survives
   terminal crashes, `kill`, closed windows — losing the last command's
   output to a SIGHUP would defeat the point of the tool.

## The PTY sandwich

`backscroll run` puts itself between your terminal and your shell:

```
terminal emulator
      │  (backscroll's stdin/stdout — the "outer" tty)
backscroll run
      │  (a new pty pair; shell runs on the slave side)
$SHELL  (interactive)
```

Concretely:

- Allocate a pty pair, spawn `$SHELL` on the slave, keep the master
  (`ptmx`).
- Put the **outer** tty into raw mode. This is the step people forget:
  the *inner* pty handles all line discipline (echo, ^C → SIGINT, canonical
  editing) for the shell, so the outer tty must do *nothing* — otherwise
  every keystroke gets processed twice and echoes twice.
- Two copy loops: stdin → ptmx (keystrokes down), ptmx → stdout (output
  up). The upward loop is where the recorder taps the stream.
- Forward `SIGWINCH`: on every resize, copy the outer tty's winsize onto
  the pty. Miss this and vim opens at the wrong size after any resize.

The shell is spawned as a plain interactive shell, *not* a login shell.
This was a real bug (v0.1.2): `$SHELL -l` makes bash skip `~/.bashrc`,
which is exactly where the integration snippet lives — so on a stock setup
the recorder ran fine and silently recorded nothing.

## The passthrough tap

The upward copy loop hands every chunk to an incremental parser
(`internal/record/parser.go`) and then writes the **original bytes** to
stdout. The parser is an observer, not a filter — it's a small state
machine (normal / ESC / OSC / CSI) that:

- recognizes the OSC 133 `A`/`B`/`C`/`D` marks that delimit prompt,
  command input, output, and exit (protocol details in
  [osc133.md](osc133.md));
- reads the command text from a private mark our shell snippets emit
  (`OSC 6973;cmd=<base64>` — base64 because command lines can contain
  every byte you'd otherwise have to escape, including BEL and ESC);
- reads the cwd from `OSC 7` (`file://host/path`);
- captures output bytes between `C` and `D` into the current command's
  buffer.

Because input arrives in arbitrary chunks from the kernel, a mark can be
split anywhere — `ESC ]1` in one read, `33;D;0` + BEL in the next. The
parser carries state across `Feed()` calls, and a fuzz test asserts
**chunking invariance**: any byte stream produces identical events
regardless of how it's sliced (400k+ random executions and counting).
That single property test caught more parser bugs than everything else
combined.

Two spans are deliberately *not* captured:

- **Alt-screen content.** When a command switches to the alternate screen
  (`CSI ? 1049 h` — vim, less, htop), capture pauses until it switches
  back. Full-screen TUIs redraw constantly; recording them produces
  megabytes of cursor-movement soup that's useless for recall. You still
  get the command, exit code, duration — just not the frame spam.
- **Paused spans.** `backscroll off` doesn't talk to the recorder over
  any IPC — the subcommand simply prints a private `OSC 6973;rec=off`
  mark in-band. The recorder sees it in the stream, exactly ordered with
  respect to everything else. No socket, no race.

## The capped buffer

Unbounded capture is a footgun (`cat 4GB.log`, an accidental binary dump,
a runaway loop). Each command's output goes into a **head + tail** buffer:
the first 256 KiB and the last 1 MiB (both configurable), with a marker
recording how many bytes were dropped between them. Head is where the
error usually is; tail is where the summary usually is; the middle of a
100 MB build log is the part nobody asks for.

The tail is a fixed-size ring. The first implementation wrote it
byte-by-byte on the modulo index — correctness-obvious, and it benched at
168 MB/s, slower than the parser it fed. Rewritten around `copy()` with
at most two segment copies per write, it does ~44 GB/s (i.e. memcpy
speed; it stopped being a term in the throughput equation). A randomized
property test pins the rewrite to the naive implementation's output.

## Storage

On each `D` mark, the buffer is flushed to SQLite
(`modernc.org/sqlite` — pure Go, so the binary stays static and
cross-compiles trivially) as one row: command, cwd, exit code, start/end
time, session id, and **two versions of the output**:

- the raw bytes, zstd-compressed — colors and all, for `show --raw`
  replay and asciicast export;
- an ANSI-stripped plain-text rendering, zstd-compressed, which also
  feeds an FTS5 index (trigram tokenizer, so substring search works
  without word-boundary guessing).

The FTS index is worth a detour, because the naive setup doubles your
database. A normal fts5 table *stores its own uncompressed copy* of
everything you index — on a 5,000-command soak DB that content shadow
was 67 MB of a 151 MB file, dwarfing the 1 MB of actual zstd'd data
sitting next to it. backscroll instead declares `commands_fts` as an
[external-content](https://sqlite.org/fts5.html#external_content_tables)
table whose "content" is a **view** that decompresses `plain_z` on
demand through a registered scalar function (`bks_unz`). fts5 doesn't
care that the content table isn't a table; it just runs `SELECT` against
it when `snippet()`/`highlight()` need text. Same features, plain text
stored exactly once, compressed — the same soak DB drops to 72 MB.

Two costs, for honesty: external-content index writes must go through
fts5's special `'delete'` command *with the originally-indexed text*
(get that wrong and the index silently drifts — `backscroll doctor
--reindex` rebuilds it from scratch if you ever suspect it), and a
plain `sqlite3` CLI can't query the view since it lacks `bks_unz` —
though the `commands` table itself stays ordinary and inspectable.
Also learned the hard way: fts5 `'delete'` only *marks* rows, so
`prune` follows up with `'optimize'` before `VACUUM`, or a
fully-pruned index keeps its full size on disk.

Stripping ANSI *before* indexing matters more than it sounds: escape
sequences routinely land in the middle of words (`conn` + SGR reset +
`ection`), so an index over raw bytes silently fails to match the exact
error message you're searching for.

## Recovering the command from its echo

Plain-OSC 133 emitters (Ghostty, iTerm2, Windows Terminal, manually
installed integration scripts) mark where the prompt ends (`B`) and
where execution starts (`C`) — but never say what the command *was*.
The recorder still holds the missing piece: everything the shell
**echoed** between those two marks is the user's typing, as rendered by
readline/ZLE — keystrokes interleaved with backspace rubouts, cursor
motion, completion redraws, syntax-highlight recoloring, and whatever
an fzf popup splattered on the screen.

So when no better source exists, backscroll replays that region through
a small prompt-relative grid emulator and stores the final visible
line. The priority order is: our snippet's OSC 6973 text (authoritative)
→ an emitter-provided cmdline (fish ≥ 4.0, VS Code `633;E`, kitty,
WezTerm) → echo reconstruction.

Three details make this honest rather than hopeful:

- **The prompt must be replayed too.** The `B` mark fires with the
  cursor *after* the prompt, and line-editor redraws are computed from
  that true position. ZLE's wrapped-line handling writes a character at
  the last column to force the wrap, then `CR` + erase-line + rewrite on
  the next row — replayed from column 0 instead of column
  `len(prompt)`, that erase lands on the *first* row and destroys the
  line (a 125-character command reconstructs as its last 7 characters;
  this was found by diffing against a real VT emulator's render of the
  same bytes). The parser therefore captures from the `A`/`133;P` mark
  and phase-tags cells as prompt vs. input; only input cells are
  extracted.
- **Never-written ≠ space.** Cells track whether anything was ever
  drawn there. A typed space writes its cell; a zsh `RPROMPT` jumps the
  cursor and leaves a gap of untouched cells. That distinction lets
  extraction trim right-aligned decorations without ever eating real
  interior spacing.
- **Refuse to guess.** Absolute cursor addressing or scroll regions
  can't be mapped into a prompt-relative grid, so the reconstruction
  aborts and the entry falls back to `(unknown command)` — a wrong
  "command" in the database is strictly worse than an unlabeled one.
  Same for oversized regions: a >32 KB paste is discarded, not
  truncated into a misleading prefix. And a region whose input ends in
  a bare `^C` (no echoed Enter before readline tears down bracketed
  paste) is a line that was *killed*, not executed — some emitters
  re-fire a phantom pre-exec mark on SIGINT, and without this check the
  literal text `^C` gets stored as a command.

The alternate screen is frozen during replay, so a full-screen fzf
Ctrl-R session contributes nothing and the final readline redraw of the
picked command is what survives. Multiline input works too: emitters
that mark continuation prompts (`133;P;k=s`) get their PS2 `> `
excluded from the reconstruction entirely.

## Session teardown is the hard part

The happy path is easy. The paths that cost real debugging:

- **SIGHUP/SIGTERM with a command still running.** Terminal window
  closes mid-command — precisely the moment you'll want the output later.
  The recorder forwards the hangup to the shell, which kills its jobs and
  exits; the slave side closes, the upward read gets EIO, and the normal
  unwind path flushes the partial output with the marks it has.
- **The fallback that didn't work.** If the shell ignores the hangup, the
  obvious fallback is closing the master. Measured reality: on Linux,
  `ptmx.Close()` does **not** unblock a goroutine parked in
  `ptmx.Read()` — the read sat for 58 seconds until an unrelated
  wakeup. The working order is: signal the shell, wait a 3-second grace
  period, *then* close — by which point the slave side is closed and the
  reader has already unwound on EIO in every observed case.
- **The `exit` stub.** fish (unlike bash/zsh) emits a full mark cycle for
  a plain `exit`, which stored a useless zero-output entry at the end of
  every session until it was special-cased.
- **`bind -x` widgets.** The shell-side integration must not fire for
  readline/zle widget bodies (fzf's Ctrl-R runs a whole process tree
  inside one). Getting this right across bash 5.x *and* bash 3.2.57
  (macOS `/bin/bash`, where `READLINE_LINE` doesn't exist) took three
  distinct guards — the full story is
  [gotcha #2 in osc133.md](osc133.md).

## Windows: same sandwich, different bread

On Windows the PTY is a [ConPTY](https://learn.microsoft.com/en-us/windows/console/pseudoconsoles)
pseudoconsole: two anonymous pipes plus `CreatePseudoConsole`, and the
shell is spawned with `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` in its
proc-thread attribute list. The byte-stream contract is the same — the
pseudoconsole renders the child's console API calls into VT sequences,
and OSC marks pass through untouched — so the parser, store, and echo
reconstruction are shared verbatim. Two hard-won specifics:

- **`STARTF_USESTDHANDLES` (with NULL std handles) is mandatory** when
  the parent itself has a console. Without it the child attaches its std
  handles to the *parent's* console: output leaks to the outer console,
  the ConPTY paints an empty frame, and input written to the ConPTY
  never reaches the shell. The official ConPTY sample omits the flag
  only because its parent has no console to leak to.
- **Teardown is inverted.** A Unix PTY read returns EIO once the shell
  exits; a ConPTY read blocks forever until `ClosePseudoConsole` runs.
  So a waiter goroutine watches the process handle and closes the
  pseudoconsole, which flushes pending output and *then* breaks the read
  loop — the same flush-through-the-normal-path guarantee as the Unix
  SIGHUP dance, driven from the opposite end. (There is no SIGWINCH
  either; resize is a 500 ms poll, and no SIGHUP grace: closing the
  console window kills the tree.)

One PowerShell trap that isn't ConPTY's fault: inside a PSReadLine key
handler, PowerShell pipes a native command's **stderr** instead of
handing the child the terminal. Launch a TUI like fzf with a plain
`& backscroll pick` there and its interface — drawn on stderr — is
swallowed while its keystrokes still land: to the user it looks like
the shell hung. The `init pwsh` picker binding therefore starts the
process via `System.Diagnostics.Process` with *only stdout* redirected,
so the UI stays on the real terminal and only the selection is
captured.

## What it costs

Measured on a 2-vCPU VM (methodology and harnesses in
`internal/record/bench_test.go`; README has the full table):

- keystroke echo: **+0.05 ms** median vs a bare shell;
- bulk `cat`: ~31 MB/s through the recorder vs ~56 MB/s bare — both far
  above what any terminal emulator renders;
- parser: ~680 MB/s single-core; capture buffer: memcpy speed;
- disk: a typical day of interactive work adds a few MB.

## How it's tested

- **Unit + property tests** for parser (chunking-invariance fuzz),
  buffer (randomized against naive model), store (roundtrip, FTS,
  migrations), redaction.
- **Live-shell integration tests**: pexpect drives real bash/zsh/fish
  sessions on a PTY through the actual init snippets — widget scenarios,
  Ctrl-C at a prompt vs mid-command, heredocs, exit codes. CI runs the
  bash matrix against a pinned-from-source bash 3.2.57 build on Linux
  *and* `/bin/bash` on a macOS runner (which is 3.2.57, covering BSD
  `stty` output too). Zero-config emitters get the same treatment:
  pinned real fish 4, nushell, VS Code / kitty / WezTerm / Ghostty /
  iTerm2 integration scripts, each driven end-to-end through
  `backscroll run` (the nushell suite doubles as a stress test of echo
  reconstruction — reedline repaints the whole prompt row on every
  keystroke).

The pattern behind most fixes in the changelog: the protocol is the easy
80%, and every remaining bug lived in a shell/terminal/version-specific
corner that only a real PTY with a real shell could reach.
