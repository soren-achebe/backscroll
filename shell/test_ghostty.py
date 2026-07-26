#!/usr/bin/env python3
r"""End-to-end test: recording under Ghostty shell integration.

Ghostty AUTO-INJECTS its shell integration into bash/zsh/fish sessions
(and its docs recommend manual rc-file sourcing for `exec zsh`/tmux/sudo/
SSH setups), so every Ghostty user who runs backscroll gets a session
where Ghostty's marks and ours interleave. Ghostty emits plain OSC 133
(no cmdline= parameter, unlike kitty), plus variants worth pinning:

  133;P;k=i / k=s          prompt-kind marks (must be ignored)
  133;A;redraw=last;cl=line;aid=<pid>
  133;C;                   trailing semicolon, no params
  133;D;<exit>;aid=<pid>   exit code with trailing params
  133;D                    bare, no exit (zsh edge paths)
  OSC 7 kitty-shell-cwd://host/path
  OSC 2 window titles (zsh writes via a dup'd tty fd, not stdout)

Spurious-D quirk (bash): __ghostty_precmd emits a D mark on every
PROMPT_COMMAND re-fire once a first command has run (its _ghostty_executing
flag is "0", which != ""), e.g. Ctrl-C at an empty prompt or plain Enter.
The parser must drop D marks when no C is open or phantom entries appear.

This drives the real `backscroll run` binary on a PTY against the REAL
pinned Ghostty scripts (see ci.yml) and asserts on the database:

  1. ghostty-only (no snippet): outputs, exit codes (incl. 130), and
     OSC 7 cwd recorded; command text reconstructed from the input echo
     (Ghostty provides no cmdline — B..C echo is the only trace of it)
  2. ghostty + our snippet (the double-emission case every Ghostty user
     actually hits): command text correct, stored exactly once, exit
     codes right, no phantom entries from spurious D marks
  3. no integration metadata (133/OSC 2 titles) inside stored output
  4. Ctrl-C at an empty prompt stores nothing; final `exit` leaves no stub

Env:
  BKS_BIN                path to a built backscroll binary (default ./backscroll)
  BKS_TEST_GHOSTTY_BASH  path to ghostty.bash            (skipped if unset)
  BKS_TEST_GHOSTTY_ZSH   path to zsh ghostty-integration (skipped if unset)
"""
import os
import shutil
import sqlite3
import subprocess
import sys
import time

try:
    import pexpect
except ImportError:
    print("SKIP: pexpect not installed")
    sys.exit(0)

HERE = os.path.dirname(os.path.abspath(__file__))
BKS = os.path.abspath(os.environ.get("BKS_BIN", os.path.join(HERE, "..", "backscroll")))
GHOSTTY_BASH = os.environ.get("BKS_TEST_GHOSTTY_BASH", "")
GHOSTTY_ZSH = os.environ.get("BKS_TEST_GHOSTTY_ZSH", "")

if not os.path.exists(BKS):
    print(f"SKIP: backscroll binary not found at {BKS} (set BKS_BIN)")
    sys.exit(0)

FAILURES = []
RAN = 0


def check(name, cond, detail=""):
    if cond:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name}  {detail}")
        FAILURES.append(name)


def session_env(home, shell):
    env = dict(os.environ, HOME=home, TERM="xterm-256color", SHELL=shell,
               ZDOTDIR=home,
               # exercise the OSC 2 title path (metadata-leak check) and
               # the cursor-shape escapes Ghostty appends to PS1/PS0
               GHOSTTY_SHELL_FEATURES="cursor,title")
    for k in list(env):
        if (k.startswith("BACKSCROLL_") or k.startswith("XDG_")
                or k == "TMUX"):
            del env[k]
    return env


def make_zsh_wrapper(home):
    # CI runner images ship global zsh rc files (compinit prompts stall the
    # PTY). `backscroll run` spawns $SHELL bare, so wrap zsh.
    path = os.path.join(home, "zsh-wrapper")
    with open(path, "w") as f:
        f.write('#!/bin/sh\nexec zsh --no-globalrcs "$@"\n')
    os.chmod(path, 0o755)
    return path


def write_rc(home, shell, snippet):
    if shell == "bash":
        lines = ['PS1="$ "', f'source "{GHOSTTY_BASH}"']
    else:
        # the documented manual-install path from the script's own header
        lines = ['PS1="$ "', f'source "{GHOSTTY_ZSH}"']
    if snippet:
        lines.append(f'eval "$({BKS} init {shell})"')
    rc = ".bashrc" if shell == "bash" else ".zshrc"
    with open(os.path.join(home, rc), "w") as f:
        f.write("\n".join(lines) + "\n")


LONG_CMD = "echo " + "x" * 120
WANT = ['echo "hi there"', "false", "echo one; echo two",
        "cd /tmp && pwd", "true", LONG_CMD, "sleep 30"]
WANT_EXITS = [0, 1, 0, 0, 0, 0, 130]

# UPSTREAM BUG (ghostty >= 1.3.0, bash >= 4.4 path): __ghostty_hook captures
# $? into a local and passes it as $1, but __ghostty_precmd ignores $1 and
# re-reads $? — which the intervening `builtin local` (a successful command)
# has reset to 0. Every bash 133;D mark therefore reports exit 0. Verified
# empirically against the pinned script on bash 5.2 (`false` and
# `sh -c 'exit 42'` both emit 133;D;0); one-line fix `ret="${1-$?}"`
# verified too. Documented in docs/osc133.md gotcha 15. NOT filed upstream:
# Ghostty's AI policy requires a human-in-the-loop reviewing any
# AI-assisted issue before submission, which this project (maintained by
# an AI agent) cannot honestly provide — so we document instead of filing.
# Pinned here so the suite tracks the pinned script's actual behavior; when
# the pin is bumped past an upstream fix, flip this back to WANT_EXITS.
# The <4.4 bash-preexec path and zsh are unaffected; with our snippet
# installed our own D mark wins, so exits are correct there regardless.
WANT_EXITS_GHOSTTY_BASH_ALONE = [0, 0, 0, 0, 0, 0, 0]


def run_session(home, shell, snippet):
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home)
    write_rc(home, shell, snippet)
    spawn_shell = shell if shell == "bash" else make_zsh_wrapper(home)
    env = session_env(home, spawn_shell)

    child = pexpect.spawn(BKS, ["run"], env=env, timeout=15,
                          dimensions=(24, 120), cwd=home)
    child.expect_exact("$ ")
    child.sendline('echo "hi there"')
    child.expect_exact("$ ")
    child.sendline("false")
    child.expect_exact("$ ")
    child.sendline("")        # empty Enter -> ghostty emits a spurious D
    child.expect_exact("$ ")
    child.sendline("echo one; echo two")
    child.expect_exact("$ ")
    child.sendline("cd /tmp && pwd")
    child.expect_exact("$ ")
    child.sendline("true")
    child.expect_exact("$ ")
    child.sendline(LONG_CMD)
    child.expect_exact("$ ")
    child.sendline("sleep 30")
    time.sleep(0.8)
    child.send("\x03")        # Ctrl-C mid-command -> 130
    child.expect_exact("$ ")
    child.send("\x03")        # Ctrl-C at empty prompt -> spurious D, no entry
    child.expect_exact("$ ")
    child.sendline("exit")
    child.expect(pexpect.EOF)
    child.wait()
    time.sleep(0.3)
    return rows(home), env


def rows(home):
    db = os.path.join(home, ".local", "share", "backscroll", "backscroll.db")
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    out = [dict(r) for r in con.execute(
        "SELECT id, cmd, cwd, exit_code FROM commands ORDER BY id")]
    con.close()
    return out


def outputs(home, env, ids):
    r = subprocess.run([BKS, "export", "--format", "json"] + [str(i) for i in ids],
                       env=env, capture_output=True, text=True)
    return r.stdout


def scenario(shell):
    global RAN
    RAN += 1

    # --- ghostty-only: no backscroll snippet (degraded but useful) ---
    tag = f"ghostty/{shell}"
    rs, env = run_session(f"/tmp/bks-ghostty-{shell}", shell, snippet=False)
    exits = [r["exit_code"] for r in rs]
    check(f"[{tag}] one entry per real command (no phantoms/stubs)",
          len(rs) == len(WANT_EXITS),
          f"got {len(rs)} entries: {[(r['cmd'], r['exit_code']) for r in rs]}")
    want_exits = (WANT_EXITS_GHOSTTY_BASH_ALONE if shell == "bash"
                  else WANT_EXITS)
    if len(rs) == len(WANT_EXITS):
        check(f"[{tag}] exit codes as emitted by ghostty's D marks",
              exits == want_exits, f"got {exits}, want {want_exits}")
        check(f"[{tag}] cwd tracking via OSC 7 kitty-shell-cwd",
              rs[5]["cwd"] == "/tmp", f"got {rs[5]['cwd']!r}")
        # Ghostty never reports the command line, but its PS1/PS2 carry
        # proper B marks (and P;k=s on continuations) — so the recorder
        # reconstructs the text from the input echo. Real text, no
        # snippet installed.
        check(f"[{tag}] command text reconstructed from input echo",
              [r["cmd"] for r in rs] == WANT,
              f"got {[r['cmd'] for r in rs]}")
    body = outputs(f"/tmp/bks-ghostty-{shell}", env, [r["id"] for r in rs])
    check(f"[{tag}] output text recorded and searchable",
          "hi there" in body, "missing 'hi there' in export")
    check(f"[{tag}] no integration metadata in stored output",
          "]133;" not in body and "]2;" not in body and "]7;" not in body,
          "found 133/title/OSC7 bytes in export")

    # --- ghostty + our snippet: what every Ghostty user actually runs ---
    tag = f"ghostty/{shell}+snippet"
    rs, env = run_session(f"/tmp/bks-ghostty-{shell}-snip", shell, snippet=True)
    got = [r["cmd"] for r in rs]
    check(f"[{tag}] command text recorded once each",
          got == WANT, f"got {got}")
    if len(rs) == len(WANT):
        exits = [r["exit_code"] for r in rs]
        check(f"[{tag}] exit codes incl. Ctrl-C 130",
              exits == WANT_EXITS, f"got {exits}")
    body = outputs(f"/tmp/bks-ghostty-{shell}-snip", env, [r["id"] for r in rs])
    check(f"[{tag}] no integration metadata in stored output",
          "]133;" not in body and "]2;" not in body and "]7;" not in body,
          "found 133/title/OSC7 bytes in export")


if GHOSTTY_BASH and os.path.exists(GHOSTTY_BASH):
    scenario("bash")
else:
    print("SKIP ghostty/bash (BKS_TEST_GHOSTTY_BASH unset)")

if GHOSTTY_ZSH and os.path.exists(GHOSTTY_ZSH) and shutil.which("zsh"):
    scenario("zsh")
else:
    print("SKIP ghostty/zsh (BKS_TEST_GHOSTTY_ZSH unset)")

print()
if RAN == 0:
    print("SKIP: no ghostty scripts configured")
    sys.exit(0)
if FAILURES:
    print(f"{len(FAILURES)} FAILED: {FAILURES}")
    sys.exit(1)
print("all ghostty integration checks passed")
