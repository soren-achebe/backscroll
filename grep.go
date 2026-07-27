package main

import (
	"strings"
	"unicode/utf8"
)

// grep-style context extraction over a command's stored plain output.
// Shared by `backscroll search -A/-B/-C` and the MCP search_output tool's
// context_lines parameter.

type grepHunk struct {
	Start   int      // 0-based index of the first line in Lines
	Lines   []string // the lines of this hunk
	IsMatch []bool   // parallel to Lines: true if the line contains the query
}

// grepContext finds case-insensitive substring matches of query in text and
// returns hunks of matching lines with before/after lines of context,
// merging hunks that touch or overlap (like grep). At most maxHunks hunks
// are returned; total is the number of matching lines in the whole text,
// shown the number included in the returned hunks.
func grepContext(text, query string, before, after, maxHunks int) (hunks []grepHunk, shown, total int) {
	if query == "" || text == "" {
		return nil, 0, 0
	}
	lines := strings.Split(text, "\n")
	// a trailing newline produces one empty final element; drop it so we
	// don't show a phantom empty context line
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	lq := strings.ToLower(query)
	var matchIdx []int
	for i, ln := range lines {
		if strings.Contains(strings.ToLower(ln), lq) {
			matchIdx = append(matchIdx, i)
		}
	}
	total = len(matchIdx)
	if total == 0 {
		return nil, 0, 0
	}
	// merge [i-before, i+after] ranges that touch or overlap
	type span struct{ lo, hi int }
	var spans []span
	for _, i := range matchIdx {
		lo, hi := i-before, i+after
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines)-1 {
			hi = len(lines) - 1
		}
		if n := len(spans); n > 0 && lo <= spans[n-1].hi+1 {
			if hi > spans[n-1].hi {
				spans[n-1].hi = hi
			}
		} else {
			spans = append(spans, span{lo, hi})
		}
	}
	mi := 0
	for _, sp := range spans {
		if len(hunks) >= maxHunks {
			break
		}
		h := grepHunk{Start: sp.lo}
		for i := sp.lo; i <= sp.hi; i++ {
			isM := mi < len(matchIdx) && matchIdx[mi] == i
			if isM {
				mi++
				shown++
			}
			h.Lines = append(h.Lines, lines[i])
			h.IsMatch = append(h.IsMatch, isM)
		}
		hunks = append(hunks, h)
	}
	return hunks, shown, total
}

// clipLine trims a long line to roughly width bytes for display. For match
// lines, pass the byte offset of the first query match so the clip window
// keeps it visible; pass 0 for plain context lines. Cuts land on rune
// boundaries and are marked with "…".
func clipLine(line string, matchOff, width int) string {
	if len(line) <= width {
		return line
	}
	if matchOff <= width-20 {
		return line[:runeFloor(line, width)] + "…"
	}
	start := runeFloor(line, matchOff-30)
	end := start + width
	if end >= len(line) {
		return "…" + line[runeFloor(line, start):]
	}
	return "…" + line[start:runeFloor(line, end)] + "…"
}

// runeFloor returns the largest rune-boundary offset <= i.
func runeFloor(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// highlightMatches wraps every case-insensitive occurrence of query in line
// with on/off strings (ANSI colors for the CLI). Returns the line unchanged
// if there is no occurrence.
func highlightMatches(line, query, on, off string) string {
	if query == "" {
		return line
	}
	ll, lq := strings.ToLower(line), strings.ToLower(query)
	if len(ll) != len(line) || len(lq) != len(query) {
		// lowercasing changed byte lengths (e.g. 'İ'): offsets into ll
		// wouldn't map back to line — fall back to case-sensitive
		ll, lq = line, query
	}
	var b strings.Builder
	pos := 0
	for {
		i := strings.Index(ll[pos:], lq)
		if i < 0 {
			break
		}
		i += pos
		b.WriteString(line[pos:i])
		b.WriteString(on)
		b.WriteString(line[i : i+len(query)])
		b.WriteString(off)
		pos = i + len(query)
	}
	if pos == 0 {
		return line
	}
	b.WriteString(line[pos:])
	return b.String()
}

// matchOffset returns the byte offset of the first case-insensitive
// occurrence of query in line, or 0 if none.
func matchOffset(line, query string) int {
	ll, lq := strings.ToLower(line), strings.ToLower(query)
	if len(ll) != len(line) || len(lq) != len(query) {
		ll, lq = line, query
	}
	i := strings.Index(ll, lq)
	if i < 0 {
		return 0
	}
	return i
}
