package record

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/store"
)

// capBuf keeps the first headCap bytes and the last tailCap bytes of a
// stream, so huge outputs stay bounded but both the beginning and the
// (usually most interesting) end survive.
type capBuf struct {
	headCap, tailCap int
	head             []byte
	tail             []byte // ring
	tailStart        int
	tailLen          int
	total            int64
}

func newCapBuf(headCap, tailCap int) *capBuf {
	return &capBuf{headCap: headCap, tailCap: tailCap}
}

// isShellExit reports whether cmd is a plain shell terminator: `exit`,
// `logout`, or `exit <n>` — the only commands whose C mark can never be
// followed by a D mark because the shell is gone.
func isShellExit(cmd string) bool {
	f := strings.Fields(cmd)
	switch len(f) {
	case 1:
		return f[0] == "exit" || f[0] == "logout"
	case 2:
		if f[0] != "exit" {
			return false
		}
		for _, r := range f[1] {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}

func (c *capBuf) Write(p []byte) {
	c.total += int64(len(p))
	if len(c.head) < c.headCap {
		n := c.headCap - len(c.head)
		if n > len(p) {
			n = len(p)
		}
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	if len(p) == 0 {
		return
	}
	if c.tail == nil {
		c.tail = make([]byte, c.tailCap)
	}
	if len(p) >= c.tailCap {
		copy(c.tail, p[len(p)-c.tailCap:])
		c.tailStart, c.tailLen = 0, c.tailCap
		return
	}
	// copy in at most two segments (up to the end of the ring, then wrap)
	for len(p) > 0 {
		idx := (c.tailStart + c.tailLen) % c.tailCap
		n := c.tailCap - idx
		if n > len(p) {
			n = len(p)
		}
		copy(c.tail[idx:idx+n], p[:n])
		if c.tailLen+n <= c.tailCap {
			c.tailLen += n
		} else {
			over := c.tailLen + n - c.tailCap
			c.tailLen = c.tailCap
			c.tailStart = (c.tailStart + over) % c.tailCap
		}
		p = p[n:]
	}
}

func (c *capBuf) Truncated() bool { return c.total > int64(len(c.head)+c.tailLen) }

func (c *capBuf) Bytes() []byte {
	out := make([]byte, 0, len(c.head)+c.tailLen+64)
	out = append(out, c.head...)
	if c.Truncated() {
		out = append(out, []byte(fmt.Sprintf("\n… [backscroll: %d bytes truncated] …\n", c.total-int64(len(c.head)+c.tailLen)))...)
	}
	for i := 0; i < c.tailLen; i++ {
		out = append(out, c.tail[(c.tailStart+i)%c.tailCap])
	}
	return out
}

// Run spawns the user's shell on a PTY, proxies all IO untouched, and
// records per-command output segments to the store.
func Run(st *store.Store, headCap, tailCap int, login bool) error {
	if os.Getenv("BACKSCROLL_ACTIVE") != "" {
		return fmt.Errorf("already inside a backscroll session")
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	sessID, err := st.NewSession(shell, os.Getenv("TERM"))
	if err != nil {
		return err
	}

	// Plain interactive shell by default: an interactive non-login bash
	// reads ~/.bashrc, which is where the README tells users to put the
	// integration snippet. A login bash would skip it (silent no-record).
	var cmd *exec.Cmd
	if login {
		cmd = exec.Command(shell, "-l")
	} else {
		cmd = exec.Command(shell)
	}
	cmd.Env = append(os.Environ(),
		"BACKSCROLL_ACTIVE=1",
		fmt.Sprintf("BACKSCROLL_SESSION=%d", sessID),
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	// resize handling
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	winch <- syscall.SIGWINCH

	// Terminal closed (SIGHUP) or polite kill (SIGTERM): forward the hangup
	// to the shell. It kills its jobs and exits, the PTY slave closes, the
	// read loop below gets EIO and unwinds through the normal path — so the
	// pending command's partial output still gets flushed to the store.
	// Closing the PTY after a grace period is the hard fallback in case the
	// shell ignores the hangup.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-hup
		_ = cmd.Process.Signal(syscall.SIGHUP)
		time.Sleep(3 * time.Second)
		_ = ptmx.Close()
	}()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// per-command state
	var (
		curCmd    string // authoritative, from our snippet's OSC 6973;cmd
		curHint   string // emitter-provided fallback (fish ≥4.0 cmdline_url)
		curCwd    string
		buf       *capBuf
		startedAt time.Time
		recorded  int
		paused    bool
	)
	ignorePats := LoadIgnore()
	// cmdName is the best-known text for the in-flight command: our
	// snippet's OSC 6973 wins; an emitter-provided C-mark hint (fish ≥4.0
	// native marks) fills in when the snippet isn't installed.
	cmdName := func() string {
		if curCmd != "" {
			return curCmd
		}
		return curHint
	}
	flush := func(exitCode int, hasExit bool) {
		if buf == nil {
			return
		}
		name := cmdName()
		if paused || Ignored(ignorePats, name) || isShellExit(name) {
			// isShellExit: a plain `exit`/`logout` is session teardown, not
			// a command anyone wants to recall (fish emits a D mark for it,
			// unlike bash/zsh, so it would otherwise be stored).
			buf = nil
			curCmd, curHint = "", ""
			return
		}
		raw := buf.Bytes()
		plain := ansi.Strip(raw)
		if name == "" {
			name = "(unknown command)"
		}
		if err := st.AddCommand(sessID, name, curCwd, exitCode, hasExit,
			startedAt, time.Now(), raw, buf.Truncated(), string(plain)); err != nil {
			fmt.Fprintf(os.Stderr, "\r\nbackscroll: store error: %v\r\n", err)
		}
		recorded++
		buf = nil
		curCmd, curHint = "", ""
	}

	parser := NewParser(Events{
		CmdText: func(c string) { curCmd = c },
		CmdHint: func(c string) { curHint = c },
		Cwd:     func(p string) { curCwd = p },
		OutStart: func() {
			buf = newCapBuf(headCap, tailCap)
			startedAt = time.Now()
		},
		Output: func(b []byte) {
			if buf != nil {
				buf.Write(b)
			}
		},
		OutEnd: flush,
		Toggle: func(on bool) { paused = !on },
	})

	rbuf := make([]byte, 32*1024)
	for {
		n, rerr := ptmx.Read(rbuf)
		if n > 0 {
			if _, werr := os.Stdout.Write(rbuf[:n]); werr != nil {
				break
			}
			parser.Feed(rbuf[:n])
		}
		if rerr != nil {
			break
		}
	}
	// If the shell died mid-command, keep what we have — unless the
	// "command" is the shell's own terminator (`exit` / `logout`), whose
	// session-ending C mark never gets a matching D and would otherwise be
	// stored as a useless stub entry.
	if isShellExit(cmdName()) {
		buf = nil
	}
	flush(0, false)

	err = cmd.Wait()
	if oldState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}
	if recorded == 0 {
		fmt.Fprintln(os.Stderr, "backscroll: no commands were recorded — is shell integration set up?")
		fmt.Fprintln(os.Stderr, `  add to your rc file:  eval "$(backscroll init zsh)"   (or bash)`)
	}
	return err
}
