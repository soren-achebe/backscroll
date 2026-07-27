#!/usr/bin/env python3
"""End-to-end test: recording nushell with its NATIVE OSC 133 marks.

nushell emits shell integration by default ($env.config.shell_integration:
osc133 and osc7 both default to true): 133;A;k=i at prompt start, a
133;P;k=r right-prompt region, 133;B at input start, 133;C before output,
133;D;<exit> after — plus OSC 7 for the cwd and an OSC 2 title. Unlike
fish 4 there is NO structured command line (the title only carries the
first word), so the command text comes from backscroll's echo
reconstruction — which has to survive reedline's full-prompt repaint on
every keystroke (absolute cursor addressing to the bottom row, ED/EL
clears, save/restore around the right prompt, inline autosuggestions in
dim text).

This drives the real `backscroll run` binary on a PTY with nu as the
shell — zero configuration, no snippet — and asserts on the DB:

  1. command text reconstructed exactly (incl. `;`, quotes, unicode)
  2. a command wider than the terminal (wrapped echo) is exact
  3. multiline input (trailing-pipe continuation) keeps its newline
  4. exit codes: 0, nonzero for a failing external, nonzero for Ctrl-C
     mid-command (nu reports 1, not 130 — its own convention)
  5. Ctrl-C at the prompt stores nothing (nu emits a bare D;0 cycle)
  6. the final `exit` leaves no stub entry
  7. cwd captured from OSC 7
  8. up-arrow history recall records the recalled text

nu's reedline probes the terminal at startup (DSR 6n, sometimes DA1 /
OSC 11) and waits for answers, so the driver responds like a real
terminal would.

Env:
  BKS_BIN      path to a built backscroll binary   (default: ./backscroll)
  BKS_TEST_NU  path to a nu binary                 (skips if unset/absent)

Run:  BKS_TEST_NU=/path/to/nu python3 shell/test_nushell.py
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

try:
    import pexpect
except ImportError:
    print("SKIP: pexpect not installed")
    sys.exit(0)

HERE = os.path.dirname(os.path.abspath(__file__))
BKS = os.environ.get("BKS_BIN", os.path.join(HERE, "..", "backscroll"))
NU = os.environ.get("BKS_TEST_NU", "")

if not NU or not os.path.exists(NU):
    print("SKIP: BKS_TEST_NU not set or missing")
    sys.exit(0)
if not os.path.exists(BKS):
    print(f"SKIP: backscroll binary not found at {BKS} (set BKS_BIN)")
    sys.exit(0)

ver = subprocess.run([NU, "--version"], capture_output=True, text=True).stdout
print(f"nu version: {ver.strip()}")

FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name}  {detail}")
        FAILURES.append(name)


class LogBuf:
    def __init__(self):
        self.chunks = []

    def write(self, data):
        self.chunks.append(data)

    def flush(self):
        pass

    def data(self):
        return b"".join(self.chunks)


def session_env(home):
    """Isolated env: fresh HOME wins even if the runner sets XDG_* dirs."""
    env = dict(os.environ, HOME=home, TERM="xterm-256color", SHELL=NU,
               XDG_CONFIG_HOME=os.path.join(home, ".config"),
               XDG_DATA_HOME=os.path.join(home, ".local", "share"))
    for k in list(env):
        if k.startswith("BACKSCROLL_"):
            del env[k]
    # Make sure nu doesn't think it's inside VS Code and switch to OSC 633.
    for k in ("TERM_PROGRAM", "TERM_PROGRAM_VERSION", "VSCODE_INJECTION"):
        env.pop(k, None)
    return env


def run_session(home, cwd):
    """Run `backscroll run` with nu as $SHELL; return the raw stream."""
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home, exist_ok=True)

    env = session_env(home)
    log = LogBuf()
    child = pexpect.spawn(BKS, ["run"], env=env, cwd=cwd, timeout=10,
                          dimensions=(24, 80))
    child.logfile_read = log
    answered = 0

    def pump(dur):
        """Read output and answer reedline's terminal probes."""
        nonlocal answered
        end = time.time() + dur
        while time.time() < end:
            try:
                child.read_nonblocking(4096, timeout=0.1)
            except pexpect.TIMEOUT:
                pass
            except pexpect.EOF:
                return
            tail = log.data()[answered:]
            if b"\x1b[0c" in tail:
                child.send(b"\x1b[?1;2c")            # DA1: VT100 w/ AVO
            if b"\x1b[6n" in tail:
                child.send(b"\x1b[24;1R")            # DSR: cursor position
            if b"\x1b]11;?" in tail:
                child.send(b"\x1b]11;rgb:0000/0000/0000\x1b\\")
            answered = len(log.data())

    pump(3.0)  # first-run config creation + banner + first prompt
    steps = [
        b"echo hello-nu\r",                    # 1: plain
        b"^false\r",                           # 2: failing external -> exit 1
        b"echo one; echo two\r",               # 3: `;` reconstructed
        b"echo partial", b"\x03",              # Ctrl-C at prompt: no entry
        ("echo " + "x" * 110 + "\r").encode(), # 4: wraps the 80-col PTY
        b"echo alpha |", b"\r", b"str upcase\r",  # 5: multiline continuation
        b"sleep 5sec\r",                       # 6: Ctrl-C mid-command...
    ]
    for step in steps:
        child.send(step)
        pump(1.2)
    child.send(b"\x03")                        # ...interrupt the sleep
    pump(1.5)
    child.send("echo 'héllo → wörld'\r".encode())  # 7: unicode
    pump(1.2)
    child.send(b"\x1b[A")                      # 8: up-arrow recalls the
    pump(0.8)                                  #    unicode echo (most recent)
    child.send(b"\r")
    pump(1.2)
    child.send(b"exit\r")
    pump(1.5)
    try:
        child.expect(pexpect.EOF, timeout=5)
    except Exception:
        pass
    child.close(force=True)
    return log.data()


def db_rows(home):
    """All stored commands, oldest first, via `export --format json -N`."""
    rows = []
    for n in range(1, 20):
        out = subprocess.run([BKS, "export", "--format", "json", f"-{n}"],
                             env=session_env(home),
                             capture_output=True, text=True)
        if out.returncode != 0:
            break
        rows.extend(json.loads(out.stdout))
    rows.sort(key=lambda r: r["id"])
    return rows


LONG = "echo " + "x" * 110

with tempfile.TemporaryDirectory() as tmp:
    home = os.path.join(tmp, "home")
    cwd = os.path.join(tmp, "workdir")
    os.makedirs(cwd, exist_ok=True)
    cwd = os.path.realpath(cwd)
    raw = run_session(home, cwd)

    check("native 133 marks seen in stream",
          b"\x1b]133;A" in raw and b"\x1b]133;C" in raw and b"\x1b]133;D;" in raw,
          "nu didn't emit OSC 133 — shell_integration defaults changed?")
    check("no structured cmdline in stream (title carries first word only)",
          b"cmdline_url=" not in raw and b"\x1b]633;E" not in raw,
          "nu now emits structured text — prefer it over echo reconstruction!")

    rows = db_rows(home)
    cmds = [r["cmd"] for r in rows]
    want = ["echo hello-nu", "^false", "echo one; echo two",
            LONG, "echo alpha |\nstr upcase", "sleep 5sec",
            "echo 'héllo → wörld'", "echo 'héllo → wörld'"]
    check("exactly 8 commands stored", len(rows) == 8, f"got {len(rows)}: {cmds}")
    check("command text reconstructed exactly (incl. up-arrow recall)",
          cmds == want, f"got {cmds}")
    if len(rows) == 8:
        exits = [r["exit_code"] for r in rows]
        check("exit codes (0 ok, 1 fail, 1 for Ctrl-C — nu convention)",
              exits == [0, 1, 0, 0, 0, 1, 0, 0], f"got {exits}")
        check("output captured", "hello-nu" in rows[0].get("output", ""),
              f"got {rows[0].get('output', '')!r}")
        check("multiline output captured (str upcase ran)",
              "ALPHA" in rows[4].get("output", ""),
              f"got {rows[4].get('output', '')!r}")
        check("Ctrl-C mid-command captured ^C in output",
              "^C" in rows[5].get("output", ""),
              f"got {rows[5].get('output', '')!r}")
        check("cwd captured from OSC 7",
              all(os.path.realpath(r["cwd"]) == cwd for r in rows),
              f"got {[r['cwd'] for r in rows]}")
    check("no stub for `exit` / Ctrl-C at prompt / partial input",
          not any(c in ("exit", "", "(unknown command)", "echo partial")
                  for c in cmds), f"got {cmds}")

print()
if FAILURES:
    print(f"{len(FAILURES)} FAILURE(S): {FAILURES}")
    sys.exit(1)
print("all passed")
