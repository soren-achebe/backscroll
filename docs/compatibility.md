---
description: >-
  Terminals, shells, prompt frameworks, and multiplexers backscroll is
  tested against — including what works with zero configuration.
---

# Compatibility

backscroll is a PTY-level recorder: it sits between your terminal and
your shell and passes every byte through untouched. That makes it mostly
tool-agnostic by construction — but "mostly" isn't a guarantee, so CI
drives **real recorded sessions against pinned real versions** of the
tools below and asserts commands, outputs, and exit codes are recorded
correctly *and* the other tool keeps working.

## Terminals — zero-config

If your terminal's shell integration already emits command marks
([OSC 133](osc133.md) or a vendor dialect), `backscroll run` records
correctly with **no rc-file setup at all**:

| terminal / integration | how command text is captured |
|---|---|
| fish ≥ 4.0 (any terminal) | native OSC 133 `cmdline_url` |
| nushell (any terminal) | OSC 133 + [echo reconstruction](how-it-records.md) |
| VS Code terminal | OSC 633 `E;<cmdline>` |
| kitty | OSC 133 `C;cmdline=%q` |
| WezTerm | OSC 1337 `SetUserVar=WEZTERM_PROG` |
| Ghostty | OSC 133 + echo reconstruction |
| iTerm2 | OSC 133 + echo reconstruction |

Everywhere else, one line in your shell rc (`eval "$(backscroll init
bash)"`, zsh/fish/pwsh equivalents) enables exact command capture; without
it you still get outputs, cwd, and timing.

## Shells

| shell | recording | command text | picker binding | history import |
|---|---|---|---|---|
| bash ≥ 4 | ✓ | init snippet | ++ctrl+x++ ++ctrl+p++ | ✓ |
| bash 3.2 (macOS `/bin/bash`) | ✓ | init snippet | skipped (readline limits) | ✓ |
| zsh | ✓ | init snippet | ✓ | ✓ (incl. `EXTENDED_HISTORY`, metafied bytes) |
| fish 3.x | ✓ | init snippet | ✓ | ✓ |
| fish ≥ 4.0 | ✓ | **zero-config** | ✓ | ✓ |
| nushell | ✓ | **zero-config** | — | ✓ (sqlite + plain) |
| PowerShell 7+ (all platforms) | ✓ (ConPTY on Windows) | init snippet | ✓ | ✓ (PSReadLine) |
| Windows PowerShell 5.1 | ✓ | init snippet | ✓ | ✓ |

`backscroll import atuin|zsh|bash|fish|nu|pwsh` seeds the database from
your existing history files — formats pinned against real-tool fixtures
([field guide](history-files.md)).

## Prompt frameworks & shell tools

From [`shell/test_compat_matrix.py`](https://github.com/soren-achebe/backscroll/blob/main/shell/test_compat_matrix.py):

| tested with | bash | zsh | fish |
|---|---|---|---|
| [atuin](https://github.com/atuinsh/atuin) (incl. its Ctrl-R TUI) | ✓ | ✓ | ✓ |
| [starship](https://github.com/starship/starship) (both load orders) | ✓ | ✓ | ✓ |
| [zoxide](https://github.com/ajeetdsouza/zoxide) | ✓ | — | ✓ |
| [direnv](https://github.com/direnv/direnv) | ✓ | — | — |
| [oh-my-zsh](https://github.com/ohmyzsh/ohmyzsh) | | ✓ | |
| [powerlevel10k](https://github.com/romkatv/powerlevel10k) (incl. instant prompt) | | ✓ | |
| [bash-preexec](https://github.com/rcaloras/bash-preexec) (both load orders) | ✓ | | |
| `bind -x` / zle widgets (fzf-style) | ✓ | ✓ | ✓ |

One cross-tool finding worth knowing even if you don't use backscroll:
with starship ≤ 1.26 on bash, anything reading `$?` from a
starship-wrapped `PROMPT_COMMAND` sees `0` instead of the real exit
status. backscroll sidesteps it by capturing the true exit in its DEBUG
trap before prompt frameworks run.

## Multiplexers

Recording works inside tmux, zellij, and GNU screen (the recorder is just
another program on a PTY). Each also gets picker keybindings:

| | history picker | failures picker | insert-at-prompt |
|---|---|---|---|
| tmux (`init tmux`) | prefix + B (popup) | prefix + F | — |
| zellij (`init zellij`) | Alt b (floating) | Alt Shift b | Alt r |
| screen (`init screen`) | C-a B | C-a F | — |

SSH works in both directions: record locally around `ssh` (outputs land in
your local DB), or install backscroll on the remote and
[sync](sync-design.md) the encrypted logs back.

## Operating systems

- **Linux** (amd64/arm64): static binaries, `.deb`/`.rpm`, install.sh
- **macOS** (Intel/Apple Silicon): Homebrew tap, install.sh
- **Windows** 10 1809+: native ConPTY recorder, Scoop bucket

## Something broken?

`backscroll doctor` prints an environment diagnosis (terminal, shell,
integration state, DB health). Please paste its output into
[bug reports](https://github.com/soren-achebe/backscroll/issues) —
see [CONTRIBUTING](https://github.com/soren-achebe/backscroll/blob/main/CONTRIBUTING.md).
