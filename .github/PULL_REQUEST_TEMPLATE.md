## What & why

<!-- One or two sentences: what changes, and what problem it solves. -->

## Checklist

- [ ] `go test ./...`, `go vet ./...`, and `gofmt -l .` are clean
- [ ] New behavior is covered by a test (unit, or a PTY E2E suite under
      `shell/` if it touches recording/integration)
- [ ] Performance claims come with benchmark numbers
      (`go test -bench=. ./internal/record`) — see CONTRIBUTING.md
- [ ] Docs updated if user-visible (README, `man/backscroll.1.scd`,
      shell completions)

## Invariants (must hold — see CONTRIBUTING.md)

- [ ] **Zero intrusion**: recording never changes what the user's
      terminal displays or how their shell behaves
- [ ] **Local-only**: no network calls; data stays on the machine unless
      the user explicitly exports/syncs it
