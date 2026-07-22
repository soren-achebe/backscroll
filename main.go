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
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/record"
	"github.com/soren-achebe/backscroll/internal/store"
)

var version = "dev"

//go:embed shell/backscroll.zsh
var zshInit string

//go:embed shell/backscroll.bash
var bashInit string

const usage = `backscroll — never lose a command's output again

Usage:
  backscroll run                 start a recorded shell session
  backscroll init <bash|zsh>     print shell-integration snippet
                                 (add: eval "$(backscroll init zsh)" to rc)
  backscroll list [-n N]         recent commands
  backscroll show [id | -N]      full stored output (default: last command)
                                 -2 = one before last, etc.  --raw keeps colors
  backscroll last [-n N]         alias for list
  backscroll search <query>      full-text search over commands + outputs
  backscroll stats               database statistics
  backscroll prune --older 30d   delete old entries
  backscroll delete <id>         delete one entry
  backscroll version             print version

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
	case "stats":
		err = cmdStats()
	case "prune":
		err = cmdPrune(args)
	case "delete":
		err = cmdDelete(args)
	case "version", "--version", "-v":
		fmt.Println("backscroll", version)
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
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	err = record.Run(st, *head, *tail)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		st.Close()
		os.Exit(ee.ExitCode()) // mirror the shell's exit status silently
	}
	return err
}

func cmdInit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: backscroll init <bash|zsh>")
	}
	switch args[0] {
	case "zsh":
		fmt.Print(zshInit)
	case "bash":
		fmt.Print(bashInit)
	default:
		return fmt.Errorf("unsupported shell %q (bash and zsh for now)", args[0])
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

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	n := fs.Int("n", 20, "number of entries")
	sess := fs.Int64("session", 0, "filter by session id")
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	cmds, err := st.List(*n, *sess)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		fmt.Println("no recorded commands yet — start a session with: backscroll run")
		return nil
	}
	color := term.IsTerminal(int(os.Stdout.Fd()))
	for i := len(cmds) - 1; i >= 0; i-- {
		c := cmds[i]
		mark := "✓"
		if c.ExitCode.Valid && c.ExitCode.Int64 != 0 {
			mark = "✗"
		}
		line := fmt.Sprintf("%5d  %s %-12s %8s  %8s  %s",
			c.ID, mark, fmtAgo(c.StartedAt), fmtDur(c.EndedAt.Sub(c.StartedAt)),
			humanBytes(c.OutputLen), oneLine(c.Cmd, 80))
		if color && mark == "✗" {
			line = "\x1b[31m" + line + "\x1b[0m"
		}
		fmt.Println(line)
	}
	return nil
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
	// allow "show -2" style: rewrite bare -N to N before flag parsing
	var target int64 = -1
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			if n, err := strconv.ParseInt(a, 10, 64); err == nil {
				target = n
				continue
			}
		}
		rest = append(rest, a)
	}
	fs.Parse(rest)
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
	if !*quiet {
		fmt.Printf("\x1b[1m$ %s\x1b[0m\n", c.Cmd)
		fmt.Printf("\x1b[2m# id %d · %s · cwd %s · exit %s · took %s · %s\x1b[0m\n",
			c.ID, c.StartedAt.Format("2006-01-02 15:04:05"), c.Cwd, exitStr(*c),
			fmtDur(c.EndedAt.Sub(c.StartedAt)), humanBytes(c.OutputLen))
	}
	out := c.Output
	if !*raw {
		out = ansi.Strip(out)
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
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: backscroll search <query>")
	}
	query := strings.Join(fs.Args(), " ")
	// trigram tokenizer: quote the query so users can type natural strings
	q := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.Search(q, *n)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, c := range res {
		fmt.Printf("\x1b[1m%5d\x1b[0m  %s  exit %s  \x1b[36m%s\x1b[0m\n",
			c.ID, fmtAgo(c.StartedAt), exitStr(c), oneLine(c.Cmd, 70))
		if s := strings.TrimSpace(c.Snippet); s != "" {
			fmt.Printf("       %s\n", oneLine(s, 100))
		}
	}
	fmt.Printf("\n(%d results — `backscroll show <id>` for full output)\n", len(res))
	return nil
}

func cmdStats() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("commands recorded : %d\n", s.Commands)
	fmt.Printf("sessions          : %d\n", s.Sessions)
	fmt.Printf("raw output stored : %s\n", humanBytes(s.RawBytes))
	fmt.Printf("database size     : %s\n", humanBytes(s.DBBytes))
	if !s.FirstAt.IsZero() {
		fmt.Printf("oldest entry      : %s\n", s.FirstAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("database path     : %s\n", store.DefaultPath())
	return nil
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
