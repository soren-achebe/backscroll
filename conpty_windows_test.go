//go:build windows

package main

// E2E: `backscroll run` on a real ConPTY with pwsh + the init pwsh
// snippet — the full Windows recording path. Drives the recorder's
// stdin/stdout over pipes (the child still sees a real console: the
// pseudoconsole), waits for prompt marks in the passthrough stream, and
// asserts the recorded DB via the CLI.

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

func TestConPTYRecordsPwsh(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
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
	prev, hadProfile := os.ReadFile(profile)
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("& \"%s\" init pwsh | Out-String | Invoke-Expression\r\n", bin)
	if err := os.WriteFile(profile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadProfile == nil {
			os.WriteFile(profile, prev, 0o644)
		} else {
			os.Remove(profile)
		}
	})

	db := filepath.Join(tmp, "backscroll.db")
	env := append(os.Environ(),
		"BACKSCROLL_DB="+db,
		"BACKSCROLL_SHELL="+pwsh,
	)

	run := exec.Command(bin, "run")
	run.Env = env
	run.Dir = tmp
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

	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}

	// Wait until the stream contains `want` copies of the prompt-end
	// mark (133;B) — i.e. the shell is at prompt number `want`.
	waitPrompt := func(want int) {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Count(snapshot(), "]133;B") >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("prompt %d never arrived; stream tail:\n%q", want, tail(snapshot(), 2000))
	}

	send := func(s string) {
		if _, err := stdin.Write([]byte(s)); err != nil {
			t.Fatalf("stdin write: %v", err)
		}
	}

	waitPrompt(1) // startup + profile
	send("echo hello-conpty\r")
	waitPrompt(2)
	send("cmd /c \"exit 7\"\r")
	waitPrompt(3)
	send("Get-Location\r")
	waitPrompt(4)
	send("\r") // empty accept: must store nothing
	send("exit\r")

	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		run.Process.Kill()
		t.Fatalf("run did not exit; stream tail:\n%q", tail(snapshot(), 2000))
	}

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

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
