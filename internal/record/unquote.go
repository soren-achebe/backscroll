package record

import "unicode/utf8"

// shellUnquote decodes shell-quoted text as produced by printf %q (bash)
// and print -f %q / ${(q)...} (zsh). kitty's shell integration attaches
// the command line to its OSC 133;C mark in exactly this form:
//
//	bash: \e]133;C;cmdline=%q\a   e.g.  echo\ \"hi\"   or  $'line1\nline2'
//	zsh:  same, but zsh mixes segments:  line1$'\n'line2
//
// The input is a concatenation of segments: $'...' (ANSI-C quoting),
// '...' (literal), "..." (double quotes), backslash escapes, and plain
// runs. Decoding is best-effort: an unterminated quote consumes to the
// end of input, and unknown $'...' escapes are kept literally — for a
// display/search hint, a slightly-off decode beats dropping the command.
func shellUnquote(s string) string {
	b := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			b = append(b, s[i+1])
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			i += 2
			for i < len(s) {
				if s[i] == '\'' {
					i++
					break
				}
				if s[i] == '\\' && i+1 < len(s) {
					dec, n := ansiCEscape(s[i:])
					b = append(b, dec...)
					i += n
				} else {
					b = append(b, s[i])
					i++
				}
			}
		case c == '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				b = append(b, s[i])
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
		case c == '"':
			i++
			for i < len(s) {
				if s[i] == '"' {
					i++
					break
				}
				// Inside double quotes, backslash only escapes $ ` " \.
				if s[i] == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case '$', '`', '"', '\\':
						b = append(b, s[i+1])
						i += 2
						continue
					}
				}
				b = append(b, s[i])
				i++
			}
		default:
			b = append(b, c)
			i++
		}
	}
	return string(b)
}

// ansiCEscape decodes one backslash escape inside $'...' quoting.
// s starts with the backslash; returns the decoded bytes and how many
// input bytes were consumed. Unknown escapes are returned literally.
func ansiCEscape(s string) ([]byte, int) {
	// s[0] == '\\' and len(s) >= 2, guaranteed by the caller.
	c := s[1]
	simple := map[byte]byte{
		'a': 0x07, 'b': 0x08, 'e': 0x1b, 'E': 0x1b, 'f': 0x0c,
		'n': 0x0a, 'r': 0x0d, 't': 0x09, 'v': 0x0b,
		'\\': '\\', '\'': '\'', '"': '"', '?': '?',
	}
	if dec, ok := simple[c]; ok {
		return []byte{dec}, 2
	}
	hexVal := func(b byte) (int, bool) {
		switch {
		case b >= '0' && b <= '9':
			return int(b - '0'), true
		case b >= 'a' && b <= 'f':
			return int(b-'a') + 10, true
		case b >= 'A' && b <= 'F':
			return int(b-'A') + 10, true
		}
		return 0, false
	}
	switch {
	case c == 'x': // \xHH — one or two hex digits
		v, n := 0, 0
		for n < 2 && 2+n < len(s) {
			h, ok := hexVal(s[2+n])
			if !ok {
				break
			}
			v = v<<4 | h
			n++
		}
		if n > 0 {
			return []byte{byte(v)}, 2 + n
		}
	case c == 'u' || c == 'U': // \uHHHH / \UHHHHHHHH — code point
		max := 4
		if c == 'U' {
			max = 8
		}
		v, n := 0, 0
		for n < max && 2+n < len(s) {
			h, ok := hexVal(s[2+n])
			if !ok {
				break
			}
			v = v<<4 | h
			n++
		}
		if n > 0 && utf8.ValidRune(rune(v)) {
			return []byte(string(rune(v))), 2 + n
		}
	case c >= '0' && c <= '7': // \NNN — up to three octal digits
		v, n := 0, 0
		for n < 3 && 1+n < len(s) && s[1+n] >= '0' && s[1+n] <= '7' {
			v = v<<3 | int(s[1+n]-'0')
			n++
		}
		return []byte{byte(v)}, 1 + n
	case c == 'c' && len(s) >= 3: // \cX — control char
		return []byte{s[2] & 0x1f}, 3
	}
	// Unknown escape: keep it literally.
	return []byte{'\\', c}, 2
}

// stripSpace removes ASCII whitespace. WezTerm's shell integration
// base64-encodes user vars with `echo -n ... | base64`, which wraps at
// 76 columns — so long command lines arrive with raw newlines embedded
// in the OSC payload.
func stripSpace(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}
