package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func add(t *testing.T, st *Store, sess int64, cmd, out string, exit int, at time.Time) {
	t.Helper()
	if err := st.AddCommand(sess, cmd, "/tmp", exit, true, at, at.Add(time.Second),
		[]byte(out), false, out); err != nil {
		t.Fatalf("AddCommand(%q): %v", cmd, err)
	}
}

func TestRoundtrip(t *testing.T) {
	st := testStore(t)
	sess, err := st.NewSession("bash", "xterm")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	raw := []byte("hello \x1b[31mred\x1b[0m world\n")
	now := time.Now()
	if err := st.AddCommand(sess, "echo hi", "/home/x", 0, true, now, now.Add(time.Second),
		raw, false, "hello red world\n"); err != nil {
		t.Fatalf("AddCommand: %v", err)
	}

	c, err := st.Get(1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Cmd != "echo hi" || c.Cwd != "/home/x" {
		t.Errorf("got cmd=%q cwd=%q", c.Cmd, c.Cwd)
	}
	if !c.ExitCode.Valid || c.ExitCode.Int64 != 0 {
		t.Errorf("exit code = %+v, want valid 0", c.ExitCode)
	}
	if !bytes.Equal(c.Output, raw) {
		t.Errorf("output roundtrip: got %q want %q", c.Output, raw)
	}
	if c.OutputLen != int64(len(raw)) {
		t.Errorf("OutputLen = %d, want %d", c.OutputLen, len(raw))
	}
}

func TestGetNegativeOffsets(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	base := time.Now().Add(-time.Hour)
	for i, cmd := range []string{"first", "second", "third"} {
		add(t, st, sess, cmd, cmd+" out", 0, base.Add(time.Duration(i)*time.Minute))
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{-1, "third"}, {-2, "second"}, {-3, "first"}, {2, "second"}} {
		c, err := st.Get(tc.n)
		if err != nil {
			t.Fatalf("Get(%d): %v", tc.n, err)
		}
		if c.Cmd != tc.want {
			t.Errorf("Get(%d).Cmd = %q, want %q", tc.n, c.Cmd, tc.want)
		}
	}
	if _, err := st.Get(-4); err == nil {
		t.Error("Get(-4) on 3 entries: want error, got nil")
	}
	if _, err := st.Get(99); err == nil {
		t.Error("Get(99): want error, got nil")
	}
}

func TestSearch(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("zsh", "xterm")
	now := time.Now()
	add(t, st, sess, "curl svc", "connection refused by host\n", 7, now.Add(-2*time.Minute))
	add(t, st, sess, "make build", "all targets up to date\n", 0, now.Add(-time.Minute))

	res, err := st.Search(`"connection refused"`, Filter{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Cmd != "curl svc" {
		t.Fatalf("Search results = %+v, want single curl svc", res)
	}
	if res[0].Snippet == "" {
		t.Error("Search returned empty snippet")
	}

	res, err = st.Search(`"no such phrase anywhere"`, Filter{Limit: 10})
	if err != nil {
		t.Fatalf("Search(miss): %v", err)
	}
	if len(res) != 0 {
		t.Errorf("Search(miss) = %d results, want 0", len(res))
	}
}

func TestPrevSame(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	base := time.Now().Add(-time.Hour)
	add(t, st, sess, "ls", "run1", 0, base)                    // id 1
	add(t, st, sess, "date", "x", 0, base.Add(time.Minute))    // id 2
	add(t, st, sess, "ls", "run2", 0, base.Add(2*time.Minute)) // id 3

	prev, err := st.PrevSame(3, "ls")
	if err != nil {
		t.Fatalf("PrevSame: %v", err)
	}
	if prev.ID != 1 || string(prev.Output) != "run1" {
		t.Errorf("PrevSame(3, ls) = id %d output %q, want id 1 run1", prev.ID, prev.Output)
	}
	if _, err := st.PrevSame(1, "ls"); err == nil {
		t.Error("PrevSame(1, ls): want error (no earlier run), got nil")
	}
}

func TestUpdateOutputAndFTS(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	add(t, st, sess, "echo tok", "secret sauce ABC123\n", 0, time.Now())

	if err := st.UpdateOutput(1, "echo tok", []byte("secret sauce [REDACTED]\n"),
		"secret sauce [REDACTED]\n"); err != nil {
		t.Fatalf("UpdateOutput: %v", err)
	}
	c, _ := st.Get(1)
	if string(c.Output) != "secret sauce [REDACTED]\n" {
		t.Errorf("output after update = %q", c.Output)
	}
	if res, _ := st.Search(`"ABC123"`, Filter{Limit: 10}); len(res) != 0 {
		t.Errorf("old text still findable in FTS after UpdateOutput: %+v", res)
	}
	if res, _ := st.Search(`"REDACTED"`, Filter{Limit: 10}); len(res) != 1 {
		t.Errorf("new text not findable in FTS after UpdateOutput")
	}
}

func TestPruneAndDelete(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	add(t, st, sess, "old", "old out", 0, time.Now().Add(-48*time.Hour))
	add(t, st, sess, "new", "new out", 0, time.Now())

	n, err := st.Prune(24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("Prune removed %d, want 1", n)
	}
	if res, _ := st.Search(`"old out"`, Filter{Limit: 10}); len(res) != 0 {
		t.Error("pruned entry still in FTS")
	}
	if _, err := st.Get(2); err != nil {
		t.Errorf("survivor gone after prune: %v", err)
	}

	if err := st.Delete(2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(2); err == nil {
		t.Error("Get(2) after Delete: want error")
	}
	if res, _ := st.Search(`"new out"`, Filter{Limit: 10}); len(res) != 0 {
		t.Error("deleted entry still in FTS")
	}
}

func TestStats(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	add(t, st, sess, "a", "aaa", 0, time.Now())
	add(t, st, sess, "b", "bbb", 1, time.Now())
	s, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Commands != 2 || s.Sessions != 1 {
		t.Errorf("Stats = %+v, want 2 commands / 1 session", s)
	}
}

func TestListFilters(t *testing.T) {
	st := testStore(t)
	s1, _ := st.NewSession("bash", "xterm")
	s2, _ := st.NewSession("zsh", "xterm")
	old := time.Now().Add(-48 * time.Hour)
	now := time.Now()
	addAt := func(sess int64, cmd, cwd string, exit int, at time.Time) {
		t.Helper()
		if err := st.AddCommand(sess, cmd, cwd, exit, true, at, at.Add(time.Second),
			[]byte(cmd+" out\n"), false, cmd+" out\n"); err != nil {
			t.Fatal(err)
		}
	}
	addAt(s1, "make", "/src/app", 2, old)      // 1: old, failed, /src/app
	addAt(s1, "ls", "/src/app/sub", 0, now)    // 2: /src/app/sub
	addAt(s2, "go test", "/src/apple", 1, now) // 3: sibling dir w/ prefix overlap
	addAt(s2, "true", "/src/app", 0, now)      // 4

	ids := func(cmds []Command) []int64 {
		var out []int64
		for _, c := range cmds {
			out = append(out, c.ID)
		}
		return out
	}
	want := func(name string, f Filter, exp ...int64) {
		t.Helper()
		got, err := st.List(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		g := ids(got)
		if len(g) != len(exp) {
			t.Fatalf("%s: got ids %v, want %v", name, g, exp)
		}
		for i := range exp {
			if g[i] != exp[i] {
				t.Fatalf("%s: got ids %v, want %v", name, g, exp)
			}
		}
	}

	want("session", Filter{Session: s2}, 4, 3)
	// cwd matches dir and subdirs, but NOT /src/apple (prefix overlap)
	want("cwd", Filter{Cwd: "/src/app"}, 4, 2, 1)
	want("failed", Filter{Failed: true}, 3, 1)
	want("exit=2", Filter{Exit: 2, ExitSet: true}, 1)
	want("exit=0", Filter{ExitSet: true}, 4, 2)
	want("since", Filter{Since: time.Now().Add(-time.Hour)}, 4, 3, 2)
	want("combo", Filter{Cwd: "/src/app", Failed: true}, 1)
	want("limit", Filter{Limit: 2}, 4, 3)

	// same filters through Search
	res, err := st.Search(`"out"`, Filter{Cwd: "/src/app", Failed: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != 1 {
		t.Fatalf("search+filter: got %v", ids(res))
	}
}
