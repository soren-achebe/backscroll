#!/usr/bin/env python3
"""E2E: PowerShell (pwsh) integration under `backscroll run`.

Verifies shell/backscroll.ps1 against a real pwsh driven over a PTY:
exact command text via OSC 6973 (base64 from PSConsoleHostReadLine),
exit codes (native failure, cmdlet success after a stale $LASTEXITCODE),
cwd via OSC 7 (percent-decoded), and that empty accepts / Ctrl-C at the
prompt / the final `exit` store nothing.

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

    env = dict(os.environ, HOME=HOME, TERM="xterm-256color", SHELL=PWSH,
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

    report()


def report():
    bad = [n for n, ok in checks if not ok]
    print(f"\n{len(checks) - len(bad)}/{len(checks)} checks passed")
    if bad:
        print("FAILED:", ", ".join(bad))
        sys.exit(1)


if __name__ == "__main__":
    main()
