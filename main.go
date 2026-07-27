// backscroll — never lose a command's output again.
//
// Records every command you run (output, exit code, cwd, timing) into a
// local SQLite database, segmented via OSC 133 shell-integration marks,
// and makes it all searchable.
package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/diff"
	"github.com/soren-achebe/backscroll/internal/record"
	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

var version = "dev"

// resolvedVersion returns the release version stamped by goreleaser, falling
// back to the module version recorded by `go install ...@version`.
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return version
}

//go:embed shell/backscroll.zsh
var zshInit string

//go:embed shell/backscroll.bash
var bashInit string

//go:embed shell/backscroll.fish
var fishInit string

//go:embed shell/backscroll.tmux
var tmuxInit string

//go:embed shell/backscroll.kdl
var zellijInit string

//go:embed shell/backscroll.screen
var screenInit string

const usage = `backscroll — never lose a command's output again

Usage:
  backscroll run                 start a recorded shell session
  backscroll init <bash|zsh|fish> print shell-integration snippet
                                 (add: eval "$(backscroll init zsh)" to rc;
                                  fish: backscroll init fish | source);
                                 also: init tmux|zellij|screen picker binds
  backscroll list [-n N]         recent commands; filter with --cwd DIR,
                                 --exit CODE|fail, --since 2h|3d|1w|DATE,
                                 --session ID, --host NAME|local
  backscroll show [id | -N]      full stored output (default: last command)
                                 -2 = one before last, etc.  --raw keeps colors
  backscroll last [-n N]         alias for list
  backscroll search <query>      full-text search over commands + outputs
                                 (same filters as list: --cwd --exit --since)
                                 -C/-A/-B N: grep-style context lines around
                                 each matching output line (-C 0 = matches only)
  backscroll pick [query]        interactive fuzzy picker (needs fzf) with
                                 live output preview; enter = view output.
                                 --pager for tmux/zellij popups, --print-id,
                                 --print-cmd; same filters as list
  backscroll diff <a> [b]        diff two stored outputs (ids or -N offsets);
                                 with one arg, diffs against the previous run
                                 of the same command ("what changed?")
  backscroll export [id|-N ...]  share a command + its output:
                                 --format md (default: paste into an issue),
                                 cast (asciinema v2), json. --details folds
                                 long md output; -o FILE writes to a file
  backscroll stats               database statistics; --by cmd|cwd|exit|host|day
                                 breaks history down (counts, fail%, total
                                 time), same filters as list — e.g.
                                 stats --by cmd --exit fail --since 1w
  backscroll prune --older 30d   delete old entries
  backscroll delete <id>         delete one entry
  backscroll redact <id|-N ...>  permanently mask secrets (tokens, keys,
                                 passwords) in stored entries; --dry-run
                                 previews. show/search/export take --redact
  backscroll sync <subcommand>   cross-machine sync via a shared folder,
                                 end-to-end encrypted, no server needed:
                                 init <dir>, export, import, status
                                 (backscroll sync --help for details)
  backscroll mcp                 Model Context Protocol server over stdio:
                                 lets AI coding agents search your history
                                 and read command outputs. Secrets are
                                 redacted by default (--no-redact opts out)
  backscroll serve               local web UI: browse + search your history
                                 in the browser, colors preserved, diffs.
                                 Loopback-only by default (127.0.0.1:4133);
                                 read-only; --redact masks secrets
  backscroll off | on            pause / resume recording (this session)
  backscroll doctor              check that everything is set up correctly
  backscroll doctor --reindex    rebuild the full-text search index
  backscroll version             print version

Ignore patterns: one Go regexp per line in ~/.config/backscroll/ignore —
matching commands are never stored (see 'backscroll doctor' for the path).
Extra redact patterns: one Go regexp per line in ~/.config/backscroll/redact.

Data lives in ~/.local/share/backscroll/backscroll.db (override: $BACKSCROLL_DB).
Everything is local; nothing ever leaves your machine.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "init":
		err = cmdInit(args)
	case "list", "last":
		err = cmdList(args)
	case "show":
		err = cmdShow(args)
	case "search":
		err = cmdSearch(args)
	case "pick":
		err = cmdPick(args)
	case "diff":
		err = cmdDiff(args)
	case "export":
		err = cmdExport(args)
	case "stats":
		err = cmdStats(args)
	case "prune":
		err = cmdPrune(args)
	case "delete":
		err = cmdDelete(args)
	case "redact":
		err = cmdRedact(args)
	case "sync":
		err = cmdSync(args)
	case "mcp":
		err = cmdMCP(args)
	case "serve", "web":
		err = cmdServe(args)
	case "off", "on":
		err = cmdToggle(cmd)
	case "doctor":
		err = cmdDoctor(args)
	case "version", "--version", "-v":
		fmt.Println("backscroll", resolvedVersion())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "backscroll: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "backscroll:", err)
		os.Exit(1)
	}
}

func openStore() (*store.Store, error) { return store.Open() }

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	head := fs.Int("head-cap", 256<<10, "max bytes kept from start of each command's output")
	tail := fs.Int("tail-cap", 1<<20, "max bytes kept from end of each command's output")
	login := fs.Bool("login", false, "spawn the shell as a login shell (note: a login bash does not read ~/.bashrc)")
	fs.BoolVar(login, "l", *login, "shorthand for --login")
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	err = record.Run(st, *head, *tail, *login)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		st.Close()
		os.Exit(ee.ExitCode()) // mirror the shell's exit status silently
	}
	return err
}

func cmdInit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: backscroll init <bash|zsh|fish|tmux|zellij|screen>")
	}
	switch args[0] {
	case "zsh":
		fmt.Print(zshInit)
	case "bash":
		fmt.Print(bashInit)
	case "fish":
		fmt.Print(fishInit)
	case "tmux":
		fmt.Print(tmuxInit)
	case "zellij":
		fmt.Print(zellijInit)
	case "screen":
		fmt.Print(screenInit)
	default:
		return fmt.Errorf("unsupported target %q (bash, zsh, fish, tmux, zellij, screen)", args[0])
	}
	return nil
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

func fmtAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 02 15:04")
	}
}

func exitStr(c store.Command) string {
	if !c.ExitCode.Valid {
		return "?"
	}
	return strconv.FormatInt(c.ExitCode.Int64, 10)
}

// filterFlags registers the shared list/search filter flags on fs and
// returns a func that builds the store.Filter after fs.Parse.
func filterFlags(fs *flag.FlagSet) func(limit int) (store.Filter, error) {
	sess := fs.Int64("session", 0, "only this session id")
	cwd := fs.String("cwd", "", "only commands run in `dir` (or beneath it); \".\" = current dir")
	exit := fs.String("exit", "", "only exit `code` (number, or \"fail\" for any nonzero)")
	since := fs.String("since", "", "only commands newer than `t` (30m, 2h, 3d, 1w, or 2006-01-02[ 15:04])")
	until := fs.String("until", "", "only commands older than `t` (same forms as --since; exclusive bound)")
	host := fs.String("host", "", "only commands from this `host` (synced machines; \"local\" = this machine)")
	return func(limit int) (store.Filter, error) {
		f := store.Filter{Session: *sess, Limit: limit, Host: *host}
		if *cwd != "" {
			abs, err := filepath.Abs(*cwd)
			if err != nil {
				return f, err
			}
			f.Cwd = filepath.Clean(abs)
		}
		switch {
		case *exit == "":
		case *exit == "fail" || *exit == "nonzero":
			f.Failed = true
		default:
			n, err := strconv.ParseInt(*exit, 10, 64)
			if err != nil {
				return f, fmt.Errorf("--exit: want a number or \"fail\", got %q", *exit)
			}
			f.Exit, f.ExitSet = n, true
		}
		if *since != "" {
			t, err := parseTimeSpec(*since)
			if err != nil {
				return f, fmt.Errorf("--since: %w", err)
			}
			f.Since = t
		}
		if *until != "" {
			t, err := parseTimeSpec(*until)
			if err != nil {
				return f, fmt.Errorf("--until: %w", err)
			}
			f.Until = t
		}
		return f, nil
	}
}

// splitFlags separates registered flags (with their values) from positional
// words so filters can trail the query: `search boom --since 1d` works the
// same as `search --since 1d boom`. A dash-word that is not a registered
// flag stays positional (queries may contain dashes); everything after a
// bare "--" is positional.
func splitFlags(fs *flag.FlagSet, args []string) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if fl := fs.Lookup(name); fl != nil {
				flags = append(flags, a)
				if !strings.Contains(a, "=") {
					b, ok := fl.Value.(interface{ IsBoolFlag() bool })
					if !(ok && b.IsBoolFlag()) && i+1 < len(args) {
						i++
						flags = append(flags, args[i])
					}
				}
				continue
			}
		}
		pos = append(pos, a)
	}
	return flags, pos
}

// parseTimeSpec accepts a relative age (30m, 2h, 3d, 1w — d/w extend Go
// durations) or an absolute local date/time (2006-01-02, 2006-01-02 15:04).
// Relative forms mean "that long ago", so --until 2h reads "older than two
// hours" and --since 2h "newer than two hours".
func parseTimeSpec(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	ds := s
	if n := len(s); n > 1 {
		switch s[n-1] {
		case 'd':
			if v, err := strconv.ParseFloat(s[:n-1], 64); err == nil {
				return time.Now().Add(-time.Duration(v * 24 * float64(time.Hour))), nil
			}
		case 'w':
			if v, err := strconv.ParseFloat(s[:n-1], 64); err == nil {
				return time.Now().Add(-time.Duration(v * 7 * 24 * float64(time.Hour))), nil
			}
		}
	}
	if d, err := time.ParseDuration(ds); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("want 30m, 2h, 3d, 1w or 2006-01-02[ 15:04], got %q", s)
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	n := fs.Int("n", 20, "number of entries")
	mkFilter := filterFlags(fs)
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	f, err := mkFilter(*n)
	if err != nil {
		return err
	}
	cmds, err := st.List(f)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		if f.Active() {
			fmt.Println("no commands match those filters")
		} else {
			fmt.Println("no recorded commands yet — start a session with: backscroll run")
		}
		return nil
	}
	color := term.IsTerminal(int(os.Stdout.Fd()))
	for i := len(cmds) - 1; i >= 0; i-- {
		c := cmds[i]
		mark := "✓"
		if c.ExitCode.Valid && c.ExitCode.Int64 != 0 {
			mark = "✗"
		}
		line := fmt.Sprintf("%5d  %s %-12s %8s  %8s  %s%s",
			c.ID, mark, fmtAgo(c.StartedAt), fmtDur(c.EndedAt.Sub(c.StartedAt)),
			humanBytes(c.OutputLen), hostTag(c, color), oneLine(c.Cmd, 80))
		if color && mark == "✗" {
			line = "\x1b[31m" + line + "\x1b[0m"
		}
		fmt.Println(line)
	}
	return nil
}

// hostTag renders a "[host] " prefix for entries imported from another
// machine via sync ("" for local ones).
func hostTag(c store.Command, color bool) string {
	if c.Local() {
		return ""
	}
	if color {
		return "\x1b[35m[" + c.Host + "]\x1b[0m "
	}
	return "[" + c.Host + "] "
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	raw := fs.Bool("raw", false, "keep ANSI colors / control sequences")
	quiet := fs.Bool("q", false, "output only, no header")
	doRedact := fs.Bool("redact", false, "mask secrets (tokens, keys, passwords) in the output")
	// allow "show -2" style (bare -N = Nth from last) and flags in any
	// position ("show 5 -q"): split flags from positionals ourselves.
	// All show flags are booleans, so this is safe.
	var target int64 = -1
	var flags, pos []string
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			if n, err := strconv.ParseInt(a, 10, 64); err == nil {
				target = n
				continue
			}
			flags = append(flags, a)
			continue
		}
		pos = append(pos, a)
	}
	fs.Parse(append(flags, pos...))
	if fs.NArg() > 0 {
		n, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			return fmt.Errorf("bad id %q", fs.Arg(0))
		}
		target = n
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.Get(target)
	if err != nil {
		return fmt.Errorf("not found (%v)", err)
	}
	if *doRedact {
		c.Cmd, _ = redact.String(c.Cmd, redact.LoadExtra())
	}
	if !*quiet {
		fmt.Printf("\x1b[1m$ %s\x1b[0m\n", c.Cmd)
		from := ""
		if !c.Local() {
			from = " · from " + c.Host
		}
		fmt.Printf("\x1b[2m# id %d · %s · cwd %s · exit %s · took %s · %s%s\x1b[0m\n",
			c.ID, c.StartedAt.Format("2006-01-02 15:04:05"), c.Cwd, exitStr(*c),
			fmtDur(c.EndedAt.Sub(c.StartedAt)), humanBytes(c.OutputLen), from)
	}
	out := c.Output
	if !*raw {
		out = ansi.Strip(out)
	}
	if *doRedact {
		out, _ = redact.Bytes(out, redact.LoadExtra())
	}
	os.Stdout.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	n := fs.Int("n", 20, "max results")
	doRedact := fs.Bool("redact", false, "mask secrets in displayed commands and snippets")
	ctxC := fs.Int("C", -1, "show N lines of context around each matching output line (like grep -C)")
	ctxA := fs.Int("A", -1, "show N lines after each matching output line (like grep -A)")
	ctxB := fs.Int("B", -1, "show N lines before each matching output line (like grep -B)")
	mkFilter := filterFlags(fs)
	flagArgs, posArgs := splitFlags(fs, args)
	fs.Parse(flagArgs)
	if len(posArgs) == 0 {
		return fmt.Errorf("usage: backscroll search <query> [flags]")
	}
	query := strings.Join(posArgs, " ")
	// trigram tokenizer: quote the query so users can type natural strings
	q := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	f, err := mkFilter(*n)
	if err != nil {
		return err
	}
	res, err := st.Search(q, f)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("no matches")
		return nil
	}
	var extra []*regexp.Regexp
	if *doRedact {
		extra = redact.LoadExtra()
	}
	ctxMode := *ctxC >= 0 || *ctxA >= 0 || *ctxB >= 0
	before, after := *ctxB, *ctxA
	if before < 0 {
		before = max(*ctxC, 0)
	}
	if after < 0 {
		after = max(*ctxC, 0)
	}
	for _, c := range res {
		cmd, snip := c.Cmd, strings.TrimSpace(c.Snippet)
		if *doRedact {
			cmd, _ = redact.String(cmd, extra)
			// drop the FTS highlight escapes first: a secret split by
			// them would otherwise slip past the redaction patterns
			snip, _ = redact.String(string(ansi.Strip([]byte(snip))), extra)
		}
		fmt.Printf("\x1b[1m%5d\x1b[0m  %s  exit %s  %s\x1b[36m%s\x1b[0m\n",
			c.ID, fmtAgo(c.StartedAt), exitStr(c), hostTag(c, true), oneLine(cmd, 70))
		if ctxMode {
			printSearchContext(st, c, query, before, after, *doRedact, extra)
			continue
		}
		if snip != "" {
			fmt.Printf("       %s\n", oneLine(snip, 100))
		}
	}
	fmt.Printf("\n(%d results — `backscroll show <id>` for full output)\n", len(res))
	return nil
}

// printSearchContext renders grep-style context hunks from a result's
// stored plain output under its search headline (search -A/-B/-C).
// Redaction runs on the whole text BEFORE matching and highlighting, so
// highlight escapes can never split a secret past the redact patterns.
func printSearchContext(st *store.Store, c store.Command, query string, before, after int, doRedact bool, extra []*regexp.Regexp) {
	const maxHunks = 5
	text, err := st.Plain(c.ID)
	if err != nil || text == "" {
		return
	}
	if doRedact {
		text, _ = redact.String(text, extra)
	}
	hunks, shown, total := grepContext(text, query, before, after, maxHunks)
	for hi, h := range hunks {
		if hi > 0 {
			fmt.Println("          \x1b[2m--\x1b[0m")
		}
		for i, ln := range h.Lines {
			lineNo := h.Start + i + 1
			if h.IsMatch[i] {
				clipped := clipLine(ln, matchOffset(ln, query), 140)
				fmt.Printf("       \x1b[32m%5d\x1b[0m:%s\n", lineNo,
					highlightMatches(clipped, query, "\x1b[1;31m", "\x1b[0m"))
			} else {
				fmt.Printf("       \x1b[2m%5d-%s\x1b[0m\n", lineNo, clipLine(ln, 0, 140))
			}
		}
	}
	if total > shown {
		fmt.Printf("       \x1b[2m(%d more matching lines — backscroll show %d | grep …)\x1b[0m\n", total-shown, c.ID)
	}
}

func parseDur(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	older := fs.String("older", "90d", "delete entries older than this (e.g. 30d, 12h)")
	fs.Parse(args)
	d, err := parseDur(*older)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.Prune(d)
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d entries older than %s\n", n, *older)
	return nil
}

func cmdDelete(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: backscroll delete <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Delete(id); err != nil {
		return err
	}
	fmt.Printf("deleted entry %d\n", id)
	return nil
}

// cmdRedact permanently scrubs secrets from stored entries: the command
// line, the raw output, and the search index are all rewritten in place.
func cmdRedact(args []string) error {
	fs := flag.NewFlagSet("redact", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "report what would be masked without changing the store")
	var flags, pos []string
	for _, a := range args {
		if _, err := strconv.ParseInt(a, 10, 64); err == nil {
			pos = append(pos, a) // ids and -N offsets, flags may interleave
			continue
		}
		flags = append(flags, a)
	}
	fs.Parse(flags)
	pos = append(pos, fs.Args()...)
	if len(pos) == 0 {
		return fmt.Errorf("usage: backscroll redact [--dry-run] <id|-N> [...]\n" +
			"permanently masks secrets in stored entries (see also: show --redact, export --redact)")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	extra := redact.LoadExtra()
	for _, p := range pos {
		n, _ := strconv.ParseInt(p, 10, 64)
		c, err := st.Get(n)
		if err != nil {
			return fmt.Errorf("%s: not found (%v)", p, err)
		}
		newCmd, hitsCmd := redact.String(c.Cmd, extra)
		newOut, hitsOut := redact.Bytes(c.Output, extra)
		hits := hitsCmd + hitsOut
		if hits == 0 {
			fmt.Printf("entry %d: nothing to redact\n", c.ID)
			continue
		}
		if *dry {
			fmt.Printf("entry %d: would mask %d secret(s)\n", c.ID, hits)
			continue
		}
		plain := string(ansi.Strip(newOut))
		if err := st.UpdateOutput(c.ID, newCmd, newOut, plain); err != nil {
			return fmt.Errorf("entry %d: %v", c.ID, err)
		}
		fmt.Printf("entry %d: masked %d secret(s)\n", c.ID, hits)
	}
	return nil
}

func cmdToggle(which string) error {
	if os.Getenv("BACKSCROLL_ACTIVE") == "" {
		return fmt.Errorf("not inside a backscroll session (start one with: backscroll run)")
	}
	// The recorder watches the PTY stream, so an in-band private OSC
	// sequence is all it takes — no sockets, no signals.
	fmt.Printf("\x1b]6973;rec=%s\x07", which)
	if which == "off" {
		fmt.Println("backscroll: recording paused for this session (resume: backscroll on)")
	} else {
		fmt.Println("backscroll: recording resumed")
	}
	return nil
}

// rcFor maps $SHELL to its rc file and the hook line to add. A "" rc
// means the shell needs no snippet at all (it emits OSC 133 natively).
func rcFor(shell string) (rc string, hook string) {
	home, _ := os.UserHomeDir()
	base := filepath.Base(shell)
	switch {
	case base == "nu" || base == "nushell":
		return "", "" // nushell emits OSC 133 + OSC 7 by default
	case strings.Contains(shell, "zsh"):
		return home + "/.zshrc", `eval "$(backscroll init zsh)"`
	case strings.Contains(shell, "fish"):
		return home + "/.config/fish/config.fish", `backscroll init fish | source`
	default:
		return home + "/.bashrc", `eval "$(backscroll init bash)"`
	}
}

func cmdDoctor(args []string) error {
	if len(args) == 1 && args[0] == "--reindex" {
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		fmt.Println("rebuilding full-text index from stored outputs…")
		if err := st.RebuildFTS(); err != nil {
			return err
		}
		fmt.Println("done.")
		return nil
	}
	ok := func(b bool) string {
		if b {
			return "\x1b[32m✓\x1b[0m"
		}
		return "\x1b[31m✗\x1b[0m"
	}
	fmt.Println("backscroll doctor")
	fmt.Println("─────────────────")
	fmt.Printf("version            : %s\n", resolvedVersion())

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	fmt.Printf("shell              : %s\n", shell)

	inSession := os.Getenv("BACKSCROLL_ACTIVE") != ""
	fmt.Printf("%s inside a backscroll session", ok(inSession))
	if !inSession {
		fmt.Print("  → start one: backscroll run")
	}
	fmt.Println()

	hooked := os.Getenv("BACKSCROLL_HOOKED") != ""
	rc, hookLine := rcFor(shell)
	if inSession {
		fmt.Printf("%s shell integration active in this session\n", ok(hooked || rc == ""))
	}
	rcHasHook := rc == "" // zero-config shells count as hooked
	if rc == "" {
		fmt.Printf("%s shell emits OSC 133 natively — zero-config, no snippet needed\n", ok(true))
	} else {
		if b, err := os.ReadFile(rc); err == nil {
			rcHasHook = strings.Contains(string(b), "backscroll init")
		}
		fmt.Printf("%s rc file references backscroll (%s)", ok(rcHasHook), rc)
		if !rcHasHook {
			fmt.Printf("\n    → add:  %s", hookLine)
		}
		fmt.Println()
	}

	st, err := openStore()
	fmt.Printf("%s database opens (%s)", ok(err == nil), store.DefaultPath())
	if err != nil {
		fmt.Printf("  → %v\n", err)
		return nil
	}
	fmt.Println()
	defer st.Close()
	s, serr := st.Stats()
	fmt.Printf("%s database queries work", ok(serr == nil))
	if serr == nil {
		fmt.Printf("  (%d commands, %d sessions, db %s)", s.Commands, s.Sessions, humanBytes(s.DBBytes))
	} else {
		fmt.Printf("  → %v", serr)
	}
	fmt.Println()

	ig := record.IgnoreFile()
	if _, err := os.Stat(ig); err == nil {
		n := len(record.LoadIgnore())
		fmt.Printf("· ignore patterns   : %d active (%s)\n", n, ig)
	} else {
		fmt.Printf("· ignore patterns   : none (create %s — one Go regexp per line)\n", ig)
	}
	rf := redact.File()
	if _, err := os.Stat(rf); err == nil {
		fmt.Printf("· redact patterns   : %d extra (%s) + built-ins\n", len(redact.LoadExtra()), rf)
	} else {
		fmt.Printf("· redact patterns   : built-ins only (extras: %s — one Go regexp per line)\n", rf)
	}

	if inSession && (hooked || rc == "") && err == nil && serr == nil {
		fmt.Println("\nall good — your commands are being recorded.")
	} else if !inSession && rcHasHook && err == nil {
		fmt.Println("\nsetup looks fine — run `backscroll run` to start recording.")
	}
	return nil
}

// cmdDiff diffs the stored outputs of two commands. With a single target
// it diffs against the previous run of the same command line. Like
// diff(1): exit 0 if identical, 1 if different.
func cmdDiff(args []string) error {
	context := 3
	var targets []int64
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-U" || a == "--context":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", a)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fmt.Errorf("bad context %q", args[i+1])
			}
			context = n
			i++
		case strings.HasPrefix(a, "-U="):
			n, err := strconv.Atoi(a[3:])
			if err != nil || n < 0 {
				return fmt.Errorf("bad context %q", a[3:])
			}
			context = n
		default:
			n, err := strconv.ParseInt(a, 10, 64)
			if err != nil || n == 0 {
				return fmt.Errorf("usage: backscroll diff [-U n] <a> [b]   (ids or -N offsets)")
			}
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		targets = []int64{-1}
	}
	if len(targets) > 2 {
		return fmt.Errorf("diff takes at most two targets")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	var older, newer *store.Command
	if len(targets) == 2 {
		older, err = st.Get(targets[0])
		if err != nil {
			return fmt.Errorf("not found (%v)", err)
		}
		newer, err = st.Get(targets[1])
		if err != nil {
			return fmt.Errorf("not found (%v)", err)
		}
	} else {
		newer, err = st.Get(targets[0])
		if err != nil {
			return fmt.Errorf("not found (%v)", err)
		}
		older, err = st.PrevSame(newer.ID, newer.Cmd)
		if err != nil {
			return fmt.Errorf("no previous run of %q found", newer.Cmd)
		}
	}

	label := func(c *store.Command) string {
		return fmt.Sprintf("#%d $ %s  (%s, exit %s)",
			c.ID, c.Cmd, c.StartedAt.Format("2006-01-02 15:04:05"), exitStr(*c))
	}
	ops := diff.Lines(string(ansi.Strip(older.Output)), string(ansi.Strip(newer.Output)))
	color := term.IsTerminal(int(os.Stdout.Fd()))
	u := diff.Unified(ops, label(older), label(newer), context, color)
	if u == "" {
		fmt.Fprintf(os.Stderr, "backscroll: outputs of #%d and #%d are identical\n", older.ID, newer.ID)
		return nil
	}
	os.Stdout.WriteString(u)
	os.Exit(1) // like diff(1): differences found
	return nil
}
