//go:build windows

package record

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// defaultShell picks the shell to spawn when the user didn't say:
// BACKSCROLL_SHELL > SHELL > pwsh on PATH > powershell > cmd.
func defaultShell() string {
	if s := os.Getenv("BACKSCROLL_SHELL"); s != "" {
		return s
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	for _, cand := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	if s := os.Getenv("COMSPEC"); s != "" {
		return s
	}
	return "cmd.exe"
}

// winShell is a shell process attached to a ConPTY (Windows
// pseudoconsole). The pipe pair is ours; the pseudoconsole renders the
// child's console into a VT byte stream on out and accepts VT input
// (keystrokes) on in.
type winShell struct {
	in   *os.File // we write keystrokes here
	out  *os.File // we read the terminal stream here
	hpc  windows.Handle
	proc *os.Process

	closedPC atomic.Bool
	waitErr  error
	waitDone chan struct{}
}

func spawnShell(shell string, login bool, env []string) (platformShell, error) {
	// login has no meaning for ConPTY shells; pwsh -Login is Unix-only.
	_ = login

	exe, err := exec.LookPath(shell)
	if err != nil {
		return nil, fmt.Errorf("shell %q not found: %w", shell, err)
	}

	// Pipe plumbing: child's console input comes from inR (we hold inW),
	// child's rendered output goes to outW (we hold outR).
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}

	size := windows.Coord{X: 80, Y: 24}
	if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 && h > 0 {
		size = windows.Coord{X: int16(w), Y: int16(h)}
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(size, inR, outW, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// The pseudoconsole dup'ed its ends; ours can go.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, err
	}
	defer attrs.Delete()
	// The attribute value IS the HPCON (passed in the pointer slot, per
	// the ConPTY API contract) — reinterpret rather than uintptr-convert
	// so go vet doesn't flag a fake pointer.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, err
	}

	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.ProcThreadAttributeList = attrs.List()

	argv, err := windows.UTF16PtrFromString(windows.EscapeArg(exe))
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, err
	}
	envBlock := buildEnvBlock(env)

	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, argv, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&envBlock[0], nil, &si.StartupInfo, &pi)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, fmt.Errorf("CreateProcess %s: %w", exe, err)
	}
	windows.CloseHandle(pi.Thread)

	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		// Extremely unlikely; fall back to a raw handle wait below.
		proc = nil
	}

	s := &winShell{
		in:       os.NewFile(uintptr(inW), "|conpty-in"),
		out:      os.NewFile(uintptr(outR), "|conpty-out"),
		hpc:      hpc,
		proc:     proc,
		waitDone: make(chan struct{}),
	}

	// Waiter: when the shell exits, close the pseudoconsole so the read
	// loop sees EOF/broken-pipe and unwinds through the normal flush
	// path. ClosePseudoConsole blocks until pending output is drained,
	// which is exactly why it must run here and not inline in Wait().
	procH := windows.Handle(pi.Process)
	go func() {
		defer close(s.waitDone)
		if s.proc != nil {
			state, err := s.proc.Wait()
			if err != nil {
				s.waitErr = err
			} else if code := state.ExitCode(); code != 0 {
				s.waitErr = fmt.Errorf("shell exited with status %d", code)
			}
		} else {
			windows.WaitForSingleObject(procH, windows.INFINITE)
		}
		windows.CloseHandle(procH)
		if s.closedPC.CompareAndSwap(false, true) {
			windows.ClosePseudoConsole(s.hpc)
		}
	}()
	return s, nil
}

func (s *winShell) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *winShell) Write(p []byte) (int, error) { return s.in.Write(p) }

func (s *winShell) Close() error {
	if s.closedPC.CompareAndSwap(false, true) {
		windows.ClosePseudoConsole(s.hpc)
	}
	s.in.Close()
	return s.out.Close()
}

func (s *winShell) Wait() error {
	<-s.waitDone
	return s.waitErr
}

// WatchResize polls the outer console size (Windows has no SIGWINCH) and
// mirrors changes into the pseudoconsole.
func (s *winShell) WatchResize(cols *atomic.Int32) {
	update := func() {
		if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 && h > 0 {
			if int32(w) != cols.Load() {
				cols.Store(int32(w))
			}
			_ = windows.ResizePseudoConsole(s.hpc, windows.Coord{X: int16(w), Y: int16(h)})
		}
	}
	update()
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if s.closedPC.Load() {
				return
			}
			if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 && h > 0 {
				if int32(w) != cols.Load() {
					cols.Store(int32(w))
					_ = windows.ResizePseudoConsole(s.hpc, windows.Coord{X: int16(w), Y: int16(h)})
				}
			}
		}
	}()
}

// WatchHangup: closing the console window on Windows delivers
// CTRL_CLOSE_EVENT and then terminates the process tree — there is no
// SIGHUP-style grace path to arrange here. Partial output of an
// in-flight command is lost on window close; a normal `exit` flushes
// through the read loop as usual.
func (s *winShell) WatchHangup() {}

// enableVT turns on VT processing for our own console (output side) so
// the child's escape sequences render, and returns a restore func.
// (x/term's MakeRaw handles the input side, ENABLE_VIRTUAL_TERMINAL_INPUT.)
func enableVT() func() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	newMode := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(h, newMode); err != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// buildEnvBlock converts KEY=VALUE strings into the UTF-16,
// double-NUL-terminated block CreateProcess expects (sorted,
// case-insensitively, per convention).
func buildEnvBlock(env []string) []uint16 {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToUpper(sorted[i]) < strings.ToUpper(sorted[j])
	})
	var b []uint16
	for _, kv := range sorted {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	b = append(b, 0)
	return b
}

// Referenced so the syscall import stays honest if future edits need it.
var _ = syscall.Getpagesize
