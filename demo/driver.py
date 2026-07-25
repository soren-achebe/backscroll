#!/usr/bin/env python3
"""Scripted bash session for the demo GIF.

Run via demo/generate.sh (inside `asciinema rec -c ...`). Spawns bash on a
PTY with a sandboxed $HOME and plays the storyline with human-ish timing.
"""
import fcntl
import os
import pty
import random
import select
import struct
import sys
import termios
import time

COLS, ROWS = 100, 28
HOME = "/tmp/demohome"

# Actions:
#   ("say",  pause_before, line, pause_after)  -> type line + Enter
#   ("type", pause_before, text)               -> type text, no Enter
#   ("raw",  pause_before, bytes, pause_after) -> send raw bytes
SCRIPT = [
    ("say", 0.8, "# ctrl-r finds commands you typed. but who finds what they PRINTED?", 1.5),
    ("say", 0.2, "backscroll run", 1.0),
    ("say", 0.5, "openssl rand -hex 16", 0.9),
    ("say", 0.4, "make deploy", 2.1),
    ("say", 0.4, "healthcheck", 1.5),
    ("say", 0.5, "clear", 0.6),
    ("say", 0.2, "# scrollback is gone. the outputs are not:", 1.1),
    ("say", 0.2, "backscroll show -3", 2.6),
    ("say", 0.4, "backscroll search 'permission denied'", 2.2),
    ("say", 0.4, "backscroll list --exit fail --since 1h", 1.9),
    ("say", 0.5, "# minutes later, after a fix... did anything actually change?", 1.1),
    ("say", 0.2, "healthcheck", 1.3),
    ("say", 0.3, "backscroll diff -1", 3.0),
    ("say", 0.4, "clear", 0.5),
    ("say", 0.2, "# ctrl-x ctrl-p: like ctrl-r, but the preview shows the OUTPUT", 1.2),
    ("raw", 0.3, b"\x18\x10", 1.8),   # C-x C-p -> fzf picker over history
    ("raw", 0.0, b"h", 0.5),          # narrow it live...
    ("raw", 0.0, b"e", 0.5),
    ("raw", 0.0, b"x", 2.4),          # -> openssl entry, preview = its output
    ("raw", 0.2, b"\r", 1.1),         # accept -> inserts picked command
    ("raw", 0.4, b"\r", 1.3),         # run it
    ("say", 0.5, "exit", 0.5),
    ("say", 0.2, "exit", 0.4),
]


def pump(fd, seconds):
    """Relay child output to stdout for `seconds`."""
    end = time.time() + seconds
    while True:
        left = end - time.time()
        if left <= 0:
            return
        r, _, _ = select.select([fd], [], [], min(left, 0.05))
        if fd in r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return
            if not data:
                return
            os.write(1, data)


def type_text(fd, rng, text):
    for ch in text:
        os.write(fd, ch.encode())
        pump(fd, rng.uniform(0.02, 0.055))


def main():
    env = dict(os.environ)
    env.update(
        HOME=HOME,
        HOSTNAME="demo",
        TERM="xterm-256color",
        SHELL="/bin/bash",
        PATH="/tmp/demobin:" + env.get("PATH", ""),
        LANG="C.UTF-8",
    )
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(os.path.join(HOME, "project"))
        os.execvpe("bash", ["bash", "-i"], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    rng = random.Random(7)
    for action in SCRIPT:
        kind = action[0]
        if kind == "say":
            _, before, line, after = action
            pump(fd, before)
            type_text(fd, rng, line)
            pump(fd, 0.15)
            os.write(fd, b"\r")
            pump(fd, after)
        elif kind == "type":
            _, before, text = action
            pump(fd, before)
            type_text(fd, rng, text)
        elif kind == "raw":
            _, before, data, after = action
            pump(fd, before)
            os.write(fd, data)
            pump(fd, after)
    pump(fd, 0.5)
    try:
        os.waitpid(pid, 0)
    except ChildProcessError:
        pass


if __name__ == "__main__":
    main()
