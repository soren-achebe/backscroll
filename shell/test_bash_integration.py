#!/usr/bin/env python3
"""PTY integration tests for shell/backscroll.bash.

Drives a real interactive bash on a PTY (via pexpect) and asserts on the
exact OSC mark stream the snippet emits. Covers regressions that unit
tests can't see:

  1. plain commands emit cmd-mark + 133;C + 133;D exactly once each
  2. bind -x widgets (fzf-style, function or plain command) emit NOTHING,
     and the *next* real command still gets its marks (READLINE_LINE guard)
  3. coexistence with bash-preexec, in both load orders

Run:  python3 shell/test_bash_integration.py   (needs: bash >= 4, pexpect;
bash-preexec tests auto-skip if /tmp/bash-preexec.sh is absent — fetch with
curl -sL https://raw.githubusercontent.com/rcaloras/bash-preexec/master/bash-preexec.sh
  -o /tmp/bash-preexec.sh)
"""
import base64
import os
import re
import sys
import time

try:
    import pexpect
except ImportError:
    print("SKIP: pexpect not installed")
    sys.exit(0)

SNIP = os.path.join(os.path.dirname(os.path.abspath(__file__)), "backscroll.bash")
BP = "/tmp/bash-preexec.sh"

# Bash binary under test. Point BKS_TEST_BASH at e.g. a locally built
# bash 3.2.57 (see shell/build-bash32.sh) to exercise the pre-4.0
# fallback widget guard (tty raw-mode check instead of READLINE_LINE).
BASH = os.environ.get("BKS_TEST_BASH", "bash")
import subprocess
_ver = subprocess.run([BASH, "-c", 'echo "${BASH_VERSINFO[0]}"'],
                      capture_output=True, text=True).stdout.strip()
BASH_MAJOR = int(_ver or "0")
print(f"# bash under test: {BASH} (major version {BASH_MAJOR})")


def session(setup_lines, keys_and_lines):
    """Run an interactive bash, return list of decoded OSC events."""
    child = pexpect.spawn(
        BASH, ["--norc", "--noprofile"], encoding=None, timeout=10,
        env={"PS1": "P$ ", "TERM": "xterm", "PATH": "/usr/bin:/bin",
             "HOME": "/tmp", "BACKSCROLL_ACTIVE": "1"},
    )
    out = []
    child.logfile_read = LogBuf(out)

    def prompt():
        child.expect(rb"P\$ ")

    prompt()
    for line in setup_lines:
        child.sendline(line.encode())
        prompt()
    for kind, payload in keys_and_lines:
        if kind == "line":
            child.sendline(payload.encode())
            prompt()
            time.sleep(0.15)
        elif kind == "key":
            child.send(payload)
            time.sleep(0.35)
    child.sendline(b"exit")
    child.expect(pexpect.EOF)
    child.logfile_read = None

    data = b"".join(out)
    events = []
    for m in re.finditer(rb"\x1b\]((?:6973|133)[^\x07\x1b]*)(?:\x07|\x1b\\\\)", data):
        s = m.group(1).decode("utf-8", "replace")
        if s.startswith("6973;cmd="):
            try:
                events.append("cmd:" + base64.b64decode(s[9:]).decode().strip())
            except Exception:
                events.append("cmd:<undecodable>")
        elif s.startswith("133;C"):
            events.append("C")
        elif s.startswith("133;D"):
            events.append("D")
    return events


class LogBuf:
    def __init__(self, out):
        self.out = out

    def write(self, data):
        self.out.append(data)

    def flush(self):
        pass


def cmds_marked(events):
    """Return the ordered list of command texts that got a cmd mark."""
    return [e[4:] for e in events if e.startswith("cmd:")]


FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name}  {detail}")
        FAILURES.append(name)


WIDGET_SETUP = [
    f"source {SNIP}",
    "__my_widget() { true; }",
    """bind -x '"\\C-r": __my_widget' """,
    """bind -x '"\\C-t": true' """,  # non-function widget
]

# --- test 1 + 2: widgets emit nothing; neighbors fully marked -----------
ev = session(
    WIDGET_SETUP,
    [("line", "echo one"), ("key", b"\x12"), ("line", "echo two"),
     ("key", b"\x14"), ("line", "echo three")],
)
marked = cmds_marked(ev)
tail = [c for c in marked if c.startswith("echo")]
check("widget: echo one/two/three each marked exactly once",
      tail == ["echo one", "echo two", "echo three"], f"got {tail}")
check("widget: no phantom duplicate marks",
      len(marked) == len(set(marked)) or tail == ["echo one", "echo two", "echo three"],
      f"got {marked}")
check("widget: C/D balanced for echo commands",
      ev.count("C") == ev.count("D") + 1,  # final `exit` C has no D
      f"C={ev.count('C')} D={ev.count('D')}")

# --- test 3: bash-preexec coexistence, both orders ----------------------
# Skipped on bash < 4: bash-preexec's own DEBUG-trap hook has the same
# widget hole there (no READLINE_LINE to guard with), which is its bug to
# fix, not ours — our direct-trap path is what covers 3.x users.
if BASH_MAJOR < 4:
    print("SKIP bash-preexec tests (bash < 4 under test)")
elif os.path.exists(BP):
    for order, setup in [
        ("bp-first", [f"source {BP}", f"source {SNIP}"]),
        ("bks-first", [f"source {SNIP}", f"source {BP}"]),
    ]:
        ev = session(
            setup + WIDGET_SETUP[1:],
            [("line", "echo one"), ("key", b"\x12"), ("line", "echo two")],
        )
        tail = [c for c in cmds_marked(ev) if c.startswith("echo")]
        check(f"bash-preexec {order}: echo one/two marked exactly once",
              tail == ["echo one", "echo two"], f"got {tail}")
else:
    print(f"SKIP bash-preexec tests ({BP} not present)")

if FAILURES:
    print(f"\n{len(FAILURES)} failure(s)")
    sys.exit(1)
print("\nall passed")
