package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/store"
)

// newExecStore gives each test a fresh DB and neutralizes any real
// ignore config on the machine running the tests.
func newExecStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BACKSCROLL_DB", filepath.Join(dir, "db.sqlite"))
	t.Setenv("BACKSCROLL_IGNORE_FILE", filepath.Join(dir, "no-such-ignore"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// sh builds a portable "run this script" argv.
func sh(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", script}
	}
	return []string{"sh", "-c", script}
}

func lastCmd(t *testing.T, st *store.Store) *store.Command {
	t.Helper()
	c, err := st.Get(-1)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExecRecordsOutputExitCwd(t *testing.T) {
	st := newExecStore(t)
	var out, errw bytes.Buffer
	script := "echo out-line && echo err-line 1>&2 && exit 3"
	code, rerr := runExec(st, sh(script), 256<<10, 1<<20, false, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out.String(), "out-line") {
		t.Fatalf("stdout passthrough missing: %q", out.String())
	}
	if !strings.Contains(errw.String(), "err-line") {
		t.Fatalf("stderr passthrough missing: %q", errw.String())
	}
	c := lastCmd(t, st)
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 3 {
		t.Fatalf("stored exit = %+v, want 3", c.ExitCode)
	}
	got := string(c.Output)
	if !strings.Contains(got, "out-line") || !strings.Contains(got, "err-line") {
		t.Fatalf("stored output missing streams: %q", got)
	}
	wd, _ := os.Getwd()
	if c.Cwd != wd {
		t.Fatalf("cwd = %q, want %q", c.Cwd, wd)
	}
	if !strings.Contains(c.Cmd, "echo out-line") {
		t.Fatalf("cmd text = %q", c.Cmd)
	}
	if c.EndedAt.Before(c.StartedAt) {
		t.Fatalf("ended %v before started %v", c.EndedAt, c.StartedAt)
	}
}

func TestExecQuietSuppressesPassthrough(t *testing.T) {
	st := newExecStore(t)
	var out, errw bytes.Buffer
	code, rerr := runExec(st, sh("echo quiet-hello"), 256<<10, 1<<20, true, &out, &errw)
	if rerr != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, rerr)
	}
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("expected no passthrough, got out=%q err=%q", out.String(), errw.String())
	}
	if !strings.Contains(string(lastCmd(t, st).Output), "quiet-hello") {
		t.Fatal("output not recorded in quiet mode")
	}
}

func TestExecTruncation(t *testing.T) {
	st := newExecStore(t)
	var out, errw bytes.Buffer
	// ~40KB of output through 1K head + 1K tail caps
	script := "i=0; while [ $i -lt 1000 ]; do echo line-number-$i-padding-padding; i=$((i+1)); done"
	if runtime.GOOS == "windows" {
		script = "for /L %i in (1,1,1000) do @echo line-number-%i-padding-padding"
	}
	code, rerr := runExec(st, sh(script), 1024, 1024, true, &out, &errw)
	if rerr != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, rerr)
	}
	c := lastCmd(t, st)
	if !c.Truncated {
		t.Fatal("expected truncated flag")
	}
	got := string(c.Output)
	if !strings.Contains(got, "bytes truncated") {
		t.Fatalf("missing truncation marker: %q", got[:200])
	}
	if !strings.Contains(got, "line-number-1") {
		t.Fatal("head lost")
	}
	if !strings.Contains(got, "line-number-999") && !strings.Contains(got, "line-number-1000") {
		t.Fatal("tail lost")
	}
}

func TestExecNotFoundRecords127(t *testing.T) {
	st := newExecStore(t)
	var out, errw bytes.Buffer
	code, rerr := runExec(st, []string{"backscroll-no-such-binary-xyz"}, 1024, 1024, false, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 127 {
		t.Fatalf("code = %d, want 127", code)
	}
	c := lastCmd(t, st)
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 127 {
		t.Fatalf("stored exit = %+v, want 127", c.ExitCode)
	}
	if !strings.Contains(string(c.Output), "backscroll exec:") {
		t.Fatalf("startup error not recorded: %q", c.Output)
	}
	if !strings.Contains(errw.String(), "backscroll exec:") {
		t.Fatal("startup error not surfaced on stderr")
	}
}

func TestExecSignalDeath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal exit codes are a unix concept")
	}
	st := newExecStore(t)
	var out, errw bytes.Buffer
	code, rerr := runExec(st, sh("kill -TERM $$"), 1024, 1024, true, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 143 { // 128+SIGTERM
		t.Fatalf("code = %d, want 143", code)
	}
	c := lastCmd(t, st)
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 143 {
		t.Fatalf("stored exit = %+v, want 143", c.ExitCode)
	}
}

func TestExecIgnorePatternSkipsRecording(t *testing.T) {
	st := newExecStore(t)
	ign := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ign, []byte("secret-tool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BACKSCROLL_IGNORE_FILE", ign)
	var out, errw bytes.Buffer
	code, rerr := runExec(st, sh("echo secret-tool ran"), 1024, 1024, true, &out, &errw)
	if rerr != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, rerr)
	}
	if _, err := st.Get(-1); err == nil {
		t.Fatal("ignored command was recorded")
	}
}

func TestExecNilStoreStillRuns(t *testing.T) {
	// transparency: no DB, command still runs and code mirrors
	var out, errw bytes.Buffer
	code, rerr := runExec(nil, sh("echo no-db && exit 7"), 1024, 1024, false, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
	if !strings.Contains(out.String(), "no-db") {
		t.Fatal("passthrough lost without store")
	}
}

func TestExecSearchable(t *testing.T) {
	st := newExecStore(t)
	var out, errw bytes.Buffer
	if code, rerr := runExec(st, sh("echo certbot renewal FAILED for example.org"), 1024, 1024, true, &out, &errw); code != 0 || rerr != nil {
		t.Fatalf("code=%d err=%v", code, rerr)
	}
	res, err := st.Search(`"renewal FAILED"`, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("search hits = %d, want 1", len(res))
	}
}

func TestExecBackgroundChildDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on sh background jobs")
	}
	st := newExecStore(t)
	var out, errw bytes.Buffer
	start := time.Now()
	// child exits 3 immediately; orphaned sleep holds the pipes open
	code, rerr := runExec(st, sh("echo bg-marker; sleep 60 & exit 3"), 1024, 1024, true, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if elapsed := time.Now().Sub(start); elapsed > 15*time.Second {
		t.Fatalf("Wait blocked on orphaned pipe holder: took %v", elapsed)
	}
	c := lastCmd(t, st)
	if !strings.Contains(string(c.Output), "bg-marker") {
		t.Fatal("pre-exit output lost")
	}
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 3 {
		t.Fatalf("stored exit = %+v, want 3", c.ExitCode)
	}
}

func TestExecWaitDelayCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on sh background jobs")
	}
	// child exits 0 with a straggler: ErrWaitDelay must map to 0
	st := newExecStore(t)
	var out, errw bytes.Buffer
	code, rerr := runExec(st, sh("sleep 60 &"), 1024, 1024, true, &out, &errw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0 (ErrWaitDelay is not a failure)", code)
	}
	c := lastCmd(t, st)
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 0 {
		t.Fatalf("stored exit = %+v, want 0", c.ExitCode)
	}
}

func TestShellJoin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"make", "make"},
		{"two words", "'two words'"},
		{"", "''"},
		{"a'b", `'a'\''b'`},
		{"$HOME", "'$HOME'"},
		{"-j4", "-j4"},
	}
	for _, tc := range cases {
		if got := execQuote(tc.in); got != tc.want {
			t.Errorf("execQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := shellJoin([]string{"sh", "-c", "echo hi | wc"}); got != `sh -c 'echo hi | wc'` {
		t.Errorf("shellJoin = %q", got)
	}
}
