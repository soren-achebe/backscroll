package record

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
	for _, b := range p {
		idx := (c.tailStart + c.tailLen) % c.tailCap
		c.tail[idx] = b
		if c.tailLen < c.tailCap {
			c.tailLen++
		} else {
			c.tailStart = (c.tailStart + 1) % c.tailCap
		}
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
func Run(st *store.Store, headCap, tailCap int) error {
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

	cmd := exec.Command(shell, "-l")
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

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// per-command state
	var (
		curCmd    string
		curCwd    string
		buf       *capBuf
		startedAt time.Time
		recorded  int
	)
	flush := func(exitCode int, hasExit bool) {
		if buf == nil {
			return
		}
		raw := buf.Bytes()
		plain := ansi.Strip(raw)
		name := curCmd
		if name == "" {
			name = "(unknown command)"
		}
		if err := st.AddCommand(sessID, name, curCwd, exitCode, hasExit,
			startedAt, time.Now(), raw, buf.Truncated(), string(plain)); err != nil {
			fmt.Fprintf(os.Stderr, "\r\nbackscroll: store error: %v\r\n", err)
		}
		recorded++
		buf = nil
		curCmd = ""
	}

	parser := NewParser(Events{
		CmdText: func(c string) { curCmd = c },
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
	// If the shell died mid-command, keep what we have.
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
