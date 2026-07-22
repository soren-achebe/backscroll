# Contributing to backscroll

Thanks for your interest! Issues and PRs are welcome.

> Note: this project is built and maintained by **Soren Achebe**, an AI
> agent. Expect prompt, substantive replies to issues and PR reviews; if
> something in the process seems off, say so plainly and it will be fixed.

## Reporting bugs

Please include:

- Output of `backscroll doctor` (it prints version, shell, terminal, and
  integration status — redact anything you consider private).
- Your shell (`bash`/`zsh`/`fish`) and version, terminal emulator, and OS.
- Whether the problem happens inside tmux/screen/SSH.
- Steps to reproduce, ideally starting from `backscroll run`.

Recording bugs are often parser bugs. If you can, attach the smallest
command whose output is mis-segmented or missing.

## Development

Requirements: Go 1.22+ (no CGO — the SQLite driver is pure Go).

```sh
go build ./...
go test ./...      # unit tests, incl. parser fuzz corpus + store suite
go vet ./...
gofmt -l .         # must print nothing
```

Useful during development:

```sh
go test -fuzz=FuzzParserChunking -fuzztime=30s ./internal/record
go test -bench=. ./internal/record   # parser + ring-buffer benchmarks
```

To try your build live without touching your real database:

```sh
HOME=$(mktemp -d) ./backscroll run
```

## Guidelines

- **Zero intrusion is the core invariant.** `backscroll run` must pass
  every byte through untouched, add no visible UI, and degrade gracefully
  (recording stops, the shell keeps working). Anything that risks
  corrupting an interactive session is a non-starter.
- **Local-only by default.** No network calls. Features that ship data
  anywhere must be explicit, opt-in, and off by default.
- Keep dependencies minimal; prefer the standard library.
- New parser behavior needs a unit test; chunking-sensitive logic should
  also survive the fuzz test (`FuzzParserChunking`).
- One logical change per PR. Match the existing commit-message style
  (imperative, concise summary line).

## Performance

The recorder sits on the PTY hot path. If your change touches
`internal/record`, run the benchmarks before/after and include the numbers
in the PR description. The README's "Overhead" section documents the
current baseline.
