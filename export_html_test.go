package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/ansihtml"
	"github.com/soren-achebe/backscroll/internal/store"
)

func TestExportHTML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.html")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC)
	cmds := []*store.Command{
		{
			ID: 1, Cmd: "curl -s https://x | grep '<title>'",
			Cwd: "/tmp/proj", ExitCode: sql.NullInt64{Int64: 0, Valid: true},
			StartedAt: start, EndedAt: start.Add(1200 * time.Millisecond),
			Output: []byte("plain \x1b[31mred\x1b[0m & \x1b[1mbold\x1b[0m <tag>\n"),
		},
		{
			ID: 2, Cmd: "false", Cwd: "/tmp",
			ExitCode:  sql.NullInt64{Int64: 1, Valid: true},
			StartedAt: start, EndedAt: start.Add(time.Second),
			Host: "laptop", Truncated: true,
		},
	}
	if err := exportHTML(f, cmds); err != nil {
		t.Fatalf("exportHTML: %v", err)
	}
	f.Close()
	b, _ := os.ReadFile(path)
	got := string(b)

	for _, want := range []string{
		"<!doctype html>",
		"<title>curl -s https://x | grep '&lt;title&gt;' (+1 more) — backscroll</title>",
		// command line escaped, not raw
		"grep '&lt;title&gt;'",
		// ANSI rendered as spans, literal < escaped
		`<span class="f1">red</span>`,
		`<span class="bo">bold</span>`,
		"&amp; ",
		"&lt;tag&gt;",
		// meta bits
		`<span class="ok">exit 0</span>`,
		`<span class="bad">exit 1</span>`,
		"1.2s",
		"2026-07-27 11:00:00 UTC",
		"/tmp/proj",
		"[laptop]",
		"output truncated",
		`<span class="di">(no output)</span>`,
		// self-contained: palette + class rules embedded
		"--c1:#ff7b72",
		".f1{color:var(--c1)}",
		// footer link
		`<a href="https://github.com/soren-achebe/backscroll">backscroll</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// no raw escape bytes may survive into the page
	if strings.ContainsRune(got, 0x1b) {
		t.Error("raw ESC byte leaked into HTML")
	}
	// zero JS by design
	if strings.Contains(got, "<script") {
		t.Error("unexpected <script> in export")
	}
}

// TestExportHTMLEndToEnd drives cmdExport with a real store.
func TestExportHTMLEndToEnd(t *testing.T) {
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.NewSession("/bin/bash", "xterm")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	if err := st.AddCommand(sess, "echo secret", "/tmp", 0, true, at, at.Add(time.Second),
		[]byte("token=ghp_0123456789abcdefghijklmnopqrstuvwxyz12\n"), false,
		"token=ghp_0123456789abcdefghijklmnopqrstuvwxyz12\n"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := filepath.Join(t.TempDir(), "out.html")
	if err := cmdExport([]string{"--format", "html", "--redact", "-o", out, "-1"}); err != nil {
		t.Fatalf("cmdExport: %v", err)
	}
	b, _ := os.ReadFile(out)
	got := string(b)
	if strings.Contains(got, "ghp_0123456789") {
		t.Error("--redact did not scrub token from HTML export")
	}
	if !strings.Contains(got, "echo secret") || !strings.Contains(got, "<pre>") {
		t.Error("html export missing expected content")
	}
}

// TestAnsiCSSInSyncWithWebUI pins the inline ANSI CSS in web/index.html
// to ansihtml.Palette/CSS so the two renderings can't drift apart.
func TestAnsiCSSInSyncWithWebUI(t *testing.T) {
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, src := range []string{ansihtml.Palette, ansihtml.CSS} {
		for _, line := range strings.Split(src, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !strings.Contains(page, line) {
				t.Errorf("web/index.html missing rule line %q (update the inline copy or ansihtml/css.go)", line)
			}
		}
	}
}
