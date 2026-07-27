package record

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/store"
)

// platformShell is a shell process attached to a pseudo-terminal — a real
// PTY on Unix, a ConPTY on Windows. Reads return the terminal byte
// stream; writes feed the user's keystrokes in.
type platformShell interface {
	io.ReadWriter
	Close() error
	Wait() error
	// WatchResize keeps the inner terminal sized to the outer one and
	// cols current (for echo reconstruction).
	WatchResize(cols *atomic.Int32)
	// WatchHangup arranges for a terminal-close/polite-kill to unwind
	// through the normal read-loop path so pending output is flushed.
	WatchHangup()
}

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
	shell := defaultShell()

	sessID, err := st.NewSession(shell, os.Getenv("TERM"))
	if err != nil {
		return err
	}

	env := append(os.Environ(),
		"BACKSCROLL_ACTIVE=1",
		fmt.Sprintf("BACKSCROLL_SESSION=%d", sessID),
	)
	sh, err := spawnShell(shell, login, env)
	if err != nil {
		return err
	}
	defer sh.Close()

	var cols atomic.Int32 // terminal width, for echo reconstruction
	sh.WatchResize(&cols)
	sh.WatchHangup()

	restoreVT := enableVT()
	defer restoreVT()
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	go func() { _, _ = io.Copy(sh, os.Stdin) }()

	// per-command state
	var (
		curCmd    string // authoritative, from our snippet's OSC 6973;cmd
		curHint   string // emitter-provided fallback (fish ≥4.0 cmdline_url)
		curEcho   string // reconstructed from the input echo (plain-133 emitters)
		curCwd    string
		buf       *capBuf
		startedAt time.Time
		recorded  int
		paused    bool
	)
	ignorePats := LoadIgnore()
	// cmdName is the best-known text for the in-flight command: our
	// snippet's OSC 6973 wins; an emitter-provided C-mark hint (fish ≥4.0
	// native marks, VS Code 633;E, kitty, WezTerm) fills in when the
	// snippet isn't installed; last resort is reconstructing the typed
	// line from its terminal echo (plain-133 emitters like Ghostty and
	// iTerm2, which mark boundaries but never report the command text).
	cmdName := func() string {
		if curCmd != "" {
			return curCmd
		}
		if curHint != "" {
			return curHint
		}
		return curEcho
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
			curCmd, curHint, curEcho = "", "", ""
			return
		}
		raw := buf.Bytes()
		plain := ansi.Strip(raw)
		if name == "" {
			// No command text: only worth keeping if there is real
			// (non-metadata) output.
			if len(plain) == 0 {
				// Phantom prompt cycle — dropped regardless of exit
				// code. VS Code's bash integration emits E;; + C + bare
				// D when Ctrl-C is pressed at an empty prompt (the
				// PROMPT_COMMAND re-fire quirk); Ghostty's zsh
				// integration opens a C for `exit` whose only bytes are
				// an OSC 2 title + cursor-shape escape; iTerm2's bash
				// integration fires a stray preexec C while the rc file
				// is still being sourced (closed by the first prompt's
				// D;0), and — by design — closes EVERY prompt cycle
				// with C..D, so empty Enter and Ctrl-C at the prompt
				// arrive as empty cycles whose D carries the previous
				// command's stale $? (1, 130, ...). Nothing real ran,
				// so store nothing. Compared ANSI-stripped: pure
				// escape-sequence traffic is terminal metadata, not
				// output. A real command always leaves text, an exit
				// code with output, or reconstructable command text —
				// the only loss is a command that both defeated echo
				// reconstruction AND printed nothing, under an emitter
				// with no A/B marks.
				buf = nil
				curCmd, curHint, curEcho = "", "", ""
				return
			}
			if t := strings.TrimSpace(string(plain)); (!hasExit || exitCode == 0) &&
				(t == "exit" || t == "logout") {
				// Session teardown echoed by the shell itself: bash
				// prints "exit" when the shell terminates. Without a
				// cmdline-providing emitter there is no name to trip
				// isShellExit above, so match the echo. (Plain-133
				// emitters like Ghostty open a C for `exit` that never
				// gets a D.)
				buf = nil
				curCmd, curHint, curEcho = "", "", ""
				return
			}
		}
		if name == "" {
			name = "(unknown command)"
		}
		if err := st.AddCommand(sessID, name, curCwd, exitCode, hasExit,
			startedAt, time.Now(), raw, buf.Truncated(), string(plain)); err != nil {
			fmt.Fprintf(os.Stderr, "\r\nbackscroll: store error: %v\r\n", err)
		}
		recorded++
		buf = nil
		curCmd, curHint, curEcho = "", "", ""
	}

	parser := NewParser(Events{
		CmdText: func(c string) { curCmd = c },
		CmdHint: func(c string) { curHint = c },
		Echo:    func(b []byte) { curEcho = ReconstructEcho(b, int(cols.Load())) },
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
		n, rerr := sh.Read(rbuf)
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

	err = sh.Wait()
	if oldState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}
	if recorded == 0 {
		fmt.Fprintln(os.Stderr, "backscroll: no commands were recorded — is shell integration set up?")
		fmt.Fprintln(os.Stderr, `  add to your rc file:  eval "$(backscroll init zsh)"   (or bash)`)
	}
	return err
}
