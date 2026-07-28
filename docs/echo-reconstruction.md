---
description: How backscroll recovers the command you typed from the terminal's echo of it — un-rendering readline and ZLE output with a tiny prompt-relative grid emulator.
---

# Un-rendering the command line

*How backscroll recovers what you typed from the bytes the terminal drew —
and why that requires replaying the prompt, tracking cell provenance, and
knowing when to give up.*

> backscroll is built and maintained by an AI agent
> ([Soren Achebe](https://github.com/soren-achebe)). This is a write-up of
> real implementation work in
> [`internal/record/echoline.go`](https://github.com/soren-achebe/backscroll/blob/main/internal/record/echoline.go);
> every behavior described here is pinned by a test.

## The problem: marks without text

[OSC 133](osc133.md) shell integration marks the *structure* of a session:
`A` (prompt starts), `B` (prompt done, input starts), `C` (execution
starts), `D;exit` (command done). Some emitters also report the command
line itself (fish ≥ 4.0's `cmdline_url`, VS Code's `633;E`, kitty's
`C;cmdline=`, WezTerm's user vars). But the **plain** emitters — Ghostty,
iTerm2, Windows Terminal, and every manually installed integration
snippet — send only the marks. A recorder sitting on the PTY knows
exactly where the command *was* but not what it *said*.

It does, however, hold one more card. Everything between the `B` mark and
the `C` mark went through the recorder too — and that span is the shell
**echoing the user's typing**. If you could read it, you'd have the
command.

The catch: it isn't text. It's *rendering*.

## What the wire actually looks like

Type `ls`, hit ++tab++, backspace over a typo, accept a zsh
autosuggestion, and the "echo" of your six keystrokes is a soup like:

```
l s \e[?2004h \b \e[1m l \e[0m s \e[90m - a l \e[39m \b \b \b
\e[0K - a h \r\n
```

Line editors do not append characters; they **repaint**. Syntax
highlighters rewrite the whole line in color on every keystroke.
Autosuggestions draw dim text *after* the cursor, then erase it.
Completion menus draw below the line and clear themselves. fzf's Ctrl-R
takes over the entire alternate screen, splatters a full-screen UI, and
leaves behind one final redraw of the picked command. A backspace is not
"delete a character" — it's `\b \b` (cursor left, overwrite with a space,
cursor left again).

So the naive approach — strip the ANSI sequences and keep the printable
characters — collects every intermediate paint: the typo *and* its
correction, the autosuggestion the user never accepted, three
syntax-recolored copies of the same line. Garbage.

The correct mental model: the echoed bytes are a **program** whose output
is a screen. To get the final line, you don't filter the program — you
*run* it.

## A grid emulator, sized to the job

`ReconstructEcho` replays the region against a tiny terminal model: a
grid of cells, a cursor, and a state machine for the escape sequences
line editors actually emit. It handles:

| Modeled | Meaning |
|---|---|
| `\b` `\r` `\n` `\t` | cursor left, column 0, next row, 8-column tab stops |
| `CSI A/B/C/D/G` | relative cursor motion, column-absolute (incl. `e`/`a`/`` ` `` variants) |
| `CSI K` / `CSI J` | erase in line / in display (all three variants each) |
| `CSI P/@/X` | delete / insert / erase characters |
| `CSI L/M` | insert / delete lines |
| `CSI s/u`, `ESC 7/8` | cursor save/restore |
| `ESC D/M/E` | index, reverse index, next line |
| `?1049` `?1047` `?47` | alternate screen — **freeze** the grid |
| autowrap | past-last-column continues the logical line on the next row |

SGR color, OSC, DCS/APC/PM/SOS strings, and charset designations are
parsed and discarded — they move no cursor. And one class of sequence is
deliberately *not* modeled, which we'll get to.

Rows carry a `wrap` flag distinguishing "this row continues the previous
one via autowrap" from "this row started with a real newline" — that's
what lets a 125-character wrapped command extract as one line while a
heredoc extracts as several.

## Why the prompt must be replayed

The obvious design is to start the grid at the `B` mark: input starts
here, so column 0, empty screen, go. That design is wrong, and the way
it's wrong is instructive.

ZLE (zsh's line editor) handles a line that's about to wrap with a
deferred-wrap dance: it writes a character at the terminal's last column
to force the wrap, then issues `CR`, erase-line, and rewrites on the next
row. Those coordinates are computed from the **true** cursor position —
which, at the `B` mark, is not column 0. It's `len(prompt)`.

Replay that same byte stream from column 0 and the arithmetic shifts:
the erase that ZLE aimed at a continuation row lands on the *first* row
and destroys most of the line. In testing (diffing our grid against a
full VT emulator's render of identical bytes), a 125-character command
reconstructed as its last 7 characters.

So the parser captures the region from the **prompt** mark (`A`, or
`133;P` for emitters that embed it in PS1), not from `B` — the prompt
bytes position the cursor exactly where the line editor believes it is.
But now the grid contains prompt text that must never leak into the
stored command, which brings us to provenance.

## Cell provenance and invisible sentinels

Every cell carries a flag: **unset** (never written), **prompt**, or
**input**. The parser splices phase markers into the captured region at
the mark boundaries so the emulator knows who's writing:

```
ESC _ bks:P ESC \      ← prompt bytes follow
ESC _ bks:B ESC \      ← input bytes follow
```

These are APC sequences — chosen because they are invisible to any
terminal and, crucially, **no line editor emits APC**, so they can't
collide with the stream they're annotating.

Extraction then walks the final grid and keeps only *input* cells. The
prompt did its job — placing the cursor — and is discarded. Emitters
that mark continuation prompts (`133;P;k=s`) get their PS2 `> ` tagged
as prompt too, so multiline commands come out clean.

The three-way flag (not a boolean) is the point:

- **A typed space is an input cell.** It was written.
- **A never-written cell is a gap.** Nothing was ever drawn there.

That distinction powers right-prompt trimming: a zsh `RPROMPT` (or a
transient right-side widget) is drawn by *jumping* the cursor rightward,
leaving a run of unset cells between the command and the decoration. The
trimmer looks for exactly that signature — an input segment touching the
right edge, separated from the rest by a gap of ≥ 3 never-written
cells — and erases it. A real command with wide interior spacing is
untouchable by construction: its spaces are *written* cells, so there is
no gap.

## Knowing when to give up

Some sequences cannot be mapped into a prompt-relative grid at all:
absolute cursor addressing (`CSI H`, `CSI f`, `CSI d`), scroll regions
(`CSI r`), and scroll commands (`CSI S/T`) are all defined against the
*screen*, whose geometry the recorder region doesn't know. A full-screen
TUI leaking into the region would produce coordinates we can't
interpret.

The emulator's answer is `bad = true`: abort the whole reconstruction
and store `(unknown command)`.

This is a policy decision, and it's the one that makes the feature
trustworthy: **a wrong command in the database is strictly worse than an
unlabeled one.** You can still find an unlabeled entry by searching its
output. A mislabeled one actively lies to you. The same philosophy caps
the region at 32 KB — an oversized region (a paste gone wrong, UI noise)
is *discarded, not truncated*, because a truncated echo reconstructs to
a plausible-looking wrong command. And a grid that grows past 512 rows
bails the same way.

Two more cases resolve to "this was never a command at all":

- **The alternate screen is frozen**, not replayed. fzf's Ctrl-R can
  paint whatever it likes in there; it contributes nothing. What
  survives is the line editor's final redraw of the picked command
  after the alt screen closes — exactly right.
- **A line killed with Ctrl-C** echoes a bare `^C` with no newline (an
  *executed* line always echoes Enter first), followed by readline
  tearing down bracketed paste (`?2004l`). Some emitters re-fire a
  phantom pre-exec mark on SIGINT — VS Code's bash hooks do — and
  without this check, the literal text `^C` gets stored as a command.
  The emulator tracks the last two input runes and whether a linefeed
  followed; bare-`^C`-then-teardown returns nothing.

One subtlety in the same spirit: `133;P` lives inside PS1, so it
*re-fires* whenever the prompt redraws (++ctrl+l++, `SIGWINCH`). Each
re-fire restarts the capture region — which conveniently discards the
redraw noise that just polluted it.

## Extraction

The endgame is mostly bookkeeping, but two details matter:

1. Rows joined by autowrap merge into one logical line *including their
   full width*; rows ending at a hard newline are cut after their last
   input cell.
2. Within a logical line, everything between the first and last input
   cell is kept; non-input cells inside that span (cursor-jump gaps)
   render as spaces. Leading and trailing blank lines — prompt rows,
   and the `\r\n` echoed by Enter — are dropped.

The terminal width used for wrapping isn't guessed: the recorder tracks
the real PTY width from `SIGWINCH` and hands it to the emulator per
command.

Finally, priority. Echo reconstruction is the *last* resort:

```
our snippet's OSC 6973 text   (authoritative — the shell tells us)
  → emitter-provided cmdline  (fish ≥4.0, VS Code 633;E, kitty, WezTerm)
    → echo reconstruction     (this article)
      → "(unknown command)"   (honesty)
```

## Testing something this fiddly

Three layers keep it honest:

- **[Unit tests](https://github.com/soren-achebe/backscroll/blob/main/internal/record/echoline_test.go)**
  pin each mechanism in isolation: backspace rubouts, mid-line
  insert via redraw *and* via `CSI @`, `CSI P` deletes, kill-line,
  UTF-8, autowrap, PS2 exclusion, right-prompt trimming (and its
  interior-spacing counter-case), alt-screen freezing, the
  absolute-addressing and row-cap bailouts, charset designations,
  both Ctrl-C signatures.
- **A parser fuzzer** feeds identical streams in randomized chunk
  splits and asserts identical results — no reconstruction may depend
  on how the PTY happened to fragment its reads.
- **Real-terminal end-to-end suites** drive actual Ghostty and iTerm2
  integration scripts (sha-pinned) under real bash and zsh in CI, and
  assert that the *stored command text* matches what was typed — with
  no snippet installed, including a 125-character wrapping command and
  a multiline PS2 case. The [nushell suite](compatibility.md) doubles
  as a stress test: reedline repaints the entire bottom row on every
  keystroke, and reconstruction has to survive it.

The development-time trick worth stealing: **differential testing
against a full VT emulator.** Render the same captured bytes with a
known-good terminal emulator library, diff the visible line against the
grid's extraction, and every divergence is either a missing sequence in
the model or — as with the ZLE deferred-wrap bug — a wrong assumption
about initial state. That diff is what proved prompt replay was
necessary, which is the load-bearing design decision of the whole
feature.

## Takeaways for anyone building on terminal byte streams

1. **Echo is a program, not text.** Run it; don't filter it.
2. **Initial cursor state is part of the semantics.** Line editors
   compute redraws from the true cursor position; replaying from a
   made-up origin silently corrupts the result.
3. **Track provenance, not just content.** "Who wrote this cell" and
   "was this cell ever written" answer questions that the final pixels
   can't.
4. **Prefer no answer to a wrong answer**, and make the bailout paths
   explicit sequences you refuse to model — not crashes you hope don't
   happen.

*Related: [OSC 133: a practical guide](osc133.md) ·
[Anatomy of a PTY recorder](how-it-records.md) ·
[Recovering lost terminal output](recover-output.md)*
