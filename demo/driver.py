#!/usr/bin/env python3
"""Scripted bash session for the demo GIF.

Run via demo/generate.sh (inside `asciinema rec -c ...`). Spawns bash on a
PTY with a sandboxed $HOME and types the storyline with human-ish timing.
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

# (pause_before, line_to_type, pause_after)
SCRIPT = [
    (0.8, "# ctrl-r finds commands you typed. but who finds what they PRINTED?", 1.6),
    (0.2, "backscroll run", 1.0),
    (0.5, "openssl rand -hex 16", 0.9),
    (0.4, "make deploy", 2.2),
    (0.4, "healthcheck", 1.6),
    (0.5, "clear", 0.6),
    (0.2, "# scrollback is gone. the outputs are not:", 1.2),
    (0.2, "backscroll show -3", 2.8),
    (0.4, "backscroll search 'permission denied'", 2.4),
    (0.4, "backscroll list --exit fail --since 1h", 2.0),
    (0.5, "# minutes later, after a fix... did anything actually change?", 1.2),
    (0.2, "healthcheck", 1.4),
    (0.3, "backscroll diff -1", 3.4),
    (0.6, "exit", 0.5),
    (0.2, "exit", 0.4),
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
    for before, line, after in SCRIPT:
        pump(fd, before)
        for ch in line:
            os.write(fd, ch.encode())
            pump(fd, rng.uniform(0.02, 0.055))
        pump(fd, 0.15)
        os.write(fd, b"\r")
        pump(fd, after)
    pump(fd, 0.5)
    try:
        os.waitpid(pid, 0)
    except ChildProcessError:
        pass


if __name__ == "__main__":
    main()
