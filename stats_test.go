package main

import (
	"database/sql"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/store"
)

func TestCmdHead(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ls -la", "ls"},
		{"git push origin main", "git push"},
		{"git -C /tmp status", "git status"},
		{"sudo systemctl restart nginx", "systemctl restart"},
		{"sudo -u www ls", "www"}, // known imperfection: flag values aren't understood
		{"FOO=bar BAZ=1 make test", "make test"},
		{"env FOO=bar ./run.sh", "run.sh"},
		{"/usr/bin/python3 script.py", "python3"},
		{"time go test ./...", "go test"},
		{"timeout 5 curl -s example.com", "5"}, // same imperfection class
		{`\grep -r foo .`, "grep"},
		{"docker compose up -d", "docker compose"},
		{"cargo build --release", "cargo build"},
		{"echo hi | grep h", "echo"},
		{"for i in 1 2 3; do echo $i; done", "for"},
		{"make", "make"},
		{"git", "git"},
		{"", "(other)"},
		{"   ", "(other)"},
		{"-v", "(other)"},
		{"FOO=bar", "(other)"},
		{"npm run build\nsecond line ignored", "npm run"},
		{"backscroll search foo", "backscroll search"},
	}
	for _, c := range cases {
		if got := cmdHead(c.in); got != c.want {
			t.Errorf("cmdHead(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsAssignment(t *testing.T) {
	yes := []string{"FOO=bar", "a=1", "_X=", "PATH2=/x"}
	no := []string{"=x", "foo", "-x=1", "a b=c", "./x=y", "a-b=c"}
	for _, s := range yes {
		if !isAssignment(s) {
			t.Errorf("isAssignment(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isAssignment(s) {
			t.Errorf("isAssignment(%q) = true, want false", s)
		}
	}
}

func TestHumanDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "-"},
		{250 * time.Millisecond, "250ms"},
		{4200 * time.Millisecond, "4.2s"},
		{3*time.Minute + 41*time.Second, "3m41s"},
		{time.Hour + 12*time.Minute, "1h12m"},
		{51 * time.Hour, "2d3h"},
	}
	for _, c := range cases {
		if got := humanDur(c.in); got != c.want {
			t.Errorf("humanDur(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStatKey(t *testing.T) {
	mk := func(cmd, cwd string, exit int64, valid bool, host string, at time.Time) store.Command {
		return store.Command{Cmd: cmd, Cwd: cwd,
			ExitCode: sql.NullInt64{Int64: exit, Valid: valid},
			Host:     host, StartedAt: at}
	}
	at := time.Date(2026, 7, 27, 6, 0, 0, 0, time.Local)
	c := mk("git push", "/home/x/proj", 1, true, "", at)
	if got := statKey("cmd", c); got != "git push" {
		t.Errorf("cmd key = %q", got)
	}
	if got := statKey("cwd", c); got != "/home/x/proj" {
		t.Errorf("cwd key = %q", got)
	}
	if got := statKey("exit", c); got != "1" {
		t.Errorf("exit key = %q", got)
	}
	if got := statKey("host", c); got != "local" {
		t.Errorf("host key = %q, want local", got)
	}
	if got := statKey("day", c); got != "2026-07-27" {
		t.Errorf("day key = %q", got)
	}
	c2 := mk("ls", "/", 0, false, "laptop", time.Time{})
	if got := statKey("exit", c2); got != "?" {
		t.Errorf("no-exit key = %q, want ?", got)
	}
	if got := statKey("host", c2); got != "laptop" {
		t.Errorf("host key = %q, want laptop", got)
	}
	if got := statKey("day", c2); got != "unknown" {
		t.Errorf("zero-time day key = %q, want unknown", got)
	}
}

func TestCmdHeadOperators(t *testing.T) {
	cases := []struct{ in, want string }{
		{"FOO=bar env | grep FOO", "env"},
		{"env | sort", "env"},
		{"make && git push", "make"},
		{"git && ls", "git"},
		{"go build 2>err.log", "go build"},
		{"go build 2> err.log", "go build"},
		{"cargo | tee log", "cargo"},
		{"(cd /tmp && ls)", "(other)"},
		{"sudo !!", "!!"},
		{"| broken", "(other)"},
	}
	for _, c := range cases {
		if got := cmdHead(c.in); got != c.want {
			t.Errorf("cmdHead(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
