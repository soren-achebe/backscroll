package ansi

// Strip removes ANSI/VT escape sequences and control characters from b,
// returning printable text suitable for indexing or plain display.
// Carriage-return overwrites are resolved per line (last write wins is
// approximated by dropping the overwritten prefix), so progress bars don't
// bloat the result.
func Strip(b []byte) []byte {
	out := make([]byte, 0, len(b))
	lineStart := 0
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
			case '[': // CSI ... final byte 0x40-0x7e
				i++
				for i < len(b) && !(b[i] >= 0x40 && b[i] <= 0x7e) {
					i++
				}
				i++
			case ']': // OSC ... BEL or ESC \
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
			case '(', ')', '#': // charset etc: ESC + 2 bytes
				i += 2
			default:
				i++
			}
		case c == '\r':
			// overwrite current line: drop what we wrote since lineStart
			// unless the CR is immediately followed by \n (normal CRLF).
			if i+1 < len(b) && b[i+1] == '\n' {
				i++ // let the \n case handle it
				continue
			}
			out = out[:lineStart]
			i++
		case c == '\n':
			out = append(out, '\n')
			lineStart = len(out)
			i++
		case c == '\t':
			out = append(out, ' ')
			i++
		case c == 0x08: // backspace
			if len(out) > lineStart {
				out = out[:len(out)-1]
			}
			i++
		case c < 0x20 || c == 0x7f:
			i++ // other control chars
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}
