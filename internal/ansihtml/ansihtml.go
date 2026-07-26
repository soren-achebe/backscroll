// Package ansihtml renders raw terminal output (with ANSI/VT escape
// sequences) as safe HTML. SGR attributes become <span> elements with
// classes (16-color palette, bold/dim/italic/underline/strike) or inline
// styles (256-color and truecolor). All other escape sequences are
// dropped, and carriage-return overwrites are resolved per line the same
// way internal/ansi.Strip does, so progress bars collapse to their final
// state instead of bloating the page.
package ansihtml

import (
	"fmt"
	"strings"
)

type style struct {
	fg, bg    string // "" | "cN" (palette class) | "#rrggbb"
	bold      bool
	dim       bool
	italic    bool
	underline bool
	strike    bool
	reverse   bool
}

func (s style) zero() bool { return s == style{} }

type seg struct {
	st  style
	txt []byte
}

// Render converts terminal output to HTML. The result contains only
// text, <span> and newlines and is safe to embed inside <pre>.
func Render(b []byte) string {
	var out strings.Builder
	out.Grow(len(b) + len(b)/4)

	var cur style
	var line []seg // current line, rebuilt on \r overwrite

	put := func(c byte) {
		if n := len(line); n > 0 && line[n-1].st == cur {
			line[n-1].txt = append(line[n-1].txt, c)
			return
		}
		line = append(line, seg{st: cur, txt: []byte{c}})
	}
	flush := func(nl bool) {
		for _, sg := range line {
			writeSeg(&out, sg)
		}
		line = line[:0]
		if nl {
			out.WriteByte('\n')
		}
	}

	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == 0x1b:
			i++
			if i >= len(b) {
				break
			}
			switch b[i] {
			case '[': // CSI
				i++
				start := i
				for i < len(b) && !(b[i] >= 0x40 && b[i] <= 0x7e) {
					i++
				}
				if i < len(b) && b[i] == 'm' {
					cur = applySGR(cur, string(b[start:i]))
				}
				i++
			case ']': // OSC ... BEL or ST
				i++
				for i < len(b) {
					if b[i] == 0x07 {
						i++
						break
					}
					if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			case 'P', 'X', '^', '_': // DCS/SOS/PM/APC ... ST
				i++
				for i < len(b) {
					if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			case '(', ')', '#':
				i += 2
			default:
				i++
			}
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
				continue
			}
			line = line[:0] // overwrite current line
			i++
		case c == '\n':
			flush(true)
			i++
		case c == '\t':
			put('\t')
			i++
		case c == 0x08: // backspace
			for n := len(line); n > 0; n = len(line) {
				sg := &line[n-1]
				if len(sg.txt) > 0 {
					sg.txt = sg.txt[:len(sg.txt)-1]
					if len(sg.txt) == 0 {
						line = line[:n-1]
					}
					break
				}
				line = line[:n-1]
			}
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			put(c)
			i++
		}
	}
	flush(false)
	return out.String()
}

func writeSeg(out *strings.Builder, sg seg) {
	if len(sg.txt) == 0 {
		return
	}
	st := sg.st
	if st.reverse {
		// approximate: swap colors; plain reverse gets a class
		st.fg, st.bg = st.bg, st.fg
		if st.fg == "" && st.bg == "" {
			st.reverse = true
		} else {
			st.reverse = false
		}
	}
	var classes []string
	var css []string
	addColor := func(v, prefix, prop string) {
		if v == "" {
			return
		}
		if strings.HasPrefix(v, "#") {
			css = append(css, prop+":"+v)
		} else {
			classes = append(classes, prefix+v)
		}
	}
	addColor(st.fg, "f", "color")
	addColor(st.bg, "b", "background")
	if st.bold {
		classes = append(classes, "bo")
	}
	if st.dim {
		classes = append(classes, "di")
	}
	if st.italic {
		classes = append(classes, "it")
	}
	if st.underline {
		classes = append(classes, "ul")
	}
	if st.strike {
		classes = append(classes, "st")
	}
	if st.reverse {
		classes = append(classes, "rv")
	}
	open := len(classes) > 0 || len(css) > 0
	if open {
		out.WriteString(`<span`)
		if len(classes) > 0 {
			out.WriteString(` class="` + strings.Join(classes, " ") + `"`)
		}
		if len(css) > 0 {
			out.WriteString(` style="` + strings.Join(css, ";") + `"`)
		}
		out.WriteByte('>')
	}
	escapeTo(out, sg.txt)
	if open {
		out.WriteString(`</span>`)
	}
}

func escapeTo(out *strings.Builder, b []byte) {
	for _, c := range b {
		switch c {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		default:
			out.WriteByte(c)
		}
	}
}

// applySGR updates st according to a semicolon/colon separated SGR
// parameter string (the bytes between "ESC[" and "m").
func applySGR(st style, params string) style {
	if params == "" {
		return style{}
	}
	// colon sub-parameters (e.g. 38:5:196) — normalize to semicolons
	params = strings.ReplaceAll(params, ":", ";")
	parts := strings.Split(params, ";")
	toInt := func(s string) int {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				return -1
			}
			n = n*10 + int(r-'0')
			if n > 1<<24 {
				return -1
			}
		}
		return n
	}
	for i := 0; i < len(parts); i++ {
		switch n := toInt(parts[i]); {
		case n == 0:
			st = style{}
		case n == 1:
			st.bold, st.dim = true, false
		case n == 2:
			st.dim, st.bold = true, false
		case n == 3:
			st.italic = true
		case n == 4:
			st.underline = true
		case n == 7:
			st.reverse = true
		case n == 9:
			st.strike = true
		case n == 21 || n == 22:
			st.bold, st.dim = false, false
		case n == 23:
			st.italic = false
		case n == 24:
			st.underline = false
		case n == 27:
			st.reverse = false
		case n == 29:
			st.strike = false
		case n >= 30 && n <= 37:
			st.fg = fmt.Sprintf("%d", n-30)
		case n == 38 || n == 48:
			v, adv := extColor(parts[i+1:], toInt)
			if adv == 0 {
				return st // malformed; bail on the rest
			}
			if n == 38 {
				st.fg = v
			} else {
				st.bg = v
			}
			i += adv
		case n == 39:
			st.fg = ""
		case n >= 40 && n <= 47:
			st.bg = fmt.Sprintf("%d", n-40)
		case n == 49:
			st.bg = ""
		case n >= 90 && n <= 97:
			st.fg = fmt.Sprintf("%d", n-90+8)
		case n >= 100 && n <= 107:
			st.bg = fmt.Sprintf("%d", n-100+8)
		}
	}
	return st
}

// extColor parses the tail of a 38/48 extended color: "5;N" or "2;R;G;B".
// Returns the color value and how many params were consumed (0 = bad).
func extColor(rest []string, toInt func(string) int) (string, int) {
	if len(rest) >= 2 && toInt(rest[0]) == 5 {
		n := toInt(rest[1])
		if n < 0 || n > 255 {
			return "", 0
		}
		if n < 16 {
			return fmt.Sprintf("%d", n), 2
		}
		return palette256(n), 2
	}
	if len(rest) >= 4 && toInt(rest[0]) == 2 {
		r, g, b := toInt(rest[1]), toInt(rest[2]), toInt(rest[3])
		if r < 0 || r > 255 || g < 0 || g > 255 || b < 0 || b > 255 {
			return "", 0
		}
		return fmt.Sprintf("#%02x%02x%02x", r, g, b), 4
	}
	return "", 0
}

// palette256 returns the CSS hex for xterm 256-color indexes 16..255.
func palette256(n int) string {
	if n < 232 { // 6x6x6 cube
		n -= 16
		steps := []int{0, 95, 135, 175, 215, 255}
		r := steps[n/36]
		g := steps[(n/6)%6]
		b := steps[n%6]
		return fmt.Sprintf("#%02x%02x%02x", r, g, b)
	}
	// grayscale ramp
	v := 8 + (n-232)*10
	return fmt.Sprintf("#%02x%02x%02x", v, v, v)
}
