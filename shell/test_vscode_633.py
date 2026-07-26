#!/usr/bin/env python3
r"""End-to-end test: recording a shell that emits VS Code's OSC 633 marks.

VS Code's shell integration speaks its own OSC 633 protocol: A/B/C/D
prompt/command marks (same shape as OSC 133), plus E carrying the literal
command line (backslash-escaped: `\\` and `\xHH`) and P;Cwd=<path> for the
cwd. VS Code injects the script automatically for top-level terminals, and
its docs recommend *manual* installation in rc files for tmux/SSH/subshell
setups — so a shell inside `backscroll run` can be emitting 633 with no
backscroll snippet at all. backscroll consumes those marks: zero-config
recording, and no 633 metadata leaking into stored output.

This test drives the real `backscroll run` binary on a PTY with bash
sourcing the REAL shellIntegration-bash.sh (pinned; see ci.yml) and
asserts on the resulting database:

  no-snippet session (633 marks only):
    1. command text comes from the E mark (incl. `\x3b` -> `;`, `\\` -> `\`)
    2. exit codes are right (incl. 130 for Ctrl-C mid-command)
    3. cwd tracking follows P;Cwd (post-command mark applies to the NEXT
       entry, so a `cd` is attributed to the directory it STARTED in)
    4. Ctrl-C at the prompt stores nothing (bare 633;D with no C)
    5. the final `exit` doesn't leave a stub entry
    6. no literal `]633;` metadata inside any stored output
  with-snippet session (double emission):
    7. every command stored exactly once — duplicate C/D marks collapse
    8. our snippet's authoritative text wins over the 633 E hint

Env:
  BKS_BIN          path to a built backscroll binary (default: ./backscroll)
  BKS_TEST_VSC_SH  path to shellIntegration-bash.sh  (skips if unset/absent)

Run:  BKS_TEST_VSC_SH=/path/to/shellIntegration-bash.sh \
          python3 shell/test_vscode_633.py
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
VSC_SH = os.environ.get("BKS_TEST_VSC_SH", "")

if not VSC_SH or not os.path.exists(VSC_SH):
    print("SKIP: BKS_TEST_VSC_SH not set or missing")
    sys.exit(0)
if not os.path.exists(BKS):
    print(f"SKIP: backscroll binary not found at {BKS} (set BKS_BIN)")
    sys.exit(0)

FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name}  {detail}")
        FAILURES.append(name)


def session_env(home):
    env = dict(os.environ, HOME=home, TERM="xterm-256color", SHELL="/bin/bash")
    for k in list(env):
        if k.startswith("BACKSCROLL_") or k.startswith("XDG_") or k.startswith("VSCODE_"):
            del env[k]
    return env


def run_session(home, snippet):
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home)
    with open(os.path.join(home, ".bashrc"), "w") as f:
        f.write(f'. "{VSC_SH}"\n')
        if snippet:
            f.write(f'eval "$({BKS} init bash)"\n')
        f.write('PS1="$ "\n')

    env = session_env(home)
    child = pexpect.spawn(BKS, ["run"], env=env, timeout=10,
                          dimensions=(24, 120), cwd=home)
    child.expect_exact("$ ")
    child.sendline("echo hello-vsc")
    child.expect_exact("$ ")
    child.sendline("false")
    child.expect_exact("$ ")
    child.sendline("echo one; echo two")          # E mark carries \x3b
    child.expect_exact("$ ")
    child.sendline("printf '%s-%s\\n' a b")       # E mark carries \\
    child.expect_exact("$ ")
    child.sendline("cd /tmp && pwd")
    child.expect_exact("$ ")
    child.sendline("true")
    child.expect_exact("$ ")
    child.sendline("sleep 30")
    time.sleep(0.6)
    child.send("\x03")                            # Ctrl-C mid-command -> 130
    child.expect_exact("$ ")
    child.send("\x03")                            # Ctrl-C at empty prompt
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


def cmds(rs):
    return [r["cmd"] for r in rs]


# --- scenario 1: 633 marks only, no snippet -------------------------------
rs = run_session("/tmp/bks-vsc-nosnip", snippet=False)
want = ["echo hello-vsc", "false", "echo one; echo two",
        "printf '%s-%s\\n' a b", "cd /tmp && pwd", "true", "sleep 30"]
check("1. command text from E marks", cmds(rs) == want, f"got {cmds(rs)}")
if len(rs) == len(want):
    check("2. exit codes incl. Ctrl-C 130",
          [r["exit_code"] for r in rs] == [0, 1, 0, 0, 0, 0, 130],
          f"got {[r['exit_code'] for r in rs]}")
    home = "/tmp/bks-vsc-nosnip"
    check("3. cwd tracking via P;Cwd",
          rs[4]["cwd"] == home and rs[5]["cwd"] == "/tmp",
          f"cd ran in {rs[4]['cwd']!r}, next in {rs[5]['cwd']!r}")
check("4./5. no empty-prompt or exit stub entries",
      all(c not in ("", "exit", "(unknown command)") for c in cmds(rs)),
      f"got {cmds(rs)}")

# stored raw output must not contain 633 metadata
r = subprocess.run([BKS, "export", "json"] + [str(x["id"]) for x in rs],
                   env=session_env("/tmp/bks-vsc-nosnip"),
                   capture_output=True, text=True)
check("6. no ]633; metadata in stored output",
      "]633;" not in r.stdout, "found 633 sequences in export")

# --- scenario 2: 633 marks + our snippet (double emission) ----------------
rs2 = run_session("/tmp/bks-vsc-snip", snippet=True)
check("7. no duplicate entries with snippet installed",
      cmds(rs2) == want, f"got {cmds(rs2)}")
check("8. snippet text authoritative",
      len(rs2) == len(want) and rs2[3]["cmd"] == "printf '%s-%s\\n' a b",
      f"got {rs2[3]['cmd'] if len(rs2) > 3 else rs2}")

print()
if FAILURES:
    print(f"{len(FAILURES)} FAILURES: {FAILURES}")
    sys.exit(1)
print("all vscode-633 E2E checks passed")
