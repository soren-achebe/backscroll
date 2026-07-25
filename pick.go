package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/redact"
)

// cmdPick opens an interactive fuzzy picker (fzf) over recorded commands,
// with a live preview of each command's stored output. On selection the
// output is printed (or paged with --pager, handy inside a tmux popup).
func cmdPick(args []string) error {
	fs := flag.NewFlagSet("pick", flag.ExitOnError)
	n := fs.Int("n", 1000, "max entries offered to the picker")
	pager := fs.Bool("pager", false, "view the selected output in a pager (less -R)")
	raw := fs.Bool("raw", false, "keep ANSI colors / control sequences")
	printID := fs.Bool("print-id", false, "print only the selected entry's id")
	printCmd := fs.Bool("print-cmd", false, "print only the selected command line")
	doRedact := fs.Bool("redact", false, "mask secrets (tokens, keys, passwords) in the output")
	mkFilter := filterFlags(fs)
	fs.Parse(args)
	initial := strings.Join(fs.Args(), " ")

	fzf, err := exec.LookPath("fzf")
	if err != nil {
		return fmt.Errorf("'pick' needs fzf on your PATH (https://github.com/junegunn/fzf — most package managers have it)")
	}

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
		return fmt.Errorf("no recorded commands yet — start a session with: backscroll run")
	}

	var b strings.Builder
	for _, c := range cmds { // newest first; --tiebreak=index keeps that order
		mark := "\x1b[32m✓\x1b[0m"
		if c.ExitCode.Valid && c.ExitCode.Int64 != 0 {
			mark = "\x1b[31m✗\x1b[0m"
		} else if !c.ExitCode.Valid {
			mark = "?"
		}
		cmdText := c.Cmd
		if *doRedact {
			cmdText, _ = redact.String(cmdText, redact.LoadExtra())
		}
		fmt.Fprintf(&b, "%d\t\x1b[2m%-12s\x1b[0m %s %s\n",
			c.ID, fmtAgo(c.StartedAt), mark, oneLine(cmdText, 300))
	}

	self, err := os.Executable()
	if err != nil {
		self = "backscroll"
	}
	preview := shellQuote(self) + " show --raw"
	if *doRedact {
		preview += " --redact"
	}
	preview += " {1}"

	fzfArgs := []string{
		"--ansi",
		"--delimiter=\t",
		"--with-nth=2..",
		"--no-sort",
		"--tiebreak=index",
		"--layout=reverse",
		"--preview=" + preview,
		"--preview-window=down,65%,wrap",
		"--header=enter: view output · esc: cancel",
		"--prompt=backscroll> ",
	}
	if initial != "" {
		fzfArgs = append(fzfArgs, "--query="+initial)
	}

	pick := exec.Command(fzf, fzfArgs...)
	pick.Stdin = strings.NewReader(b.String())
	pick.Stderr = os.Stderr // fzf draws its UI on the tty
	sel, err := pick.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && (ee.ExitCode() == 1 || ee.ExitCode() == 130) {
			return nil // no match / cancelled — not an error
		}
		return fmt.Errorf("fzf: %w", err)
	}
	idStr, _, _ := strings.Cut(strings.TrimSpace(string(sel)), "\t")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}

	switch {
	case *printID:
		fmt.Println(id)
		return nil
	case *printCmd:
		c, err := st.Get(id)
		if err != nil {
			return err
		}
		cmdText := c.Cmd
		if *doRedact {
			cmdText, _ = redact.String(cmdText, redact.LoadExtra())
		}
		fmt.Println(cmdText)
		return nil
	}

	c, err := st.Get(id)
	if err != nil {
		return err
	}
	cmdText := c.Cmd
	if *doRedact {
		cmdText, _ = redact.String(cmdText, redact.LoadExtra())
	}
	header := fmt.Sprintf("\x1b[1m$ %s\x1b[0m\n\x1b[2m# id %d · %s · cwd %s · exit %s · took %s · %s\x1b[0m\n",
		cmdText, c.ID, c.StartedAt.Format("2006-01-02 15:04:05"), c.Cwd, exitStr(*c),
		fmtDur(c.EndedAt.Sub(c.StartedAt)), humanBytes(c.OutputLen))
	out := c.Output
	if *pager {
		*raw = true // the pager handles colors; keep them
	}
	if !*raw {
		out = ansi.Strip(out)
	}
	if *doRedact {
		out, _ = redact.Bytes(out, redact.LoadExtra())
	}

	if *pager {
		return runPager(header, out)
	}
	fmt.Print(header)
	os.Stdout.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

// runPager pipes header+out through $BACKSCROLL_PAGER / $PAGER / less -R.
func runPager(header string, out []byte) error {
	pagerCmd := os.Getenv("BACKSCROLL_PAGER")
	if pagerCmd == "" {
		pagerCmd = os.Getenv("PAGER")
	}
	if pagerCmd == "" {
		pagerCmd = "less -R"
	}
	p := exec.Command("sh", "-c", pagerCmd)
	pw, err := p.StdinPipe()
	if err != nil {
		return err
	}
	p.Stdout = os.Stdout
	p.Stderr = os.Stderr
	if err := p.Start(); err != nil {
		return err
	}
	io.WriteString(pw, header)
	pw.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		io.WriteString(pw, "\n")
	}
	pw.Close()
	return p.Wait()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
