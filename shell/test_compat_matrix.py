#!/usr/bin/env python3
r"""End-to-end compatibility matrix: backscroll alongside the tools people
actually run in their shells — atuin, starship, zoxide, direnv, oh-my-zsh,
powerlevel10k (incl. instant prompt), in bash, zsh and fish.

Each scenario drives the real `backscroll run` binary on a PTY with a real
rc file that loads the third-party tool(s) plus our snippet, then asserts
on the recorded database: exact command list (no phantom entries from the
tools' hooks/widgets), correct command text, correct exit codes, and that
the third-party tool itself still works (atuin's history has the commands,
direnv loads .envrc, zoxide jumps).

Regression pinned here (found 2026-07-26): when starship loads AFTER
backscroll in .bashrc, starship replaces PROMPT_COMMAND and re-runs the
old one via `eval "$STARSHIP_PROMPT_COMMAND"` inside starship_precmd().
Its own `_starship_set_return` is immediately defeated by the
`if [[ -n "${STARSHIP_PROMPT_COMMAND-}" ]]` test (which resets $? to 0),
so every wrapped precmd — ours included — sees $?=0 and every failing
command would be recorded as exit 0. backscroll now captures the true
exit in its DEBUG trap on the first post-command PROMPT_COMMAND fire,
where $? is still intact. (starship's open refactor PR #7606 restructures
this wholesale; released starship 1.26.0 has the bug.)

Env:
  BKS_BIN            path to a built backscroll binary (default ./backscroll)
  BKS_TEST_TOOLBIN   dir with atuin/starship/zoxide/direnv binaries
                     (scenarios skip per-tool if a binary is missing)
  BKS_TEST_OMZ       path to an oh-my-zsh checkout       (skip if unset)
  BKS_TEST_P10K      path to a powerlevel10k checkout    (skip if unset)
  bash-preexec is expected at /tmp/bash-preexec.sh for the atuin/bash
  scenario (atuin's bash integration requires it); skipped if absent.
"""
import json
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
TOOLBIN = os.environ.get("BKS_TEST_TOOLBIN", "")
OMZ = os.environ.get("BKS_TEST_OMZ", "")
P10K = os.environ.get("BKS_TEST_P10K", "")
BP = "/tmp/bash-preexec.sh"

if not os.path.exists(BKS):
    print(f"SKIP: backscroll binary not found at {BKS} (set BKS_BIN)")
    sys.exit(0)

failures = []


def check(name, cond, detail=""):
    status = "ok  " if cond else "FAIL"
    print(f"{status} {name}" + (f"  [{detail}]" if detail and not cond else ""))
    if not cond:
        failures.append(name)


def have(tool):
    return TOOLBIN and os.path.exists(os.path.join(TOOLBIN, tool))


def session_env(home, shell):
    if shell == "zsh":
        # CI runner images ship global zsh rc files (compinit's compaudit
        # prompt eats keystrokes and stalls the PTY). `backscroll run`
        # spawns $SHELL bare, so point SHELL at a --no-globalrcs wrapper.
        sh = os.path.join(home, "zsh-wrapper")
        with open(sh, "w") as f:
            f.write('#!/bin/sh\nexec zsh --no-globalrcs "$@"\n')
        os.chmod(sh, 0o755)
    else:
        sh = shutil.which(shell) or f"/usr/bin/{shell}"
    return {
        "HOME": home,
        "TERM": "xterm-256color",
        "PATH": f"{TOOLBIN}:/usr/local/bin:/usr/bin:/bin" if TOOLBIN else "/usr/local/bin:/usr/bin:/bin",
        "SHELL": sh,
        "LANG": "C.UTF-8",
        "XDG_DATA_HOME": f"{home}/.local/share",
        "XDG_CONFIG_HOME": f"{home}/.config",
        "XDG_CACHE_HOME": f"{home}/.cache",
    }


def make_home(name, shell, rc_body):
    home = f"/tmp/bks-compat-{name}"
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home)
    if shell == "bash":
        path = os.path.join(home, ".bashrc")
    elif shell == "zsh":
        path = os.path.join(home, ".zshrc")
    else:
        os.makedirs(f"{home}/.config/fish", exist_ok=True)
        path = f"{home}/.config/fish/config.fish"
    with open(path, "w") as f:
        f.write(rc_body)
    return home


def run_session(home, shell, actions, extra_env=None, settle=3.0):
    env = session_env(home, shell)
    if extra_env:
        env.update(extra_env)
    child = pexpect.spawn(BKS, ["run"], env=env, timeout=30, dimensions=(24, 120))
    time.sleep(settle)
    for kind, payload, delay in actions:
        if kind == "line":
            child.sendline(payload.encode())
        else:
            child.send(payload)
        time.sleep(delay)
    child.sendline(b"exit")
    try:
        child.expect(pexpect.EOF, timeout=20)
    except Exception:
        child.close(force=True)
    time.sleep(0.5)
    return env


def rows(home):
    db = os.path.join(home, ".local/share/backscroll/backscroll.db")
    if not os.path.exists(db):
        return []
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    r = con.execute("select id, cmd, exit_code from commands order by id").fetchall()
    con.close()
    return r


def outputs(home, env, ids):
    r = subprocess.run([BKS, "export", "--format", "json"] + [str(i) for i in ids],
                       capture_output=True, text=True, env=env)
    try:
        return {e["id"]: e.get("output", "") for e in json.loads(r.stdout)}
    except Exception:
        return {}


def assert_rows(tag, home, expected):
    """expected: list of (cmd, exit_code)."""
    rs = rows(home)
    check(f"{tag}: {len(expected)} commands recorded, no phantoms",
          len(rs) == len(expected),
          f"got {[(r['cmd'], r['exit_code']) for r in rs]}")
    for r, (cmd, ec) in zip(rs, expected):
        check(f"{tag}: {cmd!r} text", r["cmd"] == cmd, f"got {r['cmd']!r}")
        check(f"{tag}: {cmd!r} exit={ec}", r["exit_code"] == ec,
              f"got {r['exit_code']}")
    return rs


# ---------------------------------------------------------------- bash

def bash_starship(order):
    tag = f"bash+starship({order})"
    lines = ['eval "$(starship init bash)"', f'eval "$({BKS} init bash)"']
    if order == "starship-last":
        lines.reverse()  # backscroll first -> starship wraps PROMPT_COMMAND
    home = make_home(f"star-bash-{order}", "bash", "\n".join(lines) + "\n")
    run_session(home, "bash", [
        ("line", "echo hello-starship", 1.0),
        ("line", "false", 1.0),
        ("line", "sh -c 'exit 7'", 1.0),
        ("line", "echo done", 1.0),
    ])
    assert_rows(tag, home, [
        ("echo hello-starship", 0),
        ("false", 1),
        ("sh -c 'exit 7'", 7),
        ("echo done", 0),
    ])


def bash_atuin():
    tag = "bash+bash-preexec+atuin"
    rc = (f"source {BP}\n"
          'eval "$(atuin init bash)"\n'
          f'eval "$({BKS} init bash)"\n')
    home = make_home("atuin-bash", "bash", rc)
    env = run_session(home, "bash", [
        ("line", "echo hello-atuin", 1.0),
        ("line", "false", 1.0),
        ("key", "\x12", 2.0),   # Ctrl-R -> atuin's TUI (bind -x widget)
        ("key", "\x1b", 1.5),   # Esc closes it
        ("line", "echo after-widget", 1.0),
        ("line", "atuin history list --cmd-only", 2.0),
    ])
    rs = assert_rows(tag, home, [
        ("echo hello-atuin", 0),
        ("false", 1),
        ("echo after-widget", 0),
        ("atuin history list --cmd-only", 0),
    ])
    if len(rs) == 4:
        out = outputs(home, env, [rs[3]["id"]]).get(rs[3]["id"], "")
        check(f"{tag}: atuin itself recorded the commands",
              "echo hello-atuin" in out and "echo after-widget" in out,
              out[:200])


def bash_zoxide_direnv():
    tag = "bash+zoxide+direnv"
    envdir = "/tmp/bks-compat-envdir"
    os.makedirs(envdir, exist_ok=True)
    with open(f"{envdir}/.envrc", "w") as f:
        f.write("export PROBE_VAR=loaded\n")
    rc = ('eval "$(zoxide init bash)"\n'
          'eval "$(direnv hook bash)"\n'
          f'eval "$({BKS} init bash)"\n')
    home = make_home("tools-bash", "bash", rc)
    env = run_session(home, "bash", [
        ("line", f"cd {envdir}", 1.0),
        ("line", "direnv allow", 1.5),
        ("line", "echo var=$PROBE_VAR", 1.0),
        ("line", "cd /", 1.0),
        ("line", "z bks-compat-envdir", 1.0),
        ("line", "pwd", 1.0),
    ])
    rs = rows(home)
    check(f"{tag}: 6 commands recorded, no phantoms", len(rs) == 6,
          f"got {[(r['cmd'], r['exit_code']) for r in rs]}")
    if len(rs) == 6:
        outs = outputs(home, env, [rs[2]["id"], rs[5]["id"]])
        check(f"{tag}: direnv loaded .envrc",
              "var=loaded" in outs.get(rs[2]["id"], ""))
        check(f"{tag}: zoxide jump worked",
              envdir in outs.get(rs[5]["id"], ""))
        check(f"{tag}: all exits 0", all(r["exit_code"] == 0 for r in rs),
              f"{[r['exit_code'] for r in rs]}")


# ----------------------------------------------------------------- zsh

def zsh_omz():
    tag = "zsh+oh-my-zsh"
    rc = (f"export ZSH={OMZ}\n"
          'ZSH_THEME="robbyrussell"\n'
          "DISABLE_AUTO_UPDATE=true\nDISABLE_UPDATE_PROMPT=true\n"
          "plugins=(git)\n"
          "source $ZSH/oh-my-zsh.sh\n"
          f'eval "$({BKS} init zsh)"\n')
    home = make_home("omz-zsh", "zsh", rc)
    run_session(home, "zsh", [
        ("line", "echo hello-omz", 1.0),
        ("line", "false", 1.0),
        ("line", "echo done", 1.0),
    ], settle=4.0)
    assert_rows(tag, home, [
        ("echo hello-omz", 0), ("false", 1), ("echo done", 0),
    ])


def zsh_p10k_instant():
    tag = "zsh+p10k(instant)"
    rc = ('if [[ -r "${XDG_CACHE_HOME:-$HOME/.cache}/p10k-instant-prompt-${(%):-%n}.zsh" ]]; then\n'
          '  source "${XDG_CACHE_HOME:-$HOME/.cache}/p10k-instant-prompt-${(%):-%n}.zsh"\n'
          "fi\n"
          f"source {P10K}/powerlevel10k.zsh-theme\n"
          f'eval "$({BKS} init zsh)"\n')
    home = make_home("p10k-zsh", "zsh", rc)
    ex = {"POWERLEVEL9K_DISABLE_CONFIGURATION_WIZARD": "true"}
    # first run renders normally and writes the instant-prompt cache
    run_session(home, "zsh", [("line", "echo first-run", 1.2)],
                extra_env=ex, settle=5.0)
    cache = [f for f in os.listdir(f"{home}/.cache")
             if f.startswith("p10k-instant-prompt")] if os.path.isdir(f"{home}/.cache") else []
    check(f"{tag}: instant-prompt cache generated", bool(cache))
    # second run actually exercises instant prompt (deferred init, tty games)
    run_session(home, "zsh", [
        ("line", "echo with-instant", 1.2),
        ("line", "false", 1.0),
        ("line", "echo done", 1.0),
    ], extra_env=ex, settle=5.0)
    assert_rows(tag, home, [
        ("echo first-run", 0),
        ("echo with-instant", 0), ("false", 1), ("echo done", 0),
    ])


def zsh_starship():
    tag = "zsh+starship"
    rc = (f'eval "$({BKS} init zsh)"\n'   # backscroll first: riskier order
          'eval "$(starship init zsh)"\n')
    home = make_home("star-zsh", "zsh", rc)
    run_session(home, "zsh", [
        ("line", "echo hello-star", 1.0),
        ("line", "false", 1.0),
    ])
    assert_rows(tag, home, [("echo hello-star", 0), ("false", 1)])


def zsh_atuin():
    tag = "zsh+atuin"
    rc = ('eval "$(atuin init zsh)"\n'
          f'eval "$({BKS} init zsh)"\n')
    home = make_home("atuin-zsh", "zsh", rc)
    env = run_session(home, "zsh", [
        ("line", "echo hello-atuin-zsh", 1.0),
        ("line", "false", 1.0),
        ("key", "\x12", 2.0),   # Ctrl-R -> atuin TUI (zle widget)
        ("key", "\x1b", 1.5),
        ("line", "atuin history list --cmd-only", 2.0),
    ])
    rs = assert_rows(tag, home, [
        ("echo hello-atuin-zsh", 0),
        ("false", 1),
        ("atuin history list --cmd-only", 0),
    ])
    if len(rs) == 3:
        out = outputs(home, env, [rs[2]["id"]]).get(rs[2]["id"], "")
        check(f"{tag}: atuin itself recorded the commands",
              "echo hello-atuin-zsh" in out, out[:200])


# ---------------------------------------------------------------- fish

def fish_kitchen():
    tag = "fish+starship+atuin+zoxide"
    rc = ("starship init fish | source\n"
          "atuin init fish | source\n"
          "zoxide init fish | source\n"
          f"{BKS} init fish | source\n")
    home = make_home("kitchen-fish", "fish", rc)
    env = run_session(home, "fish", [
        ("line", "echo hello-fish", 1.0),
        ("line", "false", 1.0),
        ("key", "\x12", 2.0),   # Ctrl-R -> atuin TUI (fish binding)
        ("key", "\x1b", 1.5),
        ("line", "atuin history list --cmd-only", 2.0),
    ])
    rs = assert_rows(tag, home, [
        ("echo hello-fish", 0),
        ("false", 1),
        ("atuin history list --cmd-only", 0),
    ])
    if len(rs) == 3:
        out = outputs(home, env, [rs[2]["id"]]).get(rs[2]["id"], "")
        check(f"{tag}: atuin itself recorded the commands",
              "echo hello-fish" in out, out[:200])


# ---------------------------------------------------------------- main

def main():
    print(f"# binary under test: {BKS}")
    print(f"# toolbin: {TOOLBIN or '(unset)'}")

    if have("starship"):
        bash_starship("starship-first")
        bash_starship("starship-last")
    else:
        print("SKIP bash+starship (no starship binary)")

    if have("atuin") and os.path.exists(BP):
        bash_atuin()
    else:
        print("SKIP bash+atuin (needs atuin binary + /tmp/bash-preexec.sh)")

    if have("zoxide") and have("direnv"):
        bash_zoxide_direnv()
    else:
        print("SKIP bash+zoxide+direnv (missing binaries)")

    if shutil.which("zsh"):
        if OMZ and os.path.isdir(OMZ):
            zsh_omz()
        else:
            print("SKIP zsh+oh-my-zsh (BKS_TEST_OMZ unset)")
        if P10K and os.path.isdir(P10K):
            zsh_p10k_instant()
        else:
            print("SKIP zsh+p10k (BKS_TEST_P10K unset)")
        if have("starship"):
            zsh_starship()
        if have("atuin"):
            zsh_atuin()
    else:
        print("SKIP zsh scenarios (no zsh)")

    if shutil.which("fish") and have("starship") and have("atuin") and have("zoxide"):
        fish_kitchen()
    else:
        print("SKIP fish kitchen sink (needs fish + starship + atuin + zoxide)")

    print()
    if failures:
        print(f"{len(failures)} FAILURES:")
        for f in failures:
            print(f"  - {f}")
        sys.exit(1)
    print("all passed")


if __name__ == "__main__":
    main()
