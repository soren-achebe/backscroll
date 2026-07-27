//go:build !windows

package record

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// defaultShell picks the shell to spawn when the user didn't say.
func defaultShell() string {
	if s := os.Getenv("BACKSCROLL_SHELL"); s != "" {
		return s
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/bash"
}

type unixShell struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// spawnShell starts the shell attached to a fresh PTY.
func spawnShell(shell string, login bool, env []string) (platformShell, error) {
	// Plain interactive shell by default: an interactive non-login bash
	// reads ~/.bashrc, which is where the README tells users to put the
	// integration snippet. A login bash would skip it (silent no-record).
	var cmd *exec.Cmd
	if login {
		cmd = exec.Command(shell, "-l")
	} else {
		cmd = exec.Command(shell)
	}
	cmd.Env = env
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &unixShell{ptmx: ptmx, cmd: cmd}, nil
}

func (s *unixShell) Read(p []byte) (int, error)  { return s.ptmx.Read(p) }
func (s *unixShell) Write(p []byte) (int, error) { return s.ptmx.Write(p) }
func (s *unixShell) Close() error                { return s.ptmx.Close() }
func (s *unixShell) Wait() error                 { return s.cmd.Wait() }

// WatchResize keeps the inner PTY sized to the outer terminal and cols
// current for echo reconstruction.
func (s *unixShell) WatchResize(cols *atomic.Int32) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, s.ptmx)
			if w, _, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
				cols.Store(int32(w))
			}
		}
	}()
	winch <- syscall.SIGWINCH
}

// WatchHangup handles terminal close (SIGHUP) or a polite kill (SIGTERM):
// forward the hangup to the shell. It kills its jobs and exits, the PTY
// slave closes, the read loop gets EIO and unwinds through the normal
// path — so the pending command's partial output still gets flushed to
// the store. Closing the PTY after a grace period is the hard fallback in
// case the shell ignores the hangup.
func (s *unixShell) WatchHangup() {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-hup
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
		time.Sleep(3 * time.Second)
		_ = s.ptmx.Close()
	}()
}

// enableVT is a no-op on Unix: every terminal already speaks VT.
func enableVT() func() { return func() {} }

var _ io.ReadWriter = (*unixShell)(nil)
