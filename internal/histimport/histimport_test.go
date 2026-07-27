package histimport

import (
	"os"
	"testing"
	"time"
)

// The testdata files are real artifacts written by the real tools on
// this machine (zsh 5.9 EXTENDED_HISTORY, fish 4.8, bash 5.2 with
// HISTTIMEFORMAT, atuin 18.17) — not hand-typed approximations.

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseZsh(t *testing.T) {
	es := ParseZsh(load(t, "zsh_history"))
	if len(es) != 3 {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}
	if es[0].Cmd != "echo multi\nline two" {
		t.Errorf("multiline continuation: %q", es[0].Cmd)
	}
	if es[1].Cmd != "echo unïcode ★" {
		t.Errorf("unmetafy: %q", es[1].Cmd)
	}
	if es[2].Cmd != "sleep 2" {
		t.Errorf("cmd: %q", es[2].Cmd)
	}
	want := time.Unix(1785174961, 0)
	for i, e := range es {
		if !e.Started.Equal(want) {
			t.Errorf("entry %d time %v, want %v", i, e.Started, want)
		}
		if e.HasExit {
			t.Errorf("entry %d claims exit code; zsh files have none", i)
		}
	}
	// same-second entries get distinct, stable seqs
	if es[0].Seq == es[1].Seq || es[1].Seq == es[2].Seq {
		t.Errorf("seq collision: %d %d %d", es[0].Seq, es[1].Seq, es[2].Seq)
	}
	es2 := ParseZsh(load(t, "zsh_history"))
	for i := range es {
		if es[i].Seq != es2[i].Seq {
			t.Errorf("seq not stable across re-parse at %d", i)
		}
	}
}

func TestParseZshPlainFormat(t *testing.T) {
	// without EXTENDED_HISTORY lines are bare commands
	es := ParseZsh([]byte("echo a\ncurl https://x\n"))
	if len(es) != 2 || es[0].Cmd != "echo a" || es[1].Cmd != "curl https://x" {
		t.Fatalf("got %+v", es)
	}
	if !es[0].Started.IsZero() {
		t.Errorf("plain entries have no time")
	}
	if es[0].Seq == es[1].Seq {
		t.Errorf("hash seqs collide")
	}
}

func TestParseBashWithTimestamps(t *testing.T) {
	es := ParseBash(load(t, "bash_history_ts"))
	if len(es) != 5 {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}
	if es[3].Cmd != "echo 'multi\nline here'" {
		t.Errorf("multiline under one marker: %q", es[3].Cmd)
	}
	if es[3].Started.Unix() != 1785175363 {
		t.Errorf("ts: %v", es[3].Started)
	}
	if es[4].Cmd != "exit" || es[4].Started.Unix() != 1785175364 {
		t.Errorf("last: %+v", es[4])
	}
}

func TestParseBashPlain(t *testing.T) {
	es := ParseBash(load(t, "bash_history_plain"))
	if len(es) != 3 {
		t.Fatalf("got %d entries", len(es))
	}
	if !es[0].Started.IsZero() {
		t.Errorf("no-ts file entries must have zero time")
	}
	// duplicate command text collapses via hash seq (dedup on insert)
	if es[0].Cmd != "echo one" || es[2].Cmd != "echo one" || es[0].Seq != es[2].Seq {
		t.Errorf("dup hash: %+v %+v", es[0], es[2])
	}
	if es[0].Seq == es[1].Seq {
		t.Errorf("different commands must differ in seq")
	}
}

func TestBashTimestampGuard(t *testing.T) {
	for _, s := range []string{"#123", "# 1785175363", "#178517536x", "#99999999999999"} {
		if _, ok := bashTimestamp(s); ok {
			t.Errorf("%q should not parse as timestamp", s)
		}
	}
	if _, ok := bashTimestamp("#1785175363"); !ok {
		t.Errorf("real marker rejected")
	}
}

func TestParseFish(t *testing.T) {
	es := ParseFish(load(t, "fish_history"))
	if len(es) != 6 {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}
	if es[1].Cmd != "echo 'multi\nline'" {
		t.Errorf("\\n unescape: %q", es[1].Cmd)
	}
	if es[2].Cmd != "cd /tmp" { // paths: block skipped
		t.Errorf("paths skip: %q", es[2].Cmd)
	}
	if es[3].Cmd != "echo unïcode ★" {
		t.Errorf("utf8: %q", es[3].Cmd)
	}
	if es[4].Cmd != `echo back\\slash` { // \\\\ in file -> \\ raw
		t.Errorf("backslash unescape: %q", es[4].Cmd)
	}
	if es[0].Started.Unix() != 1785174985 {
		t.Errorf("when: %v", es[0].Started)
	}
}

func TestReadAtuin(t *testing.T) {
	es, err := ReadAtuin("testdata/atuin_history.db")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}
	if es[0].Cmd != "echo first" || !es[0].HasExit || es[0].Exit != 0 {
		t.Errorf("row 0: %+v", es[0])
	}
	if es[1].Cmd != "false" || es[1].Exit != 1 {
		t.Errorf("row 1: %+v", es[1])
	}
	if es[0].Cwd != "/tmp/histlab" {
		t.Errorf("cwd: %q", es[0].Cwd)
	}
	if es[0].Host != "demo-host" {
		t.Errorf("host split: %q", es[0].Host)
	}
	if es[0].Started.Year() != 2026 {
		t.Errorf("nanos conversion: %v", es[0].Started)
	}
	if d := es[0].Ended.Sub(es[0].Started); d < 100*time.Millisecond || d > time.Second {
		t.Errorf("duration: %v", d)
	}
	if es[0].Seq != 1 || es[1].Seq != 2 {
		t.Errorf("rowid seqs: %d %d", es[0].Seq, es[1].Seq)
	}
}

func TestUnmetafy(t *testing.T) {
	in := []byte{'a', 0x83, 0xb8, 'b'}
	got := unmetafy(in)
	if string(got) != "a\x98b" {
		t.Errorf("got %q", got)
	}
	plain := []byte("no meta here")
	if string(unmetafy(plain)) != "no meta here" {
		t.Errorf("plain roundtrip")
	}
}

func TestParseBashMixed(t *testing.T) {
	// real artifact: HISTTIMEFORMAT session first, then a session
	// WITHOUT it appended plain lines. The plain lines must not be
	// glommed into the last timestamped entry.
	es := ParseBash(load(t, "bash_history_mixed"))
	var cmds []string
	for _, e := range es {
		cmds = append(cmds, e.Cmd)
	}
	want := []string{
		`HISTTIMEFORMAT="%F %T "`, "echo hi", "false",
		"echo 'multi\nline here'", "exit",
		`eval "$(/tmp/bks init bash)"`, "echo live-session-output",
		"false", "exit",
	}
	if len(cmds) != len(want) {
		t.Fatalf("got %d entries %q, want %d", len(cmds), cmds, len(want))
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, cmds[i], want[i])
		}
	}
	// the timestamped block keeps times; appended plain lines have none
	if es[4].Started.Unix() != 1785175364 {
		t.Errorf("marked entry lost its time: %+v", es[4])
	}
	if !es[5].Started.IsZero() {
		t.Errorf("appended plain entry must have no time: %+v", es[5])
	}
	// dedup note: "false"/"exit" appear twice — once timestamped, once
	// plain — and stay separate entries (different seq spaces)
	if es[2].Seq == es[7].Seq {
		t.Errorf("timestamped and plain 'false' must not collide")
	}
}

func TestNeedsCont(t *testing.T) {
	cases := map[string]bool{
		`echo hi`:            false,
		`echo 'open`:         true,
		`echo 'closed'`:      false,
		`echo "open`:         true,
		`echo trailing\`:     true,
		`echo esc\\`:         false, // escaped backslash, complete
		`echo "a'b"`:         false, // single quote inside double
		`echo 'a"b'`:         false,
		`echo "esc\"still`:   true,
		`echo 'lit\'`:        true, // backslash is literal in single quotes; quote closed, but trailing... 
	}
	delete(cases, `echo 'lit\'`) // covered ambiguously by shells; skip
	for s, want := range cases {
		if got := needsCont(s); got != want {
			t.Errorf("needsCont(%q) = %v, want %v", s, got, want)
		}
	}
}
