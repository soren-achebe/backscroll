#!/usr/bin/env python3
r"""End-to-end test: recording under iTerm2 shell integration.

iTerm2's shell-integration scripts (installed into rc files by the app's
"Install Shell Integration" menu, so they are active inside tmux/SSH and
under `backscroll run`) emit plain OSC 133 with per-command `aid=` params
plus iTerm2-specific metadata. Peculiarities worth pinning, all observed
against the REAL scripts (see ci.yml for the sha-pinned fetch):

  133;D;<exit>;aid=<salt>-<n>   exit code with trailing params; embedded
                                in PS1 itself (bash), so a D arrives on
                                every prompt render
  133;A;k=s;aid=...             PS2 continuation prompts are wrapped in
                                A;k=s (where kitty uses P;k=s) — the B
                                that follows must CONTINUE the current
                                input region or multiline commands lose
                                everything before the last PS2 line
  133;C;aid=...\r               a literal \r INSIDE the OSC body before
                                the terminator when TERM_PROGRAM is
                                iTerm.app (both shells)
  1337;CurrentDir=<raw path>    cwd as a plain path — iTerm2 emits no
                                OSC 7; this is the only cwd source
  1337;RemoteHost=user@host     stateful host metadata (drives iTerm2's
                                profile switching) — must not be stored
                                or it would be replayed by `show --raw`
  stray preexec C at source time (bash): the bundled preexec machinery
    fires once while the rc file is still loading, before any A/B or
    typed input; the first prompt's D;0 closes it. Must not be stored.

Scenarios, driven through the real `backscroll run` binary on a PTY:

  1. iTerm2-only (no snippet): command text reconstructed from the input
     echo (incl. multiline across a PS2 continuation and a >1-row wrap),
     exit codes (incl. Ctrl-C 130), cwd from CurrentDir, no startup
     phantom, no stub for empty Enter / Ctrl-C at prompt / final exit
  2. iTerm2 + our snippet (double emission): text stored exactly once,
     exit codes right
  3. no integration metadata (133/1337) inside stored output

Env:
  BKS_BIN               path to a built backscroll binary (default ./backscroll)
  BKS_TEST_ITERM2_BASH  path to iTerm2's bash integration (skipped if unset)
  BKS_TEST_ITERM2_ZSH   path to iTerm2's zsh integration  (skipped if unset)
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
ITERM2_BASH = os.environ.get("BKS_TEST_ITERM2_BASH", "")
ITERM2_ZSH = os.environ.get("BKS_TEST_ITERM2_ZSH", "")

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
               # exercises the \r-inside-OSC-body variant of the C mark
               TERM_PROGRAM="iTerm.app")
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
        lines = ['PS1="\\$ "', f'source "{ITERM2_BASH}"']
    else:
        lines = ['PS1="$ "', f'source "{ITERM2_ZSH}"']
    if snippet:
        lines.append(f'eval "$({BKS} init {shell})"')
    rc = ".bashrc" if shell == "bash" else ".zshrc"
    with open(os.path.join(home, rc), "w") as f:
        f.write("\n".join(lines) + "\n")


LONG_CMD = "echo " + "y" * 120     # wraps past one row on a 120-col PTY
ML_CMD = "echo 'line1\nline2'"     # crosses a PS2 continuation prompt
WANT = ['echo "hi there"', "false", ML_CMD, "cd /tmp && pwd",
        "true", LONG_CMD, "sleep 30"]
WANT_EXITS = [0, 1, 0, 0, 0, 0, 130]


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
    child.sendline("")        # empty Enter: D-in-PS1 re-renders -> no entry
    child.expect_exact("$ ")
    child.sendline("echo 'line1")
    child.expect(r"> ")       # bash "> ", zsh "quote> " — both A;k=s-wrapped
    child.sendline("line2'")
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
    child.send("\x03")        # Ctrl-C at empty prompt -> no entry
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

    # --- iTerm2-only: no backscroll snippet ---
    tag = f"iterm2/{shell}"
    rs, env = run_session(f"/tmp/bks-iterm2-{shell}", shell, snippet=False)
    check(f"[{tag}] one entry per real command (no startup phantom/stubs)",
          len(rs) == len(WANT),
          f"got {len(rs)} entries: {[(r['cmd'], r['exit_code']) for r in rs]}")
    if len(rs) == len(WANT):
        exits = [r["exit_code"] for r in rs]
        check(f"[{tag}] exit codes from D;<exit>;aid= incl. Ctrl-C 130",
              exits == WANT_EXITS, f"got {exits}, want {WANT_EXITS}")
        check(f"[{tag}] command text reconstructed from input echo "
              "(incl. A;k=s multiline + wrapped line)",
              [r["cmd"] for r in rs] == WANT,
              f"got {[r['cmd'] for r in rs]}")
        # CurrentDir is emitted at precmd: bash prints it before the D (cd
        # entry shows the new dir), zsh after (new dir applies from the next
        # entry). Both must end at /tmp.
        check(f"[{tag}] cwd tracked via 1337;CurrentDir",
              rs[0]["cwd"].startswith("/tmp/bks-iterm2-") and
              rs[-1]["cwd"] == "/tmp",
              f"got first={rs[0]['cwd']!r} last={rs[-1]['cwd']!r}")
    body = outputs(f"/tmp/bks-iterm2-{shell}", env, [r["id"] for r in rs])
    check(f"[{tag}] output text recorded and searchable",
          "hi there" in body and "line1" in body, "missing output in export")
    check(f"[{tag}] no integration metadata in stored output",
          "]133;" not in body and "]1337;" not in body and
          "RemoteHost" not in body,
          "found 133/1337 bytes in export")

    # --- iTerm2 + our snippet: double emission ---
    tag = f"iterm2/{shell}+snippet"
    rs, env = run_session(f"/tmp/bks-iterm2-{shell}-snip", shell, snippet=True)
    got = [r["cmd"] for r in rs]
    check(f"[{tag}] command text recorded once each",
          got == WANT, f"got {got}")
    if len(rs) == len(WANT):
        exits = [r["exit_code"] for r in rs]
        check(f"[{tag}] exit codes incl. Ctrl-C 130",
              exits == WANT_EXITS, f"got {exits}")
    body = outputs(f"/tmp/bks-iterm2-{shell}-snip", env, [r["id"] for r in rs])
    check(f"[{tag}] no integration metadata in stored output",
          "]133;" not in body and "]1337;" not in body,
          "found 133/1337 bytes in export")


if ITERM2_BASH and os.path.exists(ITERM2_BASH):
    scenario("bash")
else:
    print("SKIP iterm2/bash (BKS_TEST_ITERM2_BASH unset)")

if ITERM2_ZSH and os.path.exists(ITERM2_ZSH) and shutil.which("zsh"):
    scenario("zsh")
else:
    print("SKIP iterm2/zsh (BKS_TEST_ITERM2_ZSH unset)")

print()
if RAN == 0:
    print("SKIP: no iTerm2 scripts configured")
    sys.exit(0)
if FAILURES:
    print(f"{len(FAILURES)} FAILED: {FAILURES}")
    sys.exit(1)
print("all iTerm2 integration checks passed")
