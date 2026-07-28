// backscroll exec — record a single command, no shell session needed.
//
// `backscroll exec -- make test` runs the command with stdin/stdout/
// stderr passed straight through (tee-style) and stores the combined
// output, exit code, cwd and timing in the database, exactly like a
// command recorded inside `backscroll run`. Built for cron jobs, CI
// steps, deploys and long builds: the places where output scrolls away
// (or lands in root's mailbox) and you want it back next week.
//
// Transparency contract: exec mirrors the child's exit code (signal
// deaths as 128+n), forwards SIGINT/SIGTERM, and never lets a recording
// problem stop or fail the command itself — storage errors are a
// warning on stderr, nothing more.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/record"
	"github.com/soren-achebe/backscroll/internal/store"
)

const execUsage = `usage: backscroll exec [flags] <command> [args...]

Run a command and record its output, exit code, cwd and timing —
no recorded shell session required. Output passes through untouched;
the exit code is mirrored. Made for cron jobs, CI steps and builds.

  --quiet          don't pass output through; just record it
  --head-cap N     max bytes kept from start of output (default 256K)
  --tail-cap N     max bytes kept from end of output (default 1M)

Flags after the command belong to the command:
  backscroll exec make -j4 test        # -j4 goes to make
Use your shell explicitly for pipes/redirection:
  backscroll exec sh -c 'du -sh * | sort -h'
`

func cmdExec(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	head := fs.Int("head-cap", 256<<10, "max bytes kept from start of the output")
	tail := fs.Int("tail-cap", 1<<20, "max bytes kept from end of the output")
	quiet := fs.Bool("quiet", false, "don't pass output through, just record it")
	fs.Usage = func() { fmt.Fprint(os.Stderr, execUsage) }
	fs.Parse(args)
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, execUsage)
		os.Exit(2)
	}

	// A broken database must never stop the command from running:
	// exec is transparent first, a recorder second.
	st, serr := openStore()
	if serr != nil {
		fmt.Fprintln(os.Stderr, "backscroll: warning: not recording:", serr)
		st = nil
	}

	code, rerr := runExec(st, argv, *head, *tail, *quiet, os.Stdout, os.Stderr)
	if st != nil {
		st.Close()
	}
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "backscroll: warning: not recorded:", rerr)
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// runExec runs argv with output tapped into a capped buffer, then
// records the result (unless st is nil or the command matches an
// ignore pattern). Returns the child's exit code (128+n for signal
// deaths, 127 if it could not be started) and any recording error.
// The child's exit code is authoritative regardless of recording.
func runExec(st *store.Store, argv []string, headCap, tailCap int, quiet bool,
	stdout, stderr io.Writer) (int, error) {

	buf := record.NewCapBuf(headCap, tailCap)
	var mu sync.Mutex
	tap := func(dst io.Writer) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			buf.Write(p)
			mu.Unlock()
			if quiet {
				return len(p), nil
			}
			return dst.Write(p)
		})
	}

	cwd, _ := os.Getwd()
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin = os.Stdin
	c.Stdout = tap(stdout)
	c.Stderr = tap(stderr)
	// Backgrounded grandchildren (daemons, `foo &`) inherit the output
	// pipes; without a deadline Wait would block until *they* exit —
	// a Ctrl-C'd `sh -c 'sleep 100'` took the whole 100s to come back
	// in testing. Give stragglers a short grace after the child exits,
	// then let go.
	c.WaitDelay = 2 * time.Second

	// Stay alive through Ctrl-C so the child's final output gets
	// recorded; forward the signal (usually a duplicate — the terminal
	// already signals the whole foreground group, but cron/systemd
	// deliver to us alone).
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)

	startedAt := time.Now()
	code := 0
	if err := c.Start(); err != nil {
		// Startup failures (not found, not executable) are worth
		// recording too: cron's "command not found" is exactly the
		// kind of output people go digging for.
		code = 127
		msg := fmt.Sprintf("backscroll exec: %v\n", err)
		if !quiet {
			io.WriteString(stderr, msg)
		}
		buf.Write([]byte(msg))
	} else {
		done := make(chan struct{})
		go func() {
			for {
				select {
				case s := <-sigc:
					_ = c.Process.Signal(s)
				case <-done:
					return
				}
			}
		}()
		werr := c.Wait()
		close(done)
		code = exitCodeOf(werr)
	}
	endedAt := time.Now()

	if st == nil {
		return code, nil
	}
	cmdline := shellJoin(argv)
	if record.Ignored(record.LoadIgnore(), cmdline) {
		return code, nil
	}
	raw := buf.Bytes()
	plain := string(ansi.Strip(raw))
	sessID, err := st.NewSession("exec", os.Getenv("TERM"))
	if err != nil {
		return code, err
	}
	if err := st.AddCommand(sessID, cmdline, cwd, code, true,
		startedAt, endedAt, raw, buf.Truncated(), plain); err != nil {
		return code, err
	}
	return code, nil
}

// exitCodeOf maps a Wait error to the code exec should mirror:
// 0 on success, the child's code on plain exits, 128+n for signal
// deaths (shell convention), 127 if Wait failed some other way.
func exitCodeOf(werr error) int {
	if werr == nil {
		return 0
	}
	if errors.Is(werr, exec.ErrWaitDelay) {
		// The child itself exited 0; only the pipe stragglers were
		// cut off by WaitDelay.
		return 0
	}
	ee, ok := werr.(*exec.ExitError)
	if !ok {
		return 127
	}
	code := ee.ExitCode()
	if code == -1 {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
	}
	return code
}

// shellJoin renders argv as a copy-pasteable shell command line:
// words needing quoting get single quotes (POSIX quote escaping).
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = execQuote(a)
	}
	return strings.Join(parts, " ")
}

func execQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$&|;<>()[]{}*?~#`!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
