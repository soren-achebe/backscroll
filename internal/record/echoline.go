package record

import (
	"strings"
	"unicode/utf8"
)

// Echo reconstruction: recover the command line a user typed from the
// terminal's *echo* of it.
//
// Plain OSC 133 emitters (Ghostty, iTerm2, Windows Terminal, manually
// installed integration scripts) mark prompt/output boundaries but never
// report the command text itself. The recorder still sees everything the
// shell echoed between the B mark (end of prompt / start of input) and the
// C mark (pre-execution) — the typed characters, plus all the line-editor
// noise readline/ZLE produce while the user edits (backspaces, cursor
// motion, redraws, completion menus, fzf popups...).
//
// ReconstructEcho replays those bytes against a tiny terminal-line model
// and returns the final visible input line(s). It is a *hint* of last
// resort: the recorder prefers our snippet's OSC 6973 text, then any
// emitter-provided cmdline (fish ≥4.0, VS Code 633;E, kitty, WezTerm),
// and only then this reconstruction.
//
// The model is deliberately small but honest about its limits: sequences
// that can't be mapped into a prompt-relative grid (absolute cursor
// addressing, scroll regions) abort reconstruction — returning "" (and
// letting the entry fall back to "(unknown command)") beats storing text
// that was never the command.

const (
	echoCap      = 32 << 10 // parser-side cap on a captured echo region
	echoMaxRows  = 512      // emulator-side cap on grid height
	defaultWidth = 80

	// Cell provenance. The prompt is replayed into the grid too — the
	// B mark fires with the cursor *after* the prompt, and line-editor
	// redraws (ZLE's deferred-wrap dance in particular) only make sense
	// from that true cursor position — but only cells written during an
	// input phase belong to the command.
	cellUnset  = 0
	cellPrompt = 1
	cellInput  = 2
)

// Phase sentinels the parser splices into the captured region at mark
// boundaries (APC sequences: invisible to any terminal, never emitted by
// line editors).
var (
	echoSentPrompt = []byte("\x1b_bks:P\x1b\\") // prompt bytes follow (A, 133;P)
	echoSentInput  = []byte("\x1b_bks:B\x1b\\") // input bytes follow (B mark)
)

// erow is one physical grid row. Flags record who wrote each cell
// (unset / prompt / input): a typed space is an input cell — distinct
// from never-written, which is what lets extraction tell a cursor-jump
// gap (decoration, e.g. a zsh RPROMPT) from real spacing.
type erow struct {
	cells []rune
	flags []uint8
	wrap  bool // continuation of the previous row via autowrap, not \n
}

type echoScreen struct {
	width          int
	rows           []*erow
	r, c           int
	savedR, savedC int
	phase          uint8 // provenance for cells written now
	alt            bool  // inside the alternate screen (fzf etc.): freeze
	bad            bool  // saw a sequence we can't model; abort

	// Ctrl-C detection: a line killed at the prompt is echoed as a bare
	// "^C" with no newline after it (an *executed* line always ends
	// with the echoed Enter). Emitters that re-fire a phantom C mark on
	// SIGINT (VS Code's bash hooks) would otherwise turn that echo into
	// a stored "command".
	tail       [2]rune // last two input runes put
	lfAfterPut bool    // a linefeed followed the last put
	ctrlCAbort bool    // bracketed paste ended right after a bare ^C
}

// ReconstructEcho replays the echoed bytes of one input region and
// returns the reconstructed command line, or "" if nothing legible
// remains (or the stream used sequences the model cannot follow).
func ReconstructEcho(b []byte, width int) string {
	if width <= 0 {
		width = defaultWidth
	}
	s := &echoScreen{width: width, phase: cellInput}
	s.feed(b)
	if s.bad {
		return ""
	}
	if s.ctrlCAbort || (s.tail == [2]rune{'^', 'C'} && !s.lfAfterPut) {
		// The input line was killed with Ctrl-C, never executed: the
		// shell echoed a bare "^C" and tore the line editor down right
		// there (an *executed* line always echoes Enter's newline
		// first). Some emitters re-fire a phantom C mark on SIGINT
		// (VS Code's bash hooks) — without this, that echo would be
		// stored as a command named "^C".
		return ""
	}
	return s.extract()
}

func (s *echoScreen) row() *erow {
	for s.r >= len(s.rows) {
		if len(s.rows) >= echoMaxRows {
			s.bad = true
			s.r = len(s.rows) - 1
			break
		}
		s.rows = append(s.rows, &erow{
			cells: make([]rune, s.width),
			flags: make([]uint8, s.width),
		})
	}
	return s.rows[s.r]
}

func (s *echoScreen) put(r rune) {
	if s.alt || s.bad {
		return
	}
	if s.c >= s.width {
		// autowrap: continue the same logical line on the next row
		s.r++
		s.c = 0
		s.row().wrap = true
	}
	row := s.row()
	if s.c < len(row.cells) {
		row.cells[s.c] = r
		row.flags[s.c] = s.phase
	}
	s.c++
	if s.phase == cellInput {
		s.tail[0], s.tail[1] = s.tail[1], r
		s.lfAfterPut = false
	}
}

func (s *echoScreen) down(n int, hard bool) {
	for i := 0; i < n; i++ {
		s.r++
		s.row() // materialize (hard rows: wrap stays false)
		_ = hard
	}
}

func (s *echoScreen) feed(b []byte) {
	const (
		stNorm = iota
		stEsc
		stCsi
		stOsc    // skip until BEL / ST
		stOscEsc // saw ESC inside OSC
		stStr    // DCS/APC/PM/SOS: skip until ST (phase sentinels live here)
		stStrEsc
		stCharset // ESC ( ) * + — consume one designation byte
	)
	st := stNorm
	var csi []byte
	var apc []byte
	endStr := func() {
		switch string(apc) {
		case "bks:P":
			s.phase = cellPrompt
		case "bks:B":
			s.phase = cellInput
		}
		apc = apc[:0]
	}
	for i := 0; i < len(b); i++ {
		ch := b[i]
		switch st {
		case stNorm:
			switch {
			case ch == 0x1b:
				st = stEsc
			case ch == '\r':
				s.applyCR()
			case ch == '\n':
				s.applyLF()
			case ch == '\b':
				s.applyBS()
			case ch == '\t':
				s.applyTab()
			case ch < 0x20 || ch == 0x7f:
				// other C0 / DEL: line-editor internals, not content
			case ch < utf8.RuneSelf:
				s.put(rune(ch))
			default:
				r, size := utf8.DecodeRune(b[i:])
				if r != utf8.RuneError || size > 1 {
					s.put(r)
				}
				i += size - 1
			}
		case stEsc:
			switch ch {
			case '[':
				st = stCsi
				csi = csi[:0]
			case ']':
				st = stOsc
			case 'P', '_', '^', 'X': // DCS/APC/PM/SOS
				st = stStr
				apc = apc[:0]
			case '(', ')', '*', '+':
				st = stCharset
			case '7':
				s.savedR, s.savedC = s.r, s.c
				st = stNorm
			case '8':
				s.r, s.c = s.savedR, s.savedC
				st = stNorm
			case 'M': // reverse index
				if !s.alt && s.r > 0 {
					s.r--
				}
				st = stNorm
			case 'D': // index
				if !s.alt {
					s.down(1, true)
				}
				st = stNorm
			case 'E': // next line
				if !s.alt {
					s.down(1, true)
					s.c = 0
				}
				st = stNorm
			default:
				st = stNorm
			}
		case stCharset:
			st = stNorm
		case stCsi:
			if ch >= 0x40 && ch <= 0x7e {
				s.applyCSI(string(csi), ch)
				st = stNorm
			} else if len(csi) < 256 {
				csi = append(csi, ch)
			}
		case stOsc:
			if ch == 0x07 {
				st = stNorm
			} else if ch == 0x1b {
				st = stOscEsc
			}
		case stOscEsc:
			if ch == '\\' {
				st = stNorm
			} else {
				st = stOsc
			}
		case stStr:
			if ch == 0x07 {
				endStr()
				st = stNorm
			} else if ch == 0x1b {
				st = stStrEsc
			} else if len(apc) < 8 {
				apc = append(apc, ch)
			}
		case stStrEsc:
			if ch == '\\' {
				endStr()
				st = stNorm
			} else {
				apc = apc[:0]
				st = stStr
			}
		}
		if s.bad {
			return
		}
	}
}

func (s *echoScreen) applyCR() {
	if !s.alt {
		s.c = 0
	}
}

func (s *echoScreen) applyLF() {
	if !s.alt {
		s.down(1, true)
		s.lfAfterPut = true
	}
}

func (s *echoScreen) applyBS() {
	if !s.alt && s.c > 0 {
		s.c--
		if s.c >= s.width {
			s.c = s.width - 1
		}
	}
}

func (s *echoScreen) applyTab() {
	if s.alt {
		return
	}
	s.c = (s.c/8 + 1) * 8
	if s.c > s.width-1 {
		s.c = s.width - 1
	}
}

// csiParams parses a CSI parameter string into ints, applying def for
// empty/zero entries.
func csiParams(p string, def int) []int {
	parts := strings.Split(p, ";")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n := 0
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				n = -1
				break
			}
			n = n*10 + int(ch-'0')
			if n > 1<<20 {
				n = 1 << 20
			}
		}
		if n <= 0 {
			n = def
		}
		out = append(out, n)
	}
	return out
}

func (s *echoScreen) applyCSI(params string, final byte) {
	// Private-mode set/reset: only the alt-screen switch matters.
	if strings.HasPrefix(params, "?") {
		if final == 'h' || final == 'l' {
			for _, p := range strings.Split(params[1:], ";") {
				if p == "1049" || p == "1047" || p == "47" {
					s.alt = final == 'h'
				}
				if p == "2004" && final == 'l' &&
					s.tail == [2]rune{'^', 'C'} && !s.lfAfterPut {
					// readline tearing down bracketed paste directly
					// after a bare ^C: the line died at the prompt
					s.ctrlCAbort = true
				}
			}
		}
		return
	}
	if s.alt {
		return
	}
	p := csiParams(params, 1)
	n := p[0]
	switch final {
	case 'A': // up
		s.r -= n
		if s.r < 0 {
			s.r = 0
		}
	case 'B', 'e': // down
		s.down(n, true)
	case 'C', 'a': // right
		s.c += n
		if s.c > s.width {
			s.c = s.width
		}
	case 'D': // left
		s.c -= n
		if s.c < 0 {
			s.c = 0
		}
	case 'G', '`': // column absolute — prompt-relative, safe
		s.c = n - 1
		if s.c > s.width {
			s.c = s.width
		}
	case 'K':
		k := csiParams(params, 0)[0]
		row := s.row()
		switch k {
		case 0:
			row.clear(s.c, len(row.cells))
		case 1:
			row.clear(0, s.c+1)
		case 2:
			row.clear(0, len(row.cells))
		}
	case 'J':
		k := csiParams(params, 0)[0]
		switch k {
		case 0: // cursor to end of screen
			s.row().clear(s.c, s.width)
			if s.r+1 < len(s.rows) {
				s.rows = s.rows[:s.r+1]
			}
		case 1: // start of screen to cursor
			for ri := 0; ri < s.r && ri < len(s.rows); ri++ {
				s.rows[ri].clear(0, s.width)
			}
			s.row().clear(0, s.c+1)
		default: // 2, 3: everything
			s.rows = s.rows[:0]
		}
	case 'P': // delete chars (shift left)
		row := s.row()
		if s.c < len(row.cells) {
			cut := min(s.c+n, len(row.cells))
			copy(row.cells[s.c:], row.cells[cut:])
			copy(row.flags[s.c:], row.flags[cut:])
			row.clear(len(row.cells)-(cut-s.c), len(row.cells))
		}
	case '@': // insert blanks (shift right)
		row := s.row()
		if s.c < len(row.cells) {
			at := min(s.c+n, len(row.cells))
			copy(row.cells[at:], row.cells[s.c:])
			copy(row.flags[at:], row.flags[s.c:])
			for i := s.c; i < at; i++ {
				row.cells[i] = ' '
				row.flags[i] = s.phase
			}
		}
	case 'X': // erase chars (no shift)
		s.row().clear(s.c, min(s.c+n, s.width))
	case 'L': // insert lines
		for i := 0; i < n && len(s.rows) < echoMaxRows; i++ {
			s.rows = append(s.rows, nil)
			copy(s.rows[s.r+1:], s.rows[s.r:])
			s.rows[s.r] = &erow{
				cells: make([]rune, s.width),
				flags: make([]uint8, s.width),
			}
		}
	case 'M': // delete lines
		for i := 0; i < n && s.r < len(s.rows); i++ {
			s.rows = append(s.rows[:s.r], s.rows[s.r+1:]...)
		}
	case 's':
		s.savedR, s.savedC = s.r, s.c
	case 'u':
		s.r, s.c = s.savedR, s.savedC
	case 'H', 'f', 'd', 'r', 'S', 'T':
		// Absolute screen addressing / scroll regions: the grid is
		// prompt-relative, so these can't be mapped. Abort rather than
		// guess — a wrong "command" is worse than no command.
		s.bad = true
	}
	if s.c < 0 {
		s.c = 0
	}
}

// extract renders the final grid into the reconstructed command text.
// Only input cells contribute: the replayed prompt (PS1, and PS2 "> "
// continuation prompts on marked emitters) positioned the cursor
// correctly and is then discarded.
func (s *echoScreen) extract() string {
	type cell struct {
		r rune
		f uint8
	}
	var logical [][]cell
	var cur []cell
	for ri, row := range s.rows {
		s.trimDecoration(row)
		wrapped := ri+1 < len(s.rows) && s.rows[ri+1].wrap
		if wrapped {
			// a continuation follows: the whole row is interior
			for i := range row.cells {
				cur = append(cur, cell{row.cells[i], row.flags[i]})
			}
			continue
		}
		last := -1
		for i, f := range row.flags {
			if f == cellInput {
				last = i
			}
		}
		for i := 0; i <= last; i++ {
			cur = append(cur, cell{row.cells[i], row.flags[i]})
		}
		logical = append(logical, cur)
		cur = nil
	}
	if len(cur) > 0 {
		logical = append(logical, cur)
	}

	var lines []string
	for _, lc := range logical {
		first, last := -1, -1
		for i, c := range lc {
			if c.f == cellInput {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first < 0 {
			lines = append(lines, "")
			continue
		}
		var b strings.Builder
		for i := first; i <= last; i++ {
			r := lc[i].r
			if lc[i].f != cellInput || r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
		lines = append(lines, b.String())
	}
	// drop leading/trailing empty lines (prompt rows, the \r\n echoed
	// by Enter)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n")
}

// trimDecoration erases a right-aligned trailing input segment that is
// separated from the rest of the input by a run of never-written cells —
// the signature of a decoration drawn with a cursor jump (zsh RPROMPT,
// transient right-side widgets). Typed spaces mark their cells as
// written, so real commands with big interior spacing are not affected.
func (s *echoScreen) trimDecoration(row *erow) {
	last := -1
	for i, f := range row.flags {
		if f == cellInput {
			last = i
		}
	}
	if last < s.width-2 {
		return // doesn't reach the right edge: not a right-prompt
	}
	segStart := last
	for segStart > 0 && row.flags[segStart-1] != cellUnset {
		segStart--
	}
	gapEnd := segStart // exclusive
	gapStart := gapEnd
	for gapStart > 0 && row.flags[gapStart-1] == cellUnset {
		gapStart--
	}
	if gapStart == 0 || gapEnd-gapStart < 3 {
		return // no preceding text, or gap too small to be a jump
	}
	row.clear(gapEnd, last+1)
}

// clear resets cells [from, to) to never-written.
func (w *erow) clear(from, to int) {
	if from < 0 {
		from = 0
	}
	if to > len(w.cells) {
		to = len(w.cells)
	}
	for i := from; i < to; i++ {
		w.cells[i] = 0
		w.flags[i] = cellUnset
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
