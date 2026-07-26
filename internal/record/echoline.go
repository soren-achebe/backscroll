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
	echoCap      = 32 << 10 // parser-side cap on a captured B..C region
	echoMaxRows  = 512      // emulator-side cap on grid height
	defaultWidth = 80
)

// erow is one physical grid row. Cells hold 0 for "never written" —
// distinct from a typed space — which is what lets extraction tell a
// cursor-jump gap (decoration, e.g. a zsh RPROMPT) from real spacing.
type erow struct {
	cells []rune
	wrap  bool // continuation of the previous row via autowrap, not \n
}

type echoScreen struct {
	width          int
	rows           []*erow
	r, c           int
	savedR, savedC int
	alt            bool // inside the alternate screen (fzf etc.): freeze
	bad            bool // saw a sequence we can't model; abort
}

// ReconstructEcho replays the echoed bytes of one input region and
// returns the reconstructed command line, or "" if nothing legible
// remains (or the stream used sequences the model cannot follow).
func ReconstructEcho(b []byte, width int) string {
	if width <= 0 {
		width = defaultWidth
	}
	s := &echoScreen{width: width}
	s.feed(b)
	if s.bad {
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
		s.rows = append(s.rows, &erow{cells: make([]rune, s.width)})
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
	}
	s.c++
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
		stStr    // DCS/APC/PM/SOS: skip until ST
		stStrEsc
		stCharset // ESC ( ) * + — consume one designation byte
	)
	st := stNorm
	var csi []byte
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
				st = stNorm
			} else if ch == 0x1b {
				st = stStrEsc
			}
		case stStrEsc:
			if ch == '\\' {
				st = stNorm
			} else {
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
			for i := s.c; i < len(row.cells); i++ {
				row.cells[i] = 0
			}
		case 1:
			for i := 0; i <= s.c && i < len(row.cells); i++ {
				row.cells[i] = 0
			}
		case 2:
			for i := range row.cells {
				row.cells[i] = 0
			}
		}
	case 'J':
		k := csiParams(params, 0)[0]
		switch k {
		case 0: // cursor to end of screen
			row := s.row()
			for i := s.c; i < len(row.cells); i++ {
				row.cells[i] = 0
			}
			if s.r+1 < len(s.rows) {
				s.rows = s.rows[:s.r+1]
			}
		case 1: // start of screen to cursor
			for ri := 0; ri < s.r && ri < len(s.rows); ri++ {
				for i := range s.rows[ri].cells {
					s.rows[ri].cells[i] = 0
				}
			}
			row := s.row()
			for i := 0; i <= s.c && i < len(row.cells); i++ {
				row.cells[i] = 0
			}
		default: // 2, 3: everything
			s.rows = s.rows[:0]
		}
	case 'P': // delete chars (shift left)
		row := s.row()
		if s.c < len(row.cells) {
			copy(row.cells[s.c:], row.cells[min(s.c+n, len(row.cells)):])
			for i := len(row.cells) - min(n, len(row.cells)-s.c); i < len(row.cells); i++ {
				row.cells[i] = 0
			}
		}
	case '@': // insert blanks (shift right)
		row := s.row()
		if s.c < len(row.cells) {
			copy(row.cells[min(s.c+n, len(row.cells)):], row.cells[s.c:])
			for i := s.c; i < min(s.c+n, len(row.cells)); i++ {
				row.cells[i] = ' '
			}
		}
	case 'X': // erase chars (no shift)
		row := s.row()
		for i := s.c; i < min(s.c+n, len(row.cells)); i++ {
			row.cells[i] = 0
		}
	case 'L': // insert lines
		for i := 0; i < n && len(s.rows) < echoMaxRows; i++ {
			s.rows = append(s.rows, nil)
			copy(s.rows[s.r+1:], s.rows[s.r:])
			s.rows[s.r] = &erow{cells: make([]rune, s.width)}
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
func (s *echoScreen) extract() string {
	var logical []string
	var cur strings.Builder
	flushLogical := func() {
		logical = append(logical, strings.TrimRight(cur.String(), " "))
		cur.Reset()
	}
	for ri, row := range s.rows {
		cells := s.trimDecoration(row.cells)
		full := ri+1 < len(s.rows) && s.rows[ri+1].wrap
		// find last written cell
		last := -1
		for i, r := range cells {
			if r != 0 {
				last = i
			}
		}
		if full {
			// a wrapped continuation follows: contribute the whole row,
			// unset cells as spaces (they are interior)
			for _, r := range cells {
				if r == 0 {
					r = ' '
				}
				cur.WriteRune(r)
			}
			continue
		}
		for i := 0; i <= last; i++ {
			r := cells[i]
			if r == 0 {
				r = ' '
			}
			cur.WriteRune(r)
		}
		flushLogical()
	}
	if cur.Len() > 0 {
		flushLogical()
	}
	// drop trailing empty lines (the \r\n echoed by Enter)
	for len(logical) > 0 && strings.TrimSpace(logical[len(logical)-1]) == "" {
		logical = logical[:len(logical)-1]
	}
	// drop leading empty lines (prompt-drawing artifacts)
	for len(logical) > 0 && strings.TrimSpace(logical[0]) == "" {
		logical = logical[1:]
	}
	out := strings.Join(logical, "\n")
	return strings.TrimRight(out, " \t\r\n")
}

// trimDecoration drops a right-aligned trailing segment that is separated
// from the input text by a run of never-written cells — the signature of
// a decoration drawn with a cursor jump (zsh RPROMPT, transient right-side
// widgets). Typed spaces mark their cells as written, so real commands
// with big interior spacing are not affected.
func (s *echoScreen) trimDecoration(cells []rune) []rune {
	last := -1
	for i, r := range cells {
		if r != 0 {
			last = i
		}
	}
	if last < s.width-2 {
		return cells // doesn't reach the right edge: not a right-prompt
	}
	// scan backwards from the segment for a gap of >= 3 unset cells
	segStart := last
	for segStart > 0 && cells[segStart-1] != 0 {
		segStart--
	}
	gapEnd := segStart // exclusive
	gapStart := gapEnd
	for gapStart > 0 && cells[gapStart-1] == 0 {
		gapStart--
	}
	if gapStart == 0 || gapEnd-gapStart < 3 {
		return cells // no preceding text, or gap too small to be a jump
	}
	trimmed := make([]rune, len(cells))
	copy(trimmed, cells[:gapStart])
	return trimmed
}
