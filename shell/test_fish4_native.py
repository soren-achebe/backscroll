#!/usr/bin/env python3
"""End-to-end test: recording fish >= 4.0 with its NATIVE OSC 133 marks.

fish 4.0 started emitting full shell-integration marks itself — 133;A/B,
133;C with the command line attached as a percent-encoded `cmdline_url`
parameter (kitty shell-integration protocol), and 133;D;<status> — plus
OSC 7 for the cwd. That means backscroll can record a fish >= 4.0 session
with ZERO configuration: no snippet sourced at all.

This test drives the real `backscroll run` binary on a PTY with fish 4 as
the shell and asserts on the resulting database:

  no-snippet session (native marks only):
    1. command text comes from cmdline_url (percent-decoded, incl. %3B)
    2. exit codes are right (incl. 130 for Ctrl-C mid-command)
    3. Ctrl-C at the prompt stores nothing
    4. the final `exit` doesn't leave a stub entry
  with-snippet session (double emission):
    5. every command stored exactly once — duplicate C/D marks collapse
    6. our snippet's exact text wins over the native hint

fish 4's reedline probes the terminal at startup (DA1, DSR 6n, OSC 11,
kitty keyboard) and waits for answers, so the driver responds like a real
terminal would.

Env:
  BKS_BIN         path to a built backscroll binary   (default: ./backscroll)
  BKS_TEST_FISH4  path to a fish >= 4.0 binary        (skips if unset/absent)

Run:  BKS_TEST_FISH4=/path/to/fish4 python3 shell/test_fish4_native.py
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
FISH4 = os.environ.get("BKS_TEST_FISH4", "")

if not FISH4 or not os.path.exists(FISH4):
    print("SKIP: BKS_TEST_FISH4 not set or missing")
    sys.exit(0)
if not os.path.exists(BKS):
    print(f"SKIP: backscroll binary not found at {BKS} (set BKS_BIN)")
    sys.exit(0)

ver = subprocess.run([FISH4, "--version"], capture_output=True, text=True).stdout
if " 4." not in ver and " 5." not in ver:
    print(f"SKIP: {FISH4} is not fish >= 4.0 ({ver.strip()})")
    sys.exit(0)

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


def run_session(home, snippet):
    """Run `backscroll run` with fish4 as $SHELL; return the raw stream."""
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(os.path.join(home, ".config", "fish"), exist_ok=True)
    if snippet:
        with open(os.path.join(home, ".config", "fish", "config.fish"), "w") as f:
            f.write(f"{BKS} init fish | source\n")

    env = dict(os.environ, HOME=home, TERM="xterm-256color", SHELL=FISH4)
    log = LogBuf()
    child = pexpect.spawn(BKS, ["run"], env=env, timeout=10,
                          dimensions=(24, 120))
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

    pump(2.5)
    for step in [b"echo hello-world\r",
                 b"false\r",
                 b"echo one; echo two\r",   # ';' rides cmdline_url as %3B
                 b"echo partial", b"\x03",  # Ctrl-C at prompt: no exec
                 b"sleep 5\r"]:
        child.send(step)
        pump(1.2)
    child.send(b"\x03")                     # Ctrl-C mid-command
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
                             env=dict(os.environ, HOME=home),
                             capture_output=True, text=True)
        if out.returncode != 0:
            break
        rows.extend(json.loads(out.stdout))
    rows.sort(key=lambda r: r["id"])
    return rows


def assert_session(tag, home):
    rows = sorted(db_rows(home), key=lambda r: r["id"])
    cmds = [r["cmd"] for r in rows]
    check(f"{tag}: exactly 4 commands stored", len(rows) == 4, f"got {cmds}")
    check(f"{tag}: command text present (no unknowns)",
          cmds[:3] == ["echo hello-world", "false", "echo one; echo two"],
          f"got {cmds}")
    check(f"{tag}: %3B decoded to ';'", any(c == "echo one; echo two" for c in cmds),
          f"got {cmds}")
    if len(rows) == 4:
        check(f"{tag}: exit codes correct",
              [r["exit_code"] for r in rows] == [0, 1, 0, 130],
              f"got {[r['exit_code'] for r in rows]}")
        check(f"{tag}: output captured",
              "hello-world" in rows[0].get("output", ""),
              f"got {rows[0].get('output', '')!r}")
    check(f"{tag}: no stub for `exit` or Ctrl-C at prompt",
          not any(c in ("exit", "", "(unknown command)", "echo partial")
                  for c in cmds), f"got {cmds}")


with tempfile.TemporaryDirectory() as tmp:
    home_a = os.path.join(tmp, "native")
    run_session(home_a, snippet=False)
    assert_session("fish4 native (no snippet)", home_a)

    home_b = os.path.join(tmp, "double")
    raw = run_session(home_b, snippet=True)
    assert_session("fish4 + snippet (double emission)", home_b)
    check("fish4 + snippet: both mark sources present in stream",
          b"cmdline_url=" in raw and b"6973;cmd=" in raw,
          "expected native cmdline_url AND snippet 6973 marks")

print()
if FAILURES:
    print(f"{len(FAILURES)} FAILURE(S): {FAILURES}")
    sys.exit(1)
print("all passed")
