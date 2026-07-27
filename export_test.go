package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/store"
)

func TestFenceFor(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"plain output", "```"},
		{"has `inline` code", "```"},
		{"has ``` a fence", "````"},
		{"x``````y", "```````"},
		{"", "```"},
	}
	for _, c := range cases {
		if got := fenceFor(c.body); got != c.want {
			t.Errorf("fenceFor(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestHTMLEscape(t *testing.T) {
	if got := htmlEscape(`a < b && c > d`); got != "a &lt; b &amp;&amp; c &gt; d" {
		t.Errorf("htmlEscape = %q", got)
	}
}

func TestRound2(t *testing.T) {
	if round2(0.6149999) != 0.615 && round2(0.615) != 0.615 {
		t.Errorf("round2 broken: %v", round2(0.615))
	}
	if round2(0.0) != 0.0 {
		t.Errorf("round2(0) = %v", round2(0.0))
	}
}

func TestFenceForNeverContained(t *testing.T) {
	// property: the chosen fence must never appear in the body
	bodies := []string{
		"a", "`", "``", "```", "````", "`````",
		strings.Repeat("`", 20) + "text" + strings.Repeat("`", 7),
	}
	for _, b := range bodies {
		f := fenceFor(b)
		if strings.Contains(b, f) {
			t.Errorf("fence %q appears in body %q", f, b)
		}
	}
}

func TestSplitTargets(t *testing.T) {
	mk := func() *flag.FlagSet {
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		fs.String("format", "md", "")
		fs.Bool("details", false, "")
		fs.Int("n", 20, "")
		filterFlags(fs)
		return fs
	}
	cases := []struct {
		args, wantFlags, wantTargets []string
	}{
		{[]string{"-2", "--exit", "1", "5"}, []string{"--exit", "1"}, []string{"-2", "5"}},
		{[]string{"-n", "5", "-1"}, []string{"-n", "5"}, []string{"-1"}},
		{[]string{"--format=md", "3"}, []string{"--format=md"}, []string{"3"}},
		{[]string{"--details", "-1"}, []string{"--details"}, []string{"-1"}},
		{[]string{"--since", "30m", "--exit", "fail"}, []string{"--since", "30m", "--exit", "fail"}, nil},
		{[]string{"--session", "3"}, []string{"--session", "3"}, nil},
		{[]string{"-1", "-2", "-3"}, nil, []string{"-1", "-2", "-3"}},
		{[]string{"--nosuchflag", "7"}, []string{"--nosuchflag"}, []string{"7"}},
	}
	for _, c := range cases {
		gotFlags, gotTargets := splitTargets(mk(), c.args)
		if !reflect.DeepEqual(gotFlags, c.wantFlags) || !reflect.DeepEqual(gotTargets, c.wantTargets) {
			t.Errorf("splitTargets(%q) = %q, %q; want %q, %q",
				c.args, gotFlags, gotTargets, c.wantFlags, c.wantTargets)
		}
	}
}

func TestExportFilterMode(t *testing.T) {
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess, err := st.NewSession("/bin/bash", "xterm")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	base := time.Now().Add(-time.Hour)
	add := func(cmd string, exit int, out string) {
		t.Helper()
		at := base
		base = base.Add(time.Minute)
		if err := st.AddCommand(sess, cmd, "/tmp", exit, true, at, at.Add(time.Second),
			[]byte(out), false, out); err != nil {
			t.Fatalf("AddCommand(%q): %v", cmd, err)
		}
	}
	add("true", 0, "")
	add("fail-one", 1, "first failure\n")
	add("true again", 0, "ok\n")
	add("fail-two", 2, "second failure\n")
	st.Close()

	out := filepath.Join(t.TempDir(), "out.md")
	if err := cmdExport([]string{"--exit", "fail", "-o", out}); err != nil {
		t.Fatalf("cmdExport: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	i1 := bytes.Index(got, []byte("fail-one"))
	i2 := bytes.Index(got, []byte("fail-two"))
	if i1 < 0 || i2 < 0 {
		t.Fatalf("missing failures in export:\n%s", got)
	}
	if i1 > i2 {
		t.Errorf("want oldest-first order, got:\n%s", got)
	}
	if bytes.Contains(got, []byte("true again")) {
		t.Errorf("non-failing command leaked into filtered export:\n%s", got)
	}

	// ids + filters is an error
	if err := cmdExport([]string{"-1", "--exit", "fail"}); err == nil {
		t.Error("want error when mixing ids and filters")
	}
	// no matches is an error
	if err := cmdExport([]string{"--exit", "42"}); err == nil {
		t.Error("want error when no commands match")
	}
}
