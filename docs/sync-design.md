# Cross-machine sync — design sketch (not implemented)

Status: **draft / request for comments** — see the tracking issue for discussion.

## Problem

backscroll answers "what did that command print?" — but only on the machine
where you ran it. The obvious next question is:

> `backscroll search "connection refused"` — across my laptop, my desktop,
> and that build box I SSH into.

## Goals

- Search/show/list history recorded on other machines, locally.
- **Local-first**: every machine keeps working with zero network access.
- **No required server**: piggyback on whatever file sync the user already
  has (Syncthing, Dropbox, a git repo, rsync in a cron job).
- **Private by default**: exported data is end-to-end encrypted; the sync
  transport never sees plaintext.
- Idempotent, conflict-free: syncing twice, partially, or out of order must
  never corrupt anything.

## Non-goals (v1)

- A hosted service or account system.
- Real-time replication. Minutes of lag is fine.
- Syncing deletions/prunes across machines (local prune stays local; see
  Open questions).

## Design: per-machine append-only encrypted logs

Each machine owns exactly one log; logs are only ever appended by their
owner. That makes the whole scheme conflict-free by construction (same trick
as CRDT op logs / atuin's per-host stores).

### Layout

```
<syncdir>/                     # e.g. ~/Sync/backscroll, user-chosen
  <machine-id>/                # random 128-bit id, minted on first export
    meta.json                  # { "host": "laptop", "created": ... }
    000001.seg                 # encrypted segment files, ~4 MB each
    000002.seg
    ...
```

- `machine-id` is random, not the hostname — hostnames collide and change.
  The human-readable name lives in `meta.json` and in each record.
- Segments are sealed once full; the newest segment is rewritten in place
  until it reaches the size cap (writes are atomic tmp+rename).

### Record format

Each segment is a sequence of records; a record is one history entry:

```
seq        uint64   // per-machine, strictly increasing
host       string
cwd, cmd, exit, t_start, t_end, session
output     []byte   // zstd-compressed, ANSI-stripped copy (see below)
```

Records are serialized, concatenated, zstd-compressed per segment, then
encrypted (XChaCha20-Poly1305) with a key derived from a shared key file
(`~/.config/backscroll/sync.key`, 32 random bytes the user copies to each
machine once — same UX as an SSH key). age-style recipient support could
come later; a symmetric key file keeps v1 dependency-free.

**Redaction happens before export.** The exporter applies the same redact
patterns as `backscroll redact`, plus ignore patterns, so secrets that are
merely *displayed* locally are never written to the sync dir. Exported
output is the ANSI-stripped text (what FTS indexes), not the raw byte
stream — replays (`show --raw`) stay local-only. This halves the size and
avoids shipping terminal control sequences to other machines.

### Import

`backscroll sync import` (or automatic on `backscroll run` start, cheap):

1. Scan `<syncdir>/*/`, skip our own machine-id.
2. For each foreign log, read segments past our high-water mark
   (`import_state(machine_id) -> last_seq`).
3. Insert into the local DB with the foreign machine-id/host attached;
   primary key `(machine_id, seq)` makes re-import a no-op.
4. FTS-index as usual.

Foreign entries then just show up in `search`/`list`/`pick`, tagged with
their host. New filter: `--host laptop` (and `--host local`).

### CLI surface (v1)

```
backscroll sync init <syncdir>     # mint machine-id, write key if absent
backscroll sync export             # append new local entries to our log
backscroll sync import             # pull in foreign entries
backscroll sync status             # per-machine seq / lag / sizes
```

`export`+`import` also run opportunistically at session start/end once the
manual flow has soaked.

## Open questions

1. **Prune/tombstones.** If I `redact` or `prune` locally, should that
   propagate? Leaning: `redact` *must* propagate (append a tombstone record
   that importers apply); `prune` stays local. Tombstones keep the log
   append-only.
2. **Output size caps.** Export the already-capped output (head/tail) only,
   or a smaller cap for foreign entries? Leaning: same cap, it's zstd'd.
3. **Clock skew.** Ordering across machines uses wall time only for
   display; nothing correctness-critical depends on it.
4. **A server, ever?** A dumb blob store speaking the same segment format
   (GET/PUT per machine dir) would work unchanged if people want it. Not
   v1.

## Why not just sync the SQLite file?

Two writers on a synced SQLite file is corruption roulette, and it forces
all-or-nothing trust in the transport. Append-only encrypted logs are
boring, mergeable, and transport-agnostic — you can literally `rsync` them.

---

Feedback welcome — especially from anyone who runs atuin sync or Syncthing
and has scars to share.
