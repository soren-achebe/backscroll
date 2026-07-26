#!/usr/bin/env python3
r"""End-to-end test: zero-config recording under kitty / WezTerm shell
integration (no backscroll snippet installed).

Both terminals' shell-integration scripts are commonly installed *manually*
in rc files (kitty docs recommend it for `exec zsh`/tmux/sudo/SSH setups;
wezterm.sh is distributed for the same purpose), so a shell inside
`backscroll run` can be emitting their marks with no backscroll snippet:

  kitty (bash + zsh):  OSC 133;C;cmdline=%q   — command line shell-quoted
                       OSC 133;D;<exit>, OSC 7 kitty-shell-cwd://host/path
  wezterm (bash+zsh):  OSC 133;C; then OSC 1337;SetUserVar=WEZTERM_PROG=
                       <base64, wrapped at 76 cols>, OSC 133;D;<exit>;aid=,
                       OSC 7 file://host/path

This drives the real `backscroll run` binary on a PTY against the REAL
pinned integration scripts (see ci.yml) and asserts on the database:

  1. command text recorded (quotes, semicolons intact)
  2. exit codes right (incl. 130 for Ctrl-C mid-command)
  3. cwd tracking follows OSC 7 (both schemes)
  4. Ctrl-C at an empty prompt stores nothing; final `exit` leaves no stub
  5. no 133/1337 integration metadata inside stored output
  6. with our snippet TOO (double emission), entries stored once,
     snippet text authoritative

Env:
  BKS_BIN               path to a built backscroll binary (default ./backscroll)
  BKS_TEST_KITTY_BASH   path to kitty.bash            (scenario skipped if unset)
  BKS_TEST_KITTY_ZSH    path to zsh kitty-integration (scenario skipped if unset)
  BKS_TEST_WEZTERM_SH   path to wezterm.sh            (scenarios skipped if unset)
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
KITTY_BASH = os.environ.get("BKS_TEST_KITTY_BASH", "")
KITTY_ZSH = os.environ.get("BKS_TEST_KITTY_ZSH", "")
WEZTERM_SH = os.environ.get("BKS_TEST_WEZTERM_SH", "")

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
               ZDOTDIR=home)
    for k in list(env):
        if (k.startswith("BACKSCROLL_") or k.startswith("XDG_")
                or k.startswith("KITTY_") or k.startswith("WEZTERM_")
                or k == "TMUX"):
            del env[k]
    return env


def make_zsh_wrapper(home):
    # CI runner images ship global zsh rc files (compinit prompts stall the
    # PTY) — same lesson as test_zsh_fish_integration.py. `backscroll run`
    # spawns $SHELL bare, so wrap zsh to add --no-globalrcs.
    path = os.path.join(home, "zsh-wrapper")
    with open(path, "w") as f:
        f.write('#!/bin/sh\nexec zsh --no-globalrcs "$@"\n')
    os.chmod(path, 0o755)
    return path


def write_rc(home, shell, integration, snippet):
    lines = []
    if integration == "kitty" and shell == "bash":
        lines += ['export KITTY_SHELL_INTEGRATION="enabled"',
                  'PS1="$ "',
                  f'source "{KITTY_BASH}"']
    elif integration == "kitty" and shell == "zsh":
        # the documented manual-install block from kitty-integration's header
        inst = os.path.join(home, "kitty-inst")
        os.makedirs(os.path.join(inst, "shell-integration", "zsh"))
        shutil.copy(KITTY_ZSH, os.path.join(
            inst, "shell-integration", "zsh", "kitty-integration"))
        lines += ['PS1="$ "',
                  'export KITTY_SHELL_INTEGRATION="enabled"',
                  f'autoload -Uz -- "{inst}"/shell-integration/zsh/kitty-integration',
                  'kitty-integration',
                  'unfunction kitty-integration']
    elif integration == "wezterm":
        lines += ['PS1="$ "', f'source "{WEZTERM_SH}"']
    if snippet:
        lines.append(f'eval "$({BKS} init {shell})"')
    rc = ".bashrc" if shell == "bash" else ".zshrc"
    with open(os.path.join(home, rc), "w") as f:
        f.write("\n".join(lines) + "\n")


def run_session(home, shell, integration, snippet=False):
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home)
    write_rc(home, shell, integration, snippet)
    spawn_shell = shell if shell == "bash" else make_zsh_wrapper(home)
    env = session_env(home, spawn_shell)

    child = pexpect.spawn(BKS, ["run"], env=env, timeout=15,
                          dimensions=(24, 120), cwd=home)
    child.expect_exact("$ ")
    child.sendline('echo "hi there"')
    child.expect_exact("$ ")
    child.sendline("false")
    child.expect_exact("$ ")
    child.sendline("echo one; echo two")
    child.expect_exact("$ ")
    child.sendline("cd /tmp && pwd")
    child.expect_exact("$ ")
    child.sendline("true")
    child.expect_exact("$ ")
    child.sendline(LONG_CMD)  # >57 bytes: exercises wezterm's word-split base64 path
    child.expect_exact("$ ")
    child.sendline("sleep 30")
    time.sleep(0.8)
    child.send("\x03")  # Ctrl-C mid-command -> 130
    child.expect_exact("$ ")
    child.send("\x03")  # Ctrl-C at empty prompt -> nothing stored
    child.expect_exact("$ ")
    child.sendline("exit")
    child.expect(pexpect.EOF)
    child.wait()
    time.sleep(0.3)
    return rows(home)


def rows(home):
    db = os.path.join(home, ".local", "share", "backscroll", "backscroll.db")
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    out = [dict(r) for r in con.execute(
        "SELECT id, cmd, cwd, exit_code FROM commands ORDER BY id")]
    con.close()
    return out


LONG_CMD = "echo " + "x" * 120
WANT = ['echo "hi there"', "false", "echo one; echo two",
        "cd /tmp && pwd", "true", LONG_CMD, "sleep 30"]
WANT_EXITS = [0, 1, 0, 0, 0, 0, 130]


def scenario(tag, home, shell, integration):
    global RAN
    RAN += 1
    rs = run_session(home, shell, integration)
    got = [r["cmd"] for r in rs]
    check(f"[{tag}] 1. command text recorded", got == WANT, f"got {got}")
    if len(rs) == len(WANT):
        exits = [r["exit_code"] for r in rs]
        check(f"[{tag}] 2. exit codes incl. Ctrl-C 130",
              exits == WANT_EXITS, f"got {exits}")
        check(f"[{tag}] 3. cwd tracking via OSC 7",
              rs[4]["cwd"] == "/tmp", f"got {rs[4]['cwd']!r}")
    check(f"[{tag}] 4. no empty-prompt/exit stubs",
          all(c not in ("", "exit", "logout", "(unknown command)") for c in got),
          f"got {got}")
    r = subprocess.run([BKS, "export", "json"] + [str(x["id"]) for x in rs],
                       env=session_env(home, shell),
                       capture_output=True, text=True)
    check(f"[{tag}] 5. no integration metadata in stored output",
          "]133;" not in r.stdout and "WEZTERM_PROG" not in r.stdout,
          "found 133/WEZTERM_PROG bytes in export")


if KITTY_BASH and os.path.exists(KITTY_BASH):
    scenario("kitty/bash", "/tmp/bks-kitty-bash", "bash", "kitty")
    # double emission: kitty marks + our snippet
    rs = run_session("/tmp/bks-kitty-bash-snip", "bash", "kitty", snippet=True)
    got = [r["cmd"] for r in rs]
    check("[kitty/bash+snippet] 6. stored once, snippet text wins",
          got == WANT, f"got {got}")
else:
    print("SKIP kitty/bash (BKS_TEST_KITTY_BASH unset)")

if KITTY_ZSH and os.path.exists(KITTY_ZSH) and shutil.which("zsh"):
    scenario("kitty/zsh", "/tmp/bks-kitty-zsh", "zsh", "kitty")
else:
    print("SKIP kitty/zsh (BKS_TEST_KITTY_ZSH unset or no zsh)")

if WEZTERM_SH and os.path.exists(WEZTERM_SH):
    scenario("wezterm/bash", "/tmp/bks-wez-bash", "bash", "wezterm")
    if shutil.which("zsh"):
        scenario("wezterm/zsh", "/tmp/bks-wez-zsh", "zsh", "wezterm")
else:
    print("SKIP wezterm (BKS_TEST_WEZTERM_SH unset)")

print()
if FAILURES:
    print(f"{len(FAILURES)} FAILURES: {FAILURES}")
    sys.exit(1)
if RAN == 0:
    print("all scenarios skipped")
else:
    print("all kitty/wezterm E2E checks passed")
