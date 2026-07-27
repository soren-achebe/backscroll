//go:build windows

package main

// E2E: `backscroll run` on a real ConPTY — the full Windows recording
// path. Drives the recorder's stdin/stdout over pipes (the child shell
// still sees a real console: the pseudoconsole), waits for output in
// the passthrough stream, and asserts the recorded DB via the CLI.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func buildBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "backscroll.exe")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// recorder wraps a running `backscroll run` with pipe IO.
type recorder struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin interface{ Write([]byte) (int, error) }
	mu    sync.Mutex
	out   bytes.Buffer
	done  chan error
}

func startRecorder(t *testing.T, bin string, env []string, dir string) *recorder {
	t.Helper()
	run := exec.Command(bin, "run")
	run.Env = env
	run.Dir = dir
	stdin, err := run.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	run.Stderr = run.Stdout
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	r := &recorder{t: t, cmd: run, stdin: stdin, done: make(chan error, 1)}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				r.mu.Lock()
				r.out.Write(buf[:n])
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { r.done <- run.Wait() }()
	t.Cleanup(func() {
		// Make sure the recorder (and its DB handle) is gone before
		// TempDir cleanup tries to delete files.
		run.Process.Kill()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
		}
	})
	return r
}

func (r *recorder) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

func (r *recorder) send(s string) {
	r.t.Helper()
	if _, err := r.stdin.Write([]byte(s)); err != nil {
		r.t.Fatalf("stdin write: %v", err)
	}
}

// waitFor blocks until pred(stream) or the deadline; fatal with the full
// stream on timeout.
func (r *recorder) waitFor(what string, d time.Duration, pred func(string) bool) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred(r.snapshot()) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.t.Fatalf("%s never arrived; full stream (%d bytes):\n%q",
		what, len(r.snapshot()), r.snapshot())
}

func (r *recorder) waitExit(d time.Duration) {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(d):
		r.cmd.Process.Kill()
		r.t.Fatalf("run did not exit; full stream:\n%q", r.snapshot())
	}
}

// TestConPTYSanityCmd isolates the ConPTY plumbing from shell
// integration: a plain cmd.exe session must reach a prompt, run a
// command, and exit. Nothing is recorded (cmd emits no marks) — this
// pins spawn, resize, passthrough, input translation, and teardown.
func TestConPTYSanityCmd(t *testing.T) {
	tmp := t.TempDir()
	bin := buildBinary(t, tmp)
	env := append(os.Environ(),
		"BACKSCROLL_DB="+filepath.Join(tmp, "backscroll.db"),
		"BACKSCROLL_SHELL=cmd.exe",
	)
	r := startRecorder(t, bin, env, tmp)
	r.waitFor("cmd prompt", 60*time.Second, func(s string) bool {
		return strings.Contains(s, ">")
	})
	r.send("echo conpty-sanity\r")
	r.waitFor("echo output", 30*time.Second, func(s string) bool {
		return strings.Count(s, "conpty-sanity") >= 2 // echo + output
	})
	r.send("exit\r")
	r.waitExit(30 * time.Second)
}

func TestConPTYRecordsPwsh(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Skip("pwsh.exe not on PATH")
	}

	tmp := t.TempDir()
	bin := buildBinary(t, tmp)

	// Install the integration snippet into the real $PROFILE (the
	// recorder spawns a plain interactive pwsh, which sources it). The
	// snippet is a no-op outside backscroll sessions; still, restore
	// whatever was there before.
	profOut, err := exec.Command(pwsh, "-NoProfile", "-Command", "$PROFILE").Output()
	if err != nil {
		t.Fatalf("resolve $PROFILE: %v", err)
	}
	profile := strings.TrimSpace(string(profOut))
	if profile == "" {
		t.Fatal("empty $PROFILE")
	}
	prev, readErr := os.ReadFile(profile)
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("& \"%s\" init pwsh | Out-String | Invoke-Expression\r\n", bin)
	if err := os.WriteFile(profile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if readErr == nil {
			os.WriteFile(profile, prev, 0o644)
		} else {
			os.Remove(profile)
		}
	})
	// Sanity: the snippet itself must load in a fresh interactive-ish
	// pwsh without errors (catches profile-time failures with real
	// diagnostics instead of a silent hang).
	diag, diagErr := exec.Command(pwsh, "-NoProfile", "-Command",
		"$env:BACKSCROLL_ACTIVE='1'; & \""+bin+"\" init pwsh | Out-String | Invoke-Expression; 'SNIPPET-OK'").CombinedOutput()
	t.Logf("snippet load check (err=%v):\n%s", diagErr, diag)

	db := filepath.Join(tmp, "backscroll.db")
	env := append(os.Environ(),
		"BACKSCROLL_DB="+db,
		"BACKSCROLL_SHELL="+pwsh,
	)

	r := startRecorder(t, bin, env, tmp)

	// Prompt-end mark count n means the shell is at prompt number n.
	waitPrompt := func(want int) {
		r.waitFor(fmt.Sprintf("prompt %d", want), 120*time.Second, func(s string) bool {
			return strings.Count(s, "]133;B") >= want
		})
	}

	waitPrompt(1) // startup + profile
	r.send("echo hello-conpty\r")
	waitPrompt(2)
	r.send("cmd /c \"exit 7\"\r")
	waitPrompt(3)
	r.send("Get-Location\r")
	waitPrompt(4)
	r.send("\r") // empty accept: must store nothing
	r.send("exit\r")
	r.waitExit(60 * time.Second)

	cli := func(args ...string) string {
		c := exec.Command(bin, args...)
		c.Env = env
		b, _ := c.CombinedOutput()
		return string(b)
	}

	listing := cli("list", "-n", "20")
	t.Logf("list:\n%s", listing)
	for _, wantCmd := range []string{"echo hello-conpty", `cmd /c "exit 7"`, "Get-Location"} {
		if !strings.Contains(listing, wantCmd) {
			t.Errorf("list missing %q", wantCmd)
		}
	}
	if strings.Contains(listing, "(unknown command)") {
		t.Error("empty accept stored a stub entry")
	}

	failing := cli("list", "--exit", "fail")
	if !strings.Contains(failing, `cmd /c "exit 7"`) {
		t.Errorf("native exit 7 not recorded as failure:\n%s", failing)
	}
	if strings.Contains(failing, "echo hello-conpty") {
		t.Errorf("succeeding command marked failing:\n%s", failing)
	}

	shown := cli("show", "1")
	if !strings.Contains(shown, "hello-conpty") {
		t.Errorf("show 1 missing output:\n%s", shown)
	}
	// cwd must be a normalized Windows path (OSC 7 → winPath).
	if !strings.Contains(shown, tmp) {
		t.Errorf("show 1 missing cwd %q:\n%s", tmp, shown)
	}

	found := cli("search", "hello-conpty")
	if !strings.Contains(found, "echo hello-conpty") {
		t.Errorf("search missed output text:\n%s", found)
	}
}
