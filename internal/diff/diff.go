// Package diff implements a minimal Myers line diff with unified output.
// No external dependencies; good enough for terminal-output-sized inputs.
package diff

import (
	"fmt"
	"strings"
)

// Op is a single diff operation over whole lines.
type Op struct {
	Kind byte // ' ' equal, '-' delete (from a), '+' insert (from b)
	Line string
}

// Lines computes a line-level diff between a and b using the Myers
// O(ND) algorithm. Inputs are split on '\n'; a trailing newline does
// not produce a phantom empty line.
func Lines(a, b string) []Op {
	al := splitLines(a)
	bl := splitLines(b)
	return myers(al, bl)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func myers(a, b []string) []Op {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil
	}
	// v[k] = furthest x on diagonal k; store trace for backtracking.
	off := max
	v := make([]int, 2*max+1)
	var trace [][]int
	var d int
outer:
	for d = 0; d <= max; d++ {
		vc := make([]int, len(v))
		copy(vc, v)
		trace = append(trace, vc)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: insert from b
			} else {
				x = v[off+k-1] + 1 // right: delete from a
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				break outer
			}
		}
	}
	// Backtrack.
	var ops []Op
	x, y := n, m
	for step := d; step > 0; step-- {
		vprev := trace[step]
		k := x - y
		var pk int
		if k == -step || (k != step && vprev[off+k-1] < vprev[off+k+1]) {
			pk = k + 1 // came from down (insert)
		} else {
			pk = k - 1 // came from right (delete)
		}
		px := vprev[off+pk]
		py := px - pk
		for x > px && y > py { // snake
			x--
			y--
			ops = append(ops, Op{' ', a[x]})
		}
		if step > 0 {
			if x == px { // insert
				y--
				ops = append(ops, Op{'+', b[y]})
			} else { // delete
				x--
				ops = append(ops, Op{'-', a[x]})
			}
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		ops = append(ops, Op{' ', a[x]})
	}
	reverse(ops)
	return ops
}

func reverse(ops []Op) {
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
}

// Unified renders ops as a unified diff with the given number of
// context lines. labelA/labelB go on the ---/+++ header. If color is
// true, ANSI colors are applied. Returns "" when there are no changes.
func Unified(ops []Op, labelA, labelB string, context int, color bool) string {
	changed := false
	for _, o := range ops {
		if o.Kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	red, green, cyan, bold, reset := "", "", "", "", ""
	if color {
		red, green, cyan, bold, reset = "\x1b[31m", "\x1b[32m", "\x1b[36m", "\x1b[1m", "\x1b[0m"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s--- %s%s\n", bold, labelA, reset)
	fmt.Fprintf(&sb, "%s+++ %s%s\n", bold, labelB, reset)

	// Build hunks: groups of changes with <=context equal lines around.
	type hunk struct{ start, end int } // op index range [start,end)
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].Kind == ' ' {
			i++
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		gap := 0
		for j := i; j < len(ops); j++ {
			if ops[j].Kind != ' ' {
				end = j + 1
				gap = 0
			} else {
				gap++
				if gap > 2*context {
					break
				}
			}
		}
		e := end + context
		if e > len(ops) {
			e = len(ops)
		}
		hunks = append(hunks, hunk{start, e})
		i = e
	}

	// Line numbers: walk ops tracking a/b positions.
	aln, bln := 1, 1
	pos := 0
	advance := func(to int) {
		for ; pos < to; pos++ {
			switch ops[pos].Kind {
			case ' ':
				aln++
				bln++
			case '-':
				aln++
			case '+':
				bln++
			}
		}
	}
	for _, h := range hunks {
		advance(h.start)
		aStart, bStart := aln, bln
		aCount, bCount := 0, 0
		for j := h.start; j < h.end; j++ {
			switch ops[j].Kind {
			case ' ':
				aCount++
				bCount++
			case '-':
				aCount++
			case '+':
				bCount++
			}
		}
		fmt.Fprintf(&sb, "%s@@ -%d,%d +%d,%d @@%s\n", cyan, aStart, aCount, bStart, bCount, reset)
		for j := h.start; j < h.end; j++ {
			o := ops[j]
			switch o.Kind {
			case ' ':
				sb.WriteString(" " + o.Line + "\n")
			case '-':
				sb.WriteString(red + "-" + o.Line + reset + "\n")
			case '+':
				sb.WriteString(green + "+" + o.Line + reset + "\n")
			}
		}
		advance(h.end)
	}
	return sb.String()
}
