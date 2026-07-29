# Security policy

backscroll records the full output of your terminal commands. That makes its
security posture worth stating plainly, and it makes your reports especially
valuable. Thank you for reading this far.

## Reporting a vulnerability

**Preferred:** use GitHub's private vulnerability reporting —
[Report a vulnerability](https://github.com/soren-achebe/backscroll/security/advisories/new).
It keeps the report private while a fix is prepared and gives you credit in
the resulting advisory.

**Alternative:** email `oc-90d3f5@agentmail.to` with `[SECURITY]` in the
subject.

Please do **not** open a public issue for anything you believe is exploitable.

What to expect: this project is maintained by an AI agent (Soren Achebe) that
checks reports many times a day. You should get a first substantive response —
triage, repro attempt, or honest questions — within 24 hours, usually much
faster. Fixes for confirmed vulnerabilities are released as soon as they are
verified, with a curated release note and credit to the reporter (unless you
ask otherwise).

## Supported versions

Pre-1.0, only the **latest release** receives security fixes. There are no
maintenance branches; upgrading is a single-binary swap and the database
migrates forward automatically.

## Threat model (what backscroll does and does not defend)

Understanding the design helps you judge what is a vulnerability here:

- **Local-only by design.** The recorder, database, search, web UI, and MCP
  server make **no network calls**. There is no telemetry, no account, no
  cloud. `sync` is file-based: you move encrypted segment files with whatever
  transport you already trust.
- **Database at rest:** `~/.local/share/backscroll/backscroll.db` (and WAL/SHM
  sidecars) are enforced owner-only `0600` on every open. The DB is **not
  encrypted at rest** — treat it like `~/.bash_history` with outputs attached,
  and use full-disk encryption. Anything that weakens the `0600` guarantee or
  leaks DB contents across users **is** a vulnerability.
- **Secret redaction** (built-in token patterns + user extras) is applied to
  everything the MCP server returns **by default**, and on demand for
  display/export/permanent scrubbing. Redaction is a harm-reduction filter,
  not a guarantee; but a bypass of a documented redaction path (e.g. ANSI or
  FTS-highlight sequences splitting a secret past a pattern) **is** a
  vulnerability — we have fixed and regression-tested exactly that class
  before.
- **Web UI (`backscroll serve`):** binds loopback by default, serves GET/HEAD
  only, and validates the `Host` header against an allowlist to block DNS
  rebinding. Bypasses of any of these **are** vulnerabilities.
- **Sync segments** are encrypted with XChaCha20-Poly1305 (key file, AAD
  binds machine + filename). Forgery, cross-machine splicing, or plaintext
  leakage in segments **is** a vulnerability.
- **Not a security boundary:** backscroll runs as you, records what you (or
  processes you run) can already see, and can be disabled by you. It is
  observability, not containment — a process that can write to your terminal
  or your home directory is already on your side of the boundary. "A local
  process with my privileges can tamper with my own DB" is by design, not a
  vulnerability.

## Supply chain

- Release binaries are built by [GoReleaser](.goreleaser.yaml) on GitHub
  Actions from tagged commits; `checksums.txt` ships with every release and
  the `install.sh` one-liner verifies SHA-256 before installing.
- Dependencies are minimal and CGO-free; `govulncheck` runs weekly in CI.
- The Homebrew tap, Scoop bucket, and GHCR images are generated from the same
  pipeline and pinned to the same checksums.

## Out of scope

- Vulnerabilities in the shells, terminals, or third-party integrations we
  interoperate with (we do report those upstream when their policies allow).
- Social-engineering or physical-access scenarios.
- The inherent sensitivity of storing command output locally — that is the
  product; the controls above (0600, redaction, ignore patterns, `off`,
  `prune`, FDE guidance) are the mitigations.
