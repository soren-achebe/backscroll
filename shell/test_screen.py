#!/usr/bin/env python3
"""E2E: the GNU screen integration (`backscroll init screen`) vs real screen.

Drives a real `screen` session in a PTY and asserts:
  - `backscroll run` records correctly *inside* a screen window
    (text, output, failing exit codes — the documented usage)
  - the shipped ~/.screenrc snippet parses cleanly (screen aborts the
    whole rc block on syntax errors — the binds working proves it)
  - C-a B opens `backscroll pick --pager` in a new window over real
    recorded history; fzf narrows; Enter pages the stored output; q
    quits the pager, the picker exits, the window closes itself and
    focus returns to the original shell window
  - C-a F shows only failed commands
  - screen's default C-a c (new window) still works alongside our binds

Env:
  BKS_BIN          path to the backscroll binary (default: ./backscroll)
  BKS_TEST_SCREEN  path to the screen binary (default: screen on PATH)

Needs: pexpect, fzf, less.
"""

import os
import shutil
import subprocess
import sys
import time

import pexpect

BKS = os.path.abspath(os.environ.get("BKS_BIN", "./backscroll"))
SCREEN = os.environ.get("BKS_TEST_SCREEN", "screen")
HOME = "/tmp/bks-screen-test-home"
SESSION = "bksscr"

failures = []


def check(name, cond):
    print(("PASS" if cond else "FAIL"), name, flush=True)
    if not cond:
        failures.append(name)


def make_env():
    env = dict(os.environ)
    env.update(
        TERM="xterm-256color",
        HOME=HOME,
        SHELL="/bin/bash",
        PATH=os.path.join(HOME, "bin") + os.pathsep + env.get("PATH", ""),
        # keep the test hermetic: screen sockets under HOME, not /run
        SCREENDIR=os.path.join(HOME, ".screen-sockets"),
    )
    for k in ("STY", "WINDOW", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "BACKSCROLL_DB"):
        env.pop(k, None)
    return env


def setup_home():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(os.path.join(HOME, "bin"))
    os.makedirs(os.path.join(HOME, ".screen-sockets"), mode=0o700)
    shutil.copy(BKS, os.path.join(HOME, "bin", "backscroll"))
    scr = shutil.which(SCREEN)
    assert scr, f"screen binary not found: {SCREEN}"
    with open(os.path.join(HOME, ".bashrc"), "w") as f:
        f.write('eval "$(backscroll init bash)"\nPS1="scr$ "\n')
    # The documented path: append the snippet verbatim to ~/.screenrc.
    snippet = subprocess.run(
        [BKS, "init", "screen"], capture_output=True, text=True, check=True
    ).stdout
    with open(os.path.join(HOME, ".screenrc"), "w") as f:
        f.write("startup_message off\n")
        f.write(snippet)


def scmd(env, *args):
    return subprocess.run(
        [SCREEN, "-S", SESSION, "-X", *args], env=env, capture_output=True, text=True
    )


def dump(env):
    path = os.path.join(HOME, "hardcopy.txt")
    if os.path.exists(path):
        os.unlink(path)
    scmd(env, "hardcopy", path)
    for _ in range(10):
        if os.path.exists(path):
            try:
                return open(path, errors="replace").read()
            except OSError:
                pass
        time.sleep(0.2)
    return ""


def wait_dump(env, needle, tries=24, delay=0.5):
    s = ""
    for _ in range(tries):
        s = dump(env)
        if needle in s:
            return s
        time.sleep(delay)
    return s


def bks(env, *args):
    return subprocess.run(
        [BKS, *args], env=env, capture_output=True, text=True
    ).stdout


def test_record_inside_screen(env, p):
    p.send("backscroll run\r")
    time.sleep(2)
    p.send("echo screen-e2e-marker-alpha\r")
    time.sleep(1)
    p.send("ls /nonexistent-scr\r")
    time.sleep(1)
    p.send("printf 'multi\\nline\\noutput-beta\\n'\r")
    time.sleep(1)
    p.send("exit\r")
    time.sleep(2)
    out = bks(env, "list", "-n", "10")
    check(
        "recording inside a screen window (3 cmds incl. one failure)",
        "marker-alpha" in out
        and "nonexistent-scr" in out
        and "output-beta" in out,
    )
    shown = bks(env, "show", "-1")
    check("output captured inside screen", "output-beta" in shown and "multi" in shown)
    fails = bks(env, "list", "--exit", "fail", "-n", "10")
    check(
        "failing exit recorded inside screen",
        "nonexistent-scr" in fails and "marker-alpha" not in fails,
    )


def test_pick_binding(env, p):
    p.send("\x01B")  # C-a B
    s = wait_dump(env, "3/3")
    check(
        "C-a B opens pick in a new window over history",
        "3/3" in s and "nonexistent-scr" in s and "printf" in s,
    )

    p.send("beta")  # fzf query narrows
    s = wait_dump(env, "1/3")
    check("fzf narrows to printf cmd", "output-beta" in s)

    p.send("\r")  # select -> pager over stored output
    s = wait_dump(env, "output-beta")
    check("pager shows stored output", "multi" in s and "output-beta" in s)

    p.send("q")  # quit pager -> pick exits -> window closes itself
    time.sleep(2)
    p.send("echo back-in-shell\r")
    s = wait_dump(env, "back-in-shell")
    check("window closed, focus back at original shell", "back-in-shell" in s)
    check("pick UI gone after close", "enter: view output" not in s)


def test_failures_binding(env, p):
    p.send("\x01F")  # C-a F
    s = wait_dump(env, "1/1")
    check("C-a F lists only the failed cmd", "nonexistent-scr" in s)
    check("C-a F hides successful cmds", "marker-alpha" not in s)
    p.send("\x1b")  # esc closes fzf -> window closes
    time.sleep(2)


def test_default_binds_survive(env, p):
    p.send("\x01c")  # default: create window
    time.sleep(1.5)
    p.send("echo second-window-ok\r")
    s = wait_dump(env, "second-window-ok")
    check("screen default C-a c still works", "second-window-ok" in s)
    p.send("exit\r")
    time.sleep(1)


def main():
    setup_home()
    env = make_env()
    p = pexpect.spawn(
        SCREEN,
        ["-S", SESSION],
        env=env,
        dimensions=(32, 110),
        encoding="utf-8",
        timeout=20,
    )
    try:
        time.sleep(2)
        test_record_inside_screen(env, p)
        test_pick_binding(env, p)
        test_failures_binding(env, p)
        test_default_binds_survive(env, p)
    finally:
        scmd(env, "quit")
        p.close(force=True)
    if failures:
        print("\nFAILED:", len(failures))
        for f in failures:
            print(" -", f)
        sys.exit(1)
    print("\nall GNU screen integration checks passed")


if __name__ == "__main__":
    main()
