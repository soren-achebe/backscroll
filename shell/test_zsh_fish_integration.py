#!/usr/bin/env python3
"""PTY integration tests for shell/backscroll.zsh and shell/backscroll.fish.

Drives a real interactive zsh/fish on a PTY (via pexpect) and asserts on
the exact OSC mark stream each snippet emits. Companion to
test_bash_integration.py; covers the same regression classes:

  1. plain commands emit cmd-mark + 133;C + 133;D exactly once each,
     in order, with the right exit code in D
  2. key-bound widgets that run commands emit NOTHING (zsh preexec and
     fish_preexec are purpose-built hooks — unlike bash's DEBUG trap they
     don't fire for widget bodies; this test pins that assumption so a
     future rework can't silently regress it)
  3. sourcing the snippet twice hooks only once (BACKSCROLL_HOOKED guard)

Run:  python3 shell/test_zsh_fish_integration.py   (needs pexpect;
zsh/fish sections auto-skip if the shell isn't installed)
"""
import base64
import os
import re
import shutil
import sys
import tempfile
import time

try:
    import pexpect
except ImportError:
    print("SKIP: pexpect not installed")
    sys.exit(0)

HERE = os.path.dirname(os.path.abspath(__file__))
ZSH_SNIP = os.path.join(HERE, "backscroll.zsh")
FISH_SNIP = os.path.join(HERE, "backscroll.fish")

FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name}  {detail}")
        FAILURES.append(name)


class LogBuf:
    def __init__(self, out):
        self.out = out

    def write(self, data):
        self.out.append(data)

    def flush(self):
        pass


def decode_events(data):
    """Parse the raw PTY byte stream into a list of decoded OSC events."""
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
            parts = s.split(";")
            events.append("D:" + (parts[2] if len(parts) > 2 else "?"))
    return events


def drive(child, actions, prompt_re):
    out = []
    child.logfile_read = LogBuf(out)
    child.expect(prompt_re)
    for kind, payload in actions:
        if kind == "line":
            child.sendline(payload.encode())
            child.expect(prompt_re)
            time.sleep(0.15)
        elif kind == "key":
            child.send(payload)
            time.sleep(0.4)
    child.sendline(b"exit")
    child.expect(pexpect.EOF)
    child.logfile_read = None
    return decode_events(b"".join(out))


def cmds_marked(events):
    return [e[4:] for e in events if e.startswith("cmd:")]


def d_after_cmd(events, cmd):
    """Exit code of the first D event after `cmd`'s mark."""
    try:
        i = events.index("cmd:" + cmd)
    except ValueError:
        return None
    for e in events[i + 1:]:
        if e.startswith("cmd:"):
            return None  # next command started before a D showed up
        if e.startswith("D:"):
            return e[2:]
    return None


ACTIONS = [
    ("line", "echo one"),
    ("key", "\x14"),  # Ctrl-T -> widget that runs a command
    ("line", "echo two"),
    ("line", "false"),
]
EXPECTED = ["echo one", "echo two", "false"]


def run_asserts(shell, events, double_sourced_events):
    marked = [c for c in cmds_marked(events) if c in EXPECTED + ["exit"]]
    check(f"{shell}: commands marked exactly once, in order",
          [c for c in marked if c != "exit"] == EXPECTED, f"got {marked}")
    check(f"{shell}: widget emitted no cmd mark",
          not any("true" in c or "widget" in c for c in cmds_marked(events)),
          f"got {cmds_marked(events)}")
    check(f"{shell}: false recorded with exit code 1",
          d_after_cmd(events, "false") == "1",
          f"got {d_after_cmd(events, 'false')}")
    check(f"{shell}: echo one recorded with exit code 0",
          d_after_cmd(events, "echo one") == "0",
          f"got {d_after_cmd(events, 'echo one')}")
    check(f"{shell}: every C has a preceding cmd mark",
          events.count("C") == len(cmds_marked(events)),
          f"C={events.count('C')} cmds={len(cmds_marked(events))}")
    marked2 = [c for c in cmds_marked(double_sourced_events) if c in EXPECTED]
    check(f"{shell}: double-source still marks exactly once",
          marked2 == EXPECTED, f"got {marked2}")


# --------------------------- zsh ---------------------------------------
def zsh_session(source_lines):
    zdot = tempfile.mkdtemp(prefix="bkszsh")
    with open(os.path.join(zdot, ".zshrc"), "w") as f:
        f.write("PS1='P$ '\nunsetopt zle_bracketed_paste 2>/dev/null\n")
        f.write("\n".join(source_lines) + "\n")
        f.write(
            "__test_widget() { command true; zle reset-prompt }\n"
            "zle -N __test_widget\n"
            "bindkey '^T' __test_widget\n"
        )
    child = pexpect.spawn(
        "zsh", ["-i"], encoding=None, timeout=10,
        env={"ZDOTDIR": zdot, "HOME": zdot, "TERM": "xterm",
             "PATH": "/usr/bin:/bin", "BACKSCROLL_ACTIVE": "1",
             "BACKSCROLL_NO_BIND": "1"},
    )
    try:
        return drive(child, ACTIONS, rb"P\$ ")
    finally:
        shutil.rmtree(zdot, ignore_errors=True)


if shutil.which("zsh"):
    ev = zsh_session([f"source {ZSH_SNIP}"])
    ev2 = zsh_session([f"source {ZSH_SNIP}", f"source {ZSH_SNIP}"])
    run_asserts("zsh", ev, ev2)
else:
    print("SKIP zsh tests (zsh not installed)")

# --------------------------- fish --------------------------------------
def fish_session(source_lines):
    home = tempfile.mkdtemp(prefix="bksfish")
    cfg = os.path.join(home, ".config", "fish")
    os.makedirs(cfg)
    with open(os.path.join(cfg, "config.fish"), "w") as f:
        f.write("function fish_prompt; printf 'P$ '; end\n")
        f.write("function fish_greeting; end\n")
        f.write("\n".join(source_lines) + "\n")
        f.write(
            "function __test_widget\n"
            "    command true\n"
            "    commandline -f repaint\n"
            "end\n"
            "bind \\ct __test_widget\n"
        )
    child = pexpect.spawn(
        "fish", ["-i"], encoding=None, timeout=10,
        env={"HOME": home, "TERM": "xterm", "PATH": "/usr/bin:/bin",
             "BACKSCROLL_ACTIVE": "1", "BACKSCROLL_NO_BIND": "1"},
    )
    try:
        return drive(child, ACTIONS, rb"P\$ ")
    finally:
        shutil.rmtree(home, ignore_errors=True)


if shutil.which("fish"):
    ev = fish_session([f"source {FISH_SNIP}"])
    ev2 = fish_session([f"source {FISH_SNIP}", f"source {FISH_SNIP}"])
    run_asserts("fish", ev, ev2)
else:
    print("SKIP fish tests (fish not installed)")

if FAILURES:
    print(f"\n{len(FAILURES)} failure(s)")
    sys.exit(1)
print("\nall passed")
