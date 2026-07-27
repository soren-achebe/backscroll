package main

import (
	"strings"
	"testing"
)

func TestGrepContextBasic(t *testing.T) {
	text := "a\nb\nERROR here\nc\nd\ne\nERROR again\nf\n"
	hunks, shown, total := grepContext(text, "error", 1, 1, 10)
	if total != 2 || shown != 2 {
		t.Fatalf("total=%d shown=%d, want 2/2", total, shown)
	}
	if len(hunks) != 2 {
		t.Fatalf("hunks=%d, want 2 (non-adjacent)", len(hunks))
	}
	h := hunks[0]
	if h.Start != 1 || len(h.Lines) != 3 {
		t.Fatalf("hunk0 start=%d lines=%v", h.Start, h.Lines)
	}
	if !h.IsMatch[1] || h.IsMatch[0] || h.IsMatch[2] {
		t.Fatalf("hunk0 IsMatch=%v", h.IsMatch)
	}
	if h.Lines[1] != "ERROR here" {
		t.Fatalf("hunk0 match line=%q", h.Lines[1])
	}
	if hunks[1].Start != 5 {
		t.Fatalf("hunk1 start=%d, want 5", hunks[1].Start)
	}
}

func TestGrepContextMerge(t *testing.T) {
	// matches on lines 1 and 3 with context 1 -> ranges [0,2] and [2,4] merge
	text := "x\nmatch\ny\nmatch\nz"
	hunks, shown, total := grepContext(text, "match", 1, 1, 10)
	if len(hunks) != 1 || shown != 2 || total != 2 {
		t.Fatalf("hunks=%d shown=%d total=%d, want 1/2/2", len(hunks), shown, total)
	}
	if hunks[0].Start != 0 || len(hunks[0].Lines) != 5 {
		t.Fatalf("merged hunk: start=%d lines=%d", hunks[0].Start, len(hunks[0].Lines))
	}
	// adjacent (touching) ranges also merge: matches on 0 and 2, ctx 0 -> 2 hunks
	hunks, _, _ = grepContext("m\nx\nm", "m", 0, 0, 10)
	if len(hunks) != 2 {
		t.Fatalf("ctx-0 non-adjacent: hunks=%d, want 2", len(hunks))
	}
	// consecutive matching lines, ctx 0 -> one hunk
	hunks, _, _ = grepContext("m\nm\nx", "m", 0, 0, 10)
	if len(hunks) != 1 || len(hunks[0].Lines) != 2 {
		t.Fatalf("consecutive: %+v", hunks)
	}
}

func TestGrepContextMaxHunksAndCounts(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("hit\nfiller\nfiller\n")
	}
	hunks, shown, total := grepContext(sb.String(), "hit", 0, 0, 3)
	if len(hunks) != 3 || shown != 3 || total != 20 {
		t.Fatalf("hunks=%d shown=%d total=%d, want 3/3/20", len(hunks), shown, total)
	}
}

func TestGrepContextEdges(t *testing.T) {
	// match on first and last line: context clamped, no phantom lines
	hunks, _, total := grepContext("match\nmid\nmatch", "match", 2, 2, 10)
	if total != 2 || len(hunks) != 1 {
		t.Fatalf("edges: hunks=%d total=%d", len(hunks), total)
	}
	if hunks[0].Start != 0 || len(hunks[0].Lines) != 3 {
		t.Fatalf("edges clamp: %+v", hunks[0])
	}
	// trailing newline must not create an empty context line
	hunks, _, _ = grepContext("match\n", "match", 2, 2, 10)
	if len(hunks[0].Lines) != 1 {
		t.Fatalf("trailing newline phantom: %v", hunks[0].Lines)
	}
	// no match
	if h, s, tot := grepContext("nothing here", "zzz", 1, 1, 5); h != nil || s != 0 || tot != 0 {
		t.Fatalf("no-match: %v %d %d", h, s, tot)
	}
	// empty inputs
	if h, _, _ := grepContext("", "x", 1, 1, 5); h != nil {
		t.Fatal("empty text")
	}
	if h, _, _ := grepContext("x", "", 1, 1, 5); h != nil {
		t.Fatal("empty query")
	}
}

func TestGrepContextCaseInsensitive(t *testing.T) {
	_, _, total := grepContext("Connection Refused", "connection refused", 0, 0, 5)
	if total != 1 {
		t.Fatalf("case-insensitive: total=%d", total)
	}
}

func TestHighlightMatches(t *testing.T) {
	got := highlightMatches("a FOO b foo c", "foo", "<", ">")
	if got != "a <FOO> b <foo> c" {
		t.Fatalf("got %q", got)
	}
	if got := highlightMatches("no match", "zzz", "<", ">"); got != "no match" {
		t.Fatalf("unchanged: %q", got)
	}
	// overlapping occurrences: non-overlapping greedy from the left
	if got := highlightMatches("aaa", "aa", "<", ">"); got != "<aa>a" {
		t.Fatalf("overlap: %q", got)
	}
	// multi-byte content around the match must survive
	got = highlightMatches("héllo wörld foo bar", "foo", "<", ">")
	if got != "héllo wörld <foo> bar" {
		t.Fatalf("utf8: %q", got)
	}
}

func TestClipLine(t *testing.T) {
	if got := clipLine("short", 0, 100); got != "short" {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("x", 300)
	got := clipLine(long, 0, 100)
	if len(got) > 110 || !strings.HasSuffix(got, "…") {
		t.Fatalf("head clip: len=%d %q", len(got), got[:20])
	}
	// match far into the line: window must keep it visible
	line := strings.Repeat("a", 250) + "NEEDLE" + strings.Repeat("b", 250)
	got = clipLine(line, 250, 100)
	if !strings.Contains(got, "NEEDLE") {
		t.Fatalf("match clip lost needle: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("match clip marks: %q", got)
	}
	// match near the end: no trailing mark
	line = strings.Repeat("a", 250) + "NEEDLE"
	got = clipLine(line, 250, 100)
	if !strings.Contains(got, "NEEDLE") || !strings.HasPrefix(got, "…") {
		t.Fatalf("tail clip: %q", got)
	}
	// clips land on rune boundaries
	line = strings.Repeat("é", 200) // 2 bytes each
	got = clipLine(line, 0, 101)    // odd width would split a rune
	trimmed := strings.TrimSuffix(got, "…")
	for _, r := range trimmed {
		if r == '\uFFFD' {
			t.Fatalf("split rune in %q", got)
		}
	}
}

func TestMatchOffset(t *testing.T) {
	if off := matchOffset("xxfooxx", "FOO"); off != 2 {
		t.Fatalf("off=%d", off)
	}
	if off := matchOffset("none", "zzz"); off != 0 {
		t.Fatalf("no-match off=%d", off)
	}
}
