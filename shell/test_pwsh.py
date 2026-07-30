#!/usr/bin/env python3
"""E2E: PowerShell (pwsh) integration under `backscroll run`.

Verifies shell/backscroll.ps1 against a real pwsh driven over a PTY:
exact command text via OSC 6973 (base64 from PSConsoleHostReadLine),
exit codes (native failure, cmdlet success after a stale $LASTEXITCODE),
cwd via OSC 7 (percent-decoded), and that empty accepts / Ctrl-C at the
prompt / the final `exit` store nothing. Also: the Ctrl-X Ctrl-P picker
and tab completion (Register-ArgumentCompleter, sourced from the live
`init pwsh` output).

PSReadLine probes the terminal (DSR 6n) and blocks until answered, so
the driver responds like a real terminal would.

Env: BKS_TEST_BIN (backscroll binary), BKS_TEST_PWSH (pwsh binary).
"""
import os
import shutil
import subprocess
import sys
import time

import pexpect

BKS = os.environ.get("BKS_TEST_BIN", os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "backscroll")))
PWSH = os.environ.get("BKS_TEST_PWSH", shutil.which("pwsh") or "pwsh")

HOME = "/tmp/bks-pwsh-e2e-home"

checks = []


def check(name, ok, detail=""):
    checks.append((name, ok))
    mark = "ok" if ok else "FAIL"
    print(f"  [{mark}] {name}" + ("" if ok else f"  {detail}"))


def bks(*args):
    env = dict(os.environ, HOME=HOME,
               XDG_CONFIG_HOME=os.path.join(HOME, ".config"),
               XDG_DATA_HOME=os.path.join(HOME, ".local", "share"))
    return subprocess.run([BKS, *args], capture_output=True, text=True,
                          env=env).stdout


def main():
    shutil.rmtree(HOME, ignore_errors=True)
    prof_dir = os.path.join(HOME, ".config", "powershell")
    os.makedirs(prof_dir, exist_ok=True)
    with open(os.path.join(prof_dir, "Microsoft.PowerShell_profile.ps1"), "w") as f:
        f.write(f"{BKS} init pwsh | Out-String | Invoke-Expression\n")

    spaced = os.path.join(HOME, "dir with space")
    os.makedirs(spaced, exist_ok=True)

    # the picker key handler invokes bare `backscroll`; put it on PATH
    bindir = os.path.join(HOME, "bin")
    os.makedirs(bindir, exist_ok=True)
    os.symlink(BKS, os.path.join(bindir, "backscroll"))

    env = dict(os.environ, HOME=HOME, TERM="xterm-256color", SHELL=PWSH,
               PATH=bindir + os.pathsep + os.environ.get("PATH", ""),
               XDG_CONFIG_HOME=os.path.join(HOME, ".config"),
               XDG_DATA_HOME=os.path.join(HOME, ".local", "share"),
               XDG_CACHE_HOME=os.path.join(HOME, ".cache"))
    for k in list(env):
        if k.startswith("BACKSCROLL_"):
            del env[k]
    for k in ("TERM_PROGRAM", "TERM_PROGRAM_VERSION", "VSCODE_INJECTION"):
        env.pop(k, None)

    chunks = []

    class Log:
        def write(self, d):
            chunks.append(d)

        def flush(self):
            pass

    child = pexpect.spawn(BKS, ["run"], env=env, cwd=HOME, timeout=20,
                          dimensions=(24, 120))
    child.logfile_read = Log()
    answered = 0

    def pump(dur):
        nonlocal answered
        end = time.time() + dur
        while time.time() < end:
            try:
                child.read_nonblocking(4096, timeout=0.1)
            except pexpect.TIMEOUT:
                pass
            except pexpect.EOF:
                return
            tail = b"".join(chunks)[answered:]
            if b"\x1b[6n" in tail:
                child.send(b"\x1b[24;1R")
            if b"\x1b[0c" in tail:
                child.send(b"\x1b[?1;2c")
            answered = len(b"".join(chunks))

    pump(6.0)  # pwsh startup + profile + first prompt
    steps = [
        (b"echo hello-from-pwsh\r", 1.2),
        (b"Get-Location | Out-String -Stream | Select-Object -First 1\r", 1.2),
        (b"ls /definitely-not-here-xyz\r", 1.2),        # native fail
        (b"Write-Output cmdlet-ok\r", 1.2),             # success w/ stale LASTEXITCODE
        (b"cd 'dir with space'\r", 1.0),
        (b"echo in-spaced-dir\r", 1.2),
        (b"\r", 0.6),                                   # empty accept
        (b"echo trailing", None), (b"\x03", 0.8),       # Ctrl-C at prompt
        (b"exit\r", 1.5),
    ]
    for step in steps:
        data, wait = step
        child.send(data)
        pump(wait if wait is not None else 0.4)
    try:
        child.expect(pexpect.EOF, timeout=10)
    except pexpect.TIMEOUT:
        child.close(force=True)
        check("session exits cleanly", False, "EOF timeout")
        report()
        return
    child.close()
    check("session exits cleanly", True)

    listing = bks("list", "-n", "30")
    print(listing)

    check("plain echo recorded w/ exact text", "echo hello-from-pwsh" in listing)
    check("pipeline text exact",
          "Get-Location | Out-String -Stream | Select-Object -First 1" in listing)
    failing = bks("list", "--exit", "fail")
    check("native failure has nonzero exit",
          "ls /definitely-not-here-xyz" in failing, failing[:300])
    check("cmdlet after stale LASTEXITCODE records exit 0",
          "Write-Output cmdlet-ok" in listing and
          "Write-Output cmdlet-ok" not in failing)
    check("no empty-accept stub", "(unknown command)" not in listing)
    check("Ctrl-C'd prompt line not stored", "echo trailing" not in listing)
    check("exit not stored as a command",
          not any(ln.strip().endswith(" exit") or "  exit  " in ln
                  for ln in listing.splitlines() if "exit-" not in ln))

    out1 = bks("show", "1")
    check("output captured", "hello-from-pwsh" in out1, out1[:200])

    spaced_rows = bks("list", "--cwd", spaced)
    check("cwd percent-decoded (dir with space)",
          "echo in-spaced-dir" in spaced_rows, spaced_rows[:300])

    picker_phase(env)
    completion_phase(env)

    report()


def picker_phase(env):
    """Ctrl-X Ctrl-P picker: UI visible (the stderr-pipe trap), insert,
    initial query from the typed line, cancel preserves the buffer."""
    if not shutil.which("fzf", path=env["PATH"]):
        print("  [skip] picker checks: fzf not on PATH")
        return

    chunks = []

    class Log:
        def write(self, d):
            chunks.append(d)

        def flush(self):
            pass

    def snap():
        return b"".join(chunks)

    child = pexpect.spawn(BKS, ["run"], env=env, cwd=HOME, timeout=20,
                          dimensions=(24, 120))
    child.logfile_read = Log()
    answered = 0

    def pump(dur):
        nonlocal answered
        end = time.time() + dur
        while time.time() < end:
            try:
                child.read_nonblocking(4096, timeout=0.1)
            except pexpect.TIMEOUT:
                pass
            except pexpect.EOF:
                return
            tail = snap()[answered:]
            if b"\x1b[6n" in tail:
                child.send(b"\x1b[24;1R")
            if b"\x1b[0c" in tail:
                child.send(b"\x1b[?1;2c")
            answered = len(snap())

    def pump_until(needle, timeout=10.0):
        end = time.time() + timeout
        start = len(snap())
        while time.time() < end:
            pump(0.3)
            if needle in snap()[start:]:
                return True
        return False

    pump(6.0)  # startup + profile + first prompt
    child.send(b"echo pick-target-zeta-77\r")
    pump(1.2)

    # Chord at an empty prompt: fzf UI must actually render. With a plain
    # `& backscroll pick` capture, PowerShell pipes the native command's
    # stderr inside key handlers and the UI is invisible (keystrokes still
    # land) — this check pins the Process-based launch that avoids it.
    child.send(b"\x18\x10")  # C-x C-p
    check("picker: fzf UI visible (stderr reaches the tty)",
          pump_until(b"pick-target-zeta-77"))

    child.send(b"zeta")  # narrow
    pump(1.0)
    child.send(b"\r")    # accept -> insert at prompt (NOT executed)
    check("picker: pick inserted at redrawn prompt",
          pump_until(b"pick-target-zeta-77"))
    child.send(b"\r")    # now run the inserted command
    pump(1.5)

    # Initial query: typed line seeds fzf; Esc cancels; buffer preserved.
    child.send(b"echo pick-cancel-keep")
    pump(0.8)
    child.send(b"\x18\x10")
    # UI up: the typed line is seeded as fzf's query and rendered by it
    # (it fuzzy-matches nothing, so the candidate list is empty here)
    pump_until(b"pick-cancel-keep", 6.0)
    child.send(b"\x1b")  # Esc = cancel
    check("picker: cancel redraws prompt with typed line intact",
          pump_until(b"pick-cancel-keep", 6.0))
    child.send(b"\r")    # run the preserved original line
    pump(1.5)

    child.send(b"exit\r")
    pump(1.5)
    try:
        child.expect(pexpect.EOF, timeout=10)
        child.close()
        check("picker: session exits cleanly", True)
    except pexpect.TIMEOUT:
        child.close(force=True)
        check("picker: session exits cleanly", False, "EOF timeout")
        return

    listing = bks("list", "-n", "40")
    runs = listing.count("echo pick-target-zeta-77")
    check("picker: inserted command ran and was recorded (2 total)",
          runs == 2, f"count={runs}\n{listing[-500:]}")
    check("picker: cancelled line ran as typed",
          "echo pick-cancel-keep" in listing)
    check("picker: no phantom/stub entries",
          "(unknown command)" not in listing and "fzf" not in listing,
          listing[-400:])



def completion_phase(env):
    """Tab completion (Register-ArgumentCompleter, PR #11 / issue #4):
    subcommand list, init/import/sync targets, flag-value completion via
    the previous word, and the trailing-space vs mid-word AST cases that
    were the original review bugs. Sources the live `init pwsh` output,
    so embed drift fails here too."""
    cases = [
        # (input line, expected completions, exact-space-joined)
        ("backscroll ",
         "run exec init list last show search pick diff export import sync "
         "stats note prune delete redact mcp serve off on doctor upgrade "
         "version help"),
        ("backscroll se", "search serve"),
        # trailing space after a subcommand: positional targets, not files
        # (review bug 2: the empty word is NOT in CommandElements)
        ("backscroll init ", "bash zsh fish pwsh tmux zellij screen"),
        ("backscroll init z", "zsh zellij"),
        ("backscroll import ", "atuin zsh bash fish nu pwsh"),
        ("backscroll sync ", "init export import status"),
        # flag VALUE completion keys off the previous word (review bug 3)
        ("backscroll export --format ", "md cast json html"),
        ("backscroll export --format m", "md"),
        ("backscroll stats --by ", "cmd cwd exit host session day"),
        ("backscroll stats --by c", "cmd cwd"),
        ("backscroll list --exit ", "fail 0 1 2"),
        # flag-name completion ('-o'/'-n'/'-A' correctly filtered by '--')
        ("backscroll export --",
         "--format --details --raw --redact --session --cwd --exit "
         "--since --until --host"),
        ("backscroll search --",
         "--session --cwd --exit --since --until --host --redact"),
        ("backscroll import --d", "--dry-run"),
        ("backscroll prune --", "--older --max-size"),
    ]

    init_ps1 = os.path.join(HOME, "bks-init-pwsh.ps1")
    with open(init_ps1, "w") as f:
        f.write(bks("init", "pwsh"))

    sep = "\x1f"
    lines = [
        "$ErrorActionPreference = 'Stop'",
        "$env:BACKSCROLL_NO_BIND = '1'",
        f". '{init_ps1}'",
        "function T([string]$line) {",
        "    $r = TabExpansion2 -inputScript $line -cursorColumn $line.Length",
        "    ($r.CompletionMatches | ForEach-Object CompletionText) -join ' '",
        "}",
    ]
    for line, _ in cases:
        lines.append(f"Write-Output ('{line}' + [char]0x1f + (T '{line}'))")
    script = os.path.join(HOME, "bks-completion-test.ps1")
    with open(script, "w") as f:
        f.write("\n".join(lines) + "\n")

    proc = subprocess.run([PWSH, "-NoProfile", "-File", script],
                          capture_output=True, text=True, env=env, timeout=120)
    got = {}
    for ln in proc.stdout.splitlines():
        if sep in ln:
            k, v = ln.split(sep, 1)
            got[k] = v.strip()

    if proc.returncode != 0 or not got:
        check("completion: harness ran", False,
              f"rc={proc.returncode} stderr={proc.stderr[:300]}")
        return
    check("completion: harness ran", True)
    for line, want in cases:
        have = got.get(line)
        check(f"completion: '{line}'", have == want,
              f"want [{want}] got [{have}]")

def report():
    bad = [n for n, ok in checks if not ok]
    print(f"\n{len(checks) - len(bad)}/{len(checks)} checks passed")
    if bad:
        print("FAILED:", ", ".join(bad))
        sys.exit(1)


if __name__ == "__main__":
    main()
