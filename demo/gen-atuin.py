#!/usr/bin/env python3
"""Generate a fake-but-realistic atuin history.db for the demo GIF.

Usage: gen-atuin.py <output.db> (called from demo/generate.sh)
"""
import os
import random
import sqlite3
import sys
import time

out = sys.argv[1]
if os.path.exists(out):
    os.unlink(out)
os.makedirs(os.path.dirname(out), exist_ok=True)

rng = random.Random(1207)
now = time.time()

# (command, weight, fail_rate, dur_lo_s, dur_hi_s, fail_exit)
CMDS = [
    ("git status", 90, 0.0, 0.05, 0.2, 1),
    ("git diff", 55, 0.0, 0.05, 0.3, 1),
    ("git commit -m 'wip'", 35, 0.02, 0.1, 0.3, 1),
    ("git push", 28, 0.12, 0.8, 3.0, 1),
    ("git pull --rebase", 18, 0.05, 0.6, 2.5, 1),
    ("make test", 60, 0.22, 8.0, 45.0, 2),
    ("make build", 25, 0.08, 4.0, 20.0, 2),
    ("go test ./...", 42, 0.17, 3.0, 25.0, 1),
    ("kubectl get pods", 40, 0.02, 0.3, 1.2, 1),
    ("kubectl logs api-7f9c4d", 26, 0.04, 0.4, 6.0, 1),
    ("docker compose up -d", 18, 0.06, 2.0, 9.0, 1),
    ("docker ps", 22, 0.0, 0.1, 0.4, 1),
    ("vim main.go", 30, 0.0, 20.0, 600.0, 1),
    ("ls -la", 45, 0.0, 0.02, 0.1, 1),
    ("cd ~/project", 38, 0.0, 0.01, 0.02, 1),
    ("npm run build", 20, 0.15, 5.0, 30.0, 1),
    ("curl -s localhost:8080/health", 30, 0.10, 0.05, 1.0, 7),
    ("ssh prod-1", 10, 0.05, 30.0, 900.0, 255),
    ("rg TODO", 14, 0.30, 0.05, 0.3, 1),  # rg exits 1 on no match
    ("tail -n 50 /var/log/app.log", 12, 0.0, 0.05, 0.2, 1),
]
pool = []
for cmd, w, fr, lo, hi, fe in CMDS:
    pool.extend([(cmd, fr, lo, hi, fe)] * w)

CWDS = ["/home/dev/project"] * 8 + ["/home/dev/dotfiles", "/home/dev"]

entries = []
t = now - 7 * 86400
while t < now - 600:
    lt = time.localtime(t)
    busy = 9 <= lt.tm_hour <= 19 and lt.tm_wday < 6
    if rng.random() < (0.72 if busy else 0.04):
        cmd, fr, lo, hi, fe = rng.choice(pool)
        exit_code = fe if rng.random() < fr else 0
        dur = rng.uniform(lo, hi)
        entries.append((t, dur, exit_code, cmd, rng.choice(CWDS)))
        t += dur
    t += rng.uniform(35, 240)

db = sqlite3.connect(out)
db.execute("""CREATE TABLE history (
    id text primary key,
    timestamp integer not null,
    duration integer not null,
    exit integer not null,
    command text not null,
    cwd text not null,
    session text not null,
    hostname text not null, deleted_at integer, author text, intent text,
    unique(timestamp, cwd, command)
)""")
session = "0" * 32
for i, (ts, dur, exit_code, cmd, cwd) in enumerate(entries):
    db.execute(
        "INSERT OR IGNORE INTO history VALUES (?,?,?,?,?,?,?,?,NULL,'dev',NULL)",
        (f"{i:032x}", int(ts * 1e9), int(dur * 1e9), exit_code, cmd, cwd,
         session, "demo:dev"),
    )
db.commit()
n = db.execute("select count(*) from history").fetchone()[0]
print(f"seeded {n} atuin entries -> {out}", file=sys.stderr)
