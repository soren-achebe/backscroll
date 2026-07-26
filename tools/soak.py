#!/usr/bin/env python3
"""Soak test: drive a real `backscroll run` bash session through thousands
of commands on a PTY; measure recorder RSS growth, DB size, and query
latency at scale. Sandboxed HOME so the real DB is untouched.

Usage:  go build -o /tmp/bks . && python3 tools/soak.py [N] [/tmp/bks]
Needs pexpect. This is how the v0.4.0 FTS bloat and the prune
no-shrink bug were found; run it before storage-layer releases.
"""
import os, re, shutil, subprocess, sys, time

import pexpect

BKS = sys.argv[2] if len(sys.argv) > 2 else "/tmp/bks"
HOME = "/tmp/bks-soak-home"
N = int(sys.argv[1]) if len(sys.argv) > 1 else 5000

shutil.rmtree(HOME, ignore_errors=True)
os.makedirs(HOME + "/.config", exist_ok=True)
with open(HOME + "/.bashrc", "w") as f:
    f.write('eval "$(%s init bash)"\nPS1="SOAK$ "\n' % BKS)

env = {"HOME": HOME, "TERM": "xterm-256color", "PATH": "/usr/bin:/bin",
       "SHELL": "/bin/bash", "LANG": "C.UTF-8"}

child = pexpect.spawn(BKS, ["run"], encoding=None, timeout=30, env=env,
                      dimensions=(40, 120))
child.expect(rb"SOAK\$ ")

# find recorder pid (the backscroll process we spawned)
rec_pid = child.pid

def rss_kb(pid):
    try:
        with open(f"/proc/{pid}/status") as f:
            for ln in f:
                if ln.startswith("VmRSS"):
                    return int(ln.split()[1])
    except Exception:
        return -1

rss_start = None
t0 = time.time()
big = "x" * 200  # 200-char line
for i in range(N):
    kind = i % 10
    if kind < 6:
        # small unique output (unique tokens exercise FTS index growth)
        child.sendline(b"echo soaktoken%d alpha beta" % i)
    elif kind < 8:
        # multi-line medium output ~8KB
        child.sendline(("seq 1 400 | sed 's/^/ln%d /'" % i).encode())
    elif kind == 8:
        # large output ~120KB (exercises head/tail cap)
        child.sendline(("yes '%s %d' | head -600" % (big, i)).encode())
    else:
        # failing command
        child.sendline(b"ls /nonexistent%d" % i)
    child.expect(rb"SOAK\$ ", timeout=30)
    if i == 50:
        rss_start = rss_kb(rec_pid)
    if i % 1000 == 0:
        print(f"  {i} cmds, {time.time()-t0:.0f}s, recorder RSS {rss_kb(rec_pid)} kB", flush=True)

rss_end = rss_kb(rec_pid)
elapsed = time.time() - t0
child.sendline(b"exit")
child.expect(pexpect.EOF)
child.wait()
print(f"\nDrove {N} commands in {elapsed:.0f}s ({N/elapsed:.1f} cmd/s)")
print(f"Recorder RSS: after warmup {rss_start} kB -> end {rss_end} kB")

db = HOME + "/.local/share/backscroll/backscroll.db"
size = sum(os.path.getsize(db + s) for s in ("", "-wal", "-shm") if os.path.exists(db + s))
print(f"DB size after {N} cmds: {size/1e6:.1f} MB ({size/N:.0f} B/cmd)")

def timed(args, label):
    t = time.time()
    r = subprocess.run([BKS] + args, env=env, capture_output=True, text=True)
    dt = (time.time() - t) * 1000
    lines = len(r.stdout.splitlines())
    print(f"{label}: {dt:.0f} ms ({lines} lines out, rc={r.returncode})")
    return r

timed(["list"], "list (default)")
timed(["search", "soaktoken4321"], "search unique token")
timed(["search", "alpha beta"], "search common tokens")
timed(["list", "--exit", "fail"], "list --exit fail")
timed(["show", "-1"], "show -1")
timed(["stats"], "stats")
r = timed(["search", "ln3007"], "search mid-line token")
sys.stdout.flush()
