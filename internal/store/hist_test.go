package store

import (
	"testing"
	"time"
)

func histEntries(base time.Time) []HistEntry {
	return []HistEntry{
		{Seq: 100, Cmd: "make build", Started: base, Ended: base.Add(3 * time.Second)},
		{Seq: 101, Cmd: "git push", Exit: 1, HasExit: true, Started: base.Add(time.Minute)},
		{Seq: 102, Cmd: "ls -la"}, // no timestamp (plain bash history)
	}
}

func TestAddHistoryIdempotent(t *testing.T) {
	st := testStore(t)
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	added, err := st.AddHistory("hist:test", histEntries(base))
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Fatalf("added %d, want 3", added)
	}
	added, err = st.AddHistory("hist:test", histEntries(base))
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("re-import added %d, want 0", added)
	}
	cmds, err := st.List(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 3 {
		t.Fatalf("got %d rows", len(cmds))
	}
}

func TestHistTimelineOrdering(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	now := time.Now().Truncate(time.Millisecond)
	// a recent local command recorded BEFORE the import happens
	add(t, st, sess, "echo recent", "out", 0, now)
	// then a bulk import of week-old history (rows get HIGHER ids)
	old := now.Add(-7 * 24 * time.Hour)
	if _, err := st.AddHistory("hist:test", histEntries(old)); err != nil {
		t.Fatal(err)
	}
	cmds, err := st.List(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 4 {
		t.Fatalf("got %d rows", len(cmds))
	}
	// timeline order: recent local first, no-timestamp entries last
	if cmds[0].Cmd != "echo recent" {
		t.Errorf("newest first, got %q", cmds[0].Cmd)
	}
	if cmds[3].Cmd != "ls -la" {
		t.Errorf("timestamp-less entries sort oldest, got %q", cmds[3].Cmd)
	}
	// show -1 must be the recent local command, not the imports
	c, err := st.Get(-1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cmd != "echo recent" {
		t.Errorf("Get(-1) = %q, want the recent local command", c.Cmd)
	}
	// search follows timeline order too
	res, err := st.Search(`"git push" OR "echo recent"`, Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Cmd != "echo recent" {
		t.Errorf("search order: %+v", res)
	}
}

func TestPrevSameSkipsHistImports(t *testing.T) {
	st := testStore(t)
	sess, _ := st.NewSession("bash", "xterm")
	now := time.Now().Truncate(time.Millisecond)
	add(t, st, sess, "make build", "first run output", 0, now.Add(-time.Hour))
	add(t, st, sess, "make build", "second run output", 0, now)
	// import an even more recent hist row with the same cmd (no output)
	if _, err := st.AddHistory("hist:test", []HistEntry{
		{Seq: 1, Cmd: "make build", Started: now.Add(-30 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	latest, err := st.Get(-1)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Cmd != "make build" || string(latest.Output) != "second run output" {
		t.Fatalf("latest = %+v", latest)
	}
	prev, err := st.PrevSame(latest.ID, latest.Cmd)
	if err != nil {
		t.Fatal(err)
	}
	if string(prev.Output) != "first run output" {
		t.Errorf("PrevSame picked %q — must skip the output-less hist import", prev.Output)
	}
}

func TestHistNullFields(t *testing.T) {
	st := testStore(t)
	if _, err := st.AddHistory("hist:zsh", []HistEntry{
		{Seq: 5, Cmd: "true", Started: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := st.Get(-1)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExitCode.Valid {
		t.Errorf("exit must be NULL when source has none")
	}
	if !c.EndedAt.IsZero() {
		t.Errorf("ended must be NULL when source has no duration, got %v", c.EndedAt)
	}
	if c.Machine != "hist:zsh" || c.Host != "" {
		t.Errorf("machine/host: %q %q", c.Machine, c.Host)
	}
	if len(c.Output) != 0 {
		t.Errorf("imports have no output, got %q", c.Output)
	}
}
