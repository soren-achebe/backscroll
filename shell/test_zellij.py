#!/usr/bin/env python3
"""E2E: the zellij integration (`backscroll init zellij`) against a real zellij.

Drives a real zellij session in a PTY and asserts:
  - the shipped keybinds block merges with zellij's defaults when appended
    as a standalone config (the documented no-existing-keybinds path)
  - Alt b opens `backscroll pick --pager` in a floating pane over real
    recorded history; fzf narrows; Enter pages the stored output; q closes
    the pane (close_on_exit) and focus returns to the shell
  - Alt Shift b shows only failed commands
  - zellij's own default binds (Alt n) still work — merge, not clobber
  - `backscroll run` records correctly *inside* a zellij pane

Env:
  BKS_BIN          path to the backscroll binary (default: ./backscroll)
  BKS_TEST_ZELLIJ  path to the zellij binary (default: zellij on PATH)

Needs: pexpect, fzf, less.
"""

import os
import shutil
import subprocess
import sys
import time

import pexpect

BKS = os.path.abspath(os.environ.get("BKS_BIN", "./backscroll"))
ZELLIJ = os.environ.get("BKS_TEST_ZELLIJ", "zellij")
HOME = "/tmp/bks-zellij-test-home"

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
    )
    # zellij must not join an outer session; force config/data under HOME
    for k in (
        "ZELLIJ",
        "ZELLIJ_SESSION_NAME",
        "ZELLIJ_CONFIG_FILE",
        "ZELLIJ_CONFIG_DIR",
        "XDG_CONFIG_HOME",
        "XDG_DATA_HOME",
        "XDG_CACHE_HOME",
        "XDG_RUNTIME_DIR",
        "BACKSCROLL_DB",
    ):
        env.pop(k, None)
    return env


def setup_home():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(os.path.join(HOME, "bin"))
    os.makedirs(os.path.join(HOME, ".config", "zellij"))
    shutil.copy(BKS, os.path.join(HOME, "bin", "backscroll"))
    zj = shutil.which(ZELLIJ)
    assert zj, f"zellij binary not found: {ZELLIJ}"
    shutil.copy(zj, os.path.join(HOME, "bin", "zellij"))
    with open(os.path.join(HOME, ".bashrc"), "w") as f:
        f.write('eval "$(backscroll init bash)"\n')
    # The documented path for users with no keybinds block: append the
    # snippet verbatim. Quiet popups so dumps are deterministic.
    snippet = subprocess.run(
        [BKS, "init", "zellij"], capture_output=True, text=True, check=True
    ).stdout
    with open(os.path.join(HOME, ".config", "zellij", "config.kdl"), "w") as f:
        f.write(snippet)
        f.write("show_startup_tips false\nshow_release_notes false\n")


def zellij_action(env, session, *args):
    return subprocess.run(
        [os.path.join(HOME, "bin", "zellij"), "--session", session, "action", *args],
        env=env,
        capture_output=True,
        text=True,
    )


def dump(env, session):
    path = os.path.join(HOME, "dump.txt")
    if os.path.exists(path):
        os.unlink(path)
    zellij_action(env, session, "dump-screen", "--path", path)
    return open(path).read() if os.path.exists(path) else ""


def wait_dump(env, session, needle, tries=10, delay=0.5):
    """Poll dump-screen until needle appears (zellij renders async)."""
    s = ""
    for _ in range(tries):
        s = dump(env, session)
        if needle in s:
            return s
        time.sleep(delay)
    return s


def record_history(env):
    r = pexpect.spawn(
        os.path.join(HOME, "bin", "backscroll"),
        ["run"],
        env=env,
        dimensions=(24, 100),
        encoding="utf-8",
        timeout=20,
    )
    time.sleep(2)
    r.send("echo zellij-e2e-marker-alpha\r")
    time.sleep(1)
    r.send("ls /nonexistent-zj\r")
    time.sleep(1)
    r.send("printf 'multi\\nline\\noutput-beta\\n'\r")
    time.sleep(1)
    r.send("exit\r")
    r.expect(pexpect.EOF, timeout=10)
    out = subprocess.run(
        [BKS, "list", "-n", "10"], env=env, capture_output=True, text=True
    ).stdout
    check(
        "history recorded (3 cmds incl. one failure)",
        "marker-alpha" in out and "nonexistent-zj" in out and "output-beta" in out,
    )


def test_floating_pick(env):
    z = pexpect.spawn(
        os.path.join(HOME, "bin", "zellij"),
        ["--session", "bkszj"],
        env=env,
        dimensions=(32, 110),
        encoding="utf-8",
        timeout=20,
    )
    try:
        time.sleep(4)
        z.send("\x1bb")  # Alt b
        s = wait_dump(env, "bkszj", "3/3")
        check(
            "Alt b opens floating pick over history",
            "3/3" in s and "nonexistent-zj" in s and "printf" in s,
        )
        check("floating pane named backscroll", "backscroll" in s)

        z.send("beta")  # fzf query narrows
        s = wait_dump(env, "bkszj", "1/3")
        check("fzf narrows to printf cmd", "output-beta" in s)

        z.send("\r")  # select -> pager over stored output
        s = wait_dump(env, "bkszj", "output-beta")
        check("pager shows stored output", "multi" in s and "output-beta" in s)

        z.send("q")  # quit pager -> pick exits -> close_on_exit closes pane
        time.sleep(2)
        z.send("echo back-in-shell\r")
        s = wait_dump(env, "bkszj", "back-in-shell")
        check("pane closed, focus back at shell", "back-in-shell" in s)
        check("pick UI gone after close", "enter: view output" not in s)

        z.send("\x1bB")  # Alt Shift b: failures only
        s = wait_dump(env, "bkszj", "1/1")
        check("Alt Shift b lists the failed cmd", "nonexistent-zj" in s)
        check("Alt Shift b hides successful cmds", "marker-alpha" not in s)
        z.send("\x1b")  # esc closes fzf
        time.sleep(2)

        z.send("\x1bn")  # default bind: new pane — defaults must survive merge
        time.sleep(2)
        layout = zellij_action(env, "bkszj", "dump-layout").stdout
        check(
            "zellij default Alt n still works (keybinds merged, not clobbered)",
            layout.count("pane") > 1,
        )
    finally:
        z.close(force=True)


def test_record_inside_zellij(env):
    z = pexpect.spawn(
        os.path.join(HOME, "bin", "zellij"),
        ["--session", "bkszj2"],
        env=env,
        dimensions=(32, 110),
        encoding="utf-8",
        timeout=20,
    )
    try:
        time.sleep(4)
        z.send("backscroll run\r")
        time.sleep(2)
        z.send("echo inside-zellij-rec-gamma\r")
        time.sleep(1)
        z.send("false\r")
        time.sleep(1)
        z.send("exit\r")
        time.sleep(2)
        out = subprocess.run(
            [BKS, "list", "-n", "5"], env=env, capture_output=True, text=True
        ).stdout
        check(
            "recording inside a zellij pane (cmd + failing exit)",
            "inside-zellij-rec-gamma" in out and "false" in out,
        )
        shown = subprocess.run(
            [BKS, "show", "-2"], env=env, capture_output=True, text=True
        ).stdout
        check("output captured inside zellij", "inside-zellij-rec-gamma" in shown)
    finally:
        z.close(force=True)


def main():
    setup_home()
    env = make_env()
    try:
        record_history(env)
        test_floating_pick(env)
        test_record_inside_zellij(env)
    finally:
        subprocess.run(
            [os.path.join(HOME, "bin", "zellij"), "kill-all-sessions", "-y"],
            env=env,
            capture_output=True,
        )
    if failures:
        print("\nFAILED:", len(failures))
        for f in failures:
            print(" -", f)
        sys.exit(1)
    print("\nall zellij integration checks passed")


if __name__ == "__main__":
    main()
