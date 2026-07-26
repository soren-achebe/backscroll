package record

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
)

// Events emitted by the parser as it segments the terminal byte stream
// using OSC 133 shell-integration marks (plus a private OSC 6973 mark
// carrying the command text, and OSC 7 carrying the cwd).
type Events struct {
	CmdText  func(cmd string)            // OSC 6973;cmd=<b64> — authoritative (our snippet)
	CmdHint  func(cmd string)            // OSC 133;C;cmdline_url=<pct> — emitter-provided fallback (fish ≥4.0)
	OutStart func()                      // OSC 133;C  — output begins
	OutEnd   func(exitCode int, ok bool) // OSC 133;D[;code]
	Cwd      func(path string)           // OSC 7;file://host/path
	Output   func(b []byte)              // raw output bytes between C and D (alt-screen excluded)
	Toggle   func(on bool)               // OSC 6973;rec=on|off — pause/resume recording
}

type pstate int

const (
	sNorm pstate = iota
	sEsc
	sOsc
	sOscEsc // saw ESC inside OSC (possible ST)
	sCsi
)

// Parser is an incremental scanner over the child PTY's output stream.
// It never modifies the stream (the recorder passes bytes through
// unchanged); it only observes marks and captures output spans.
type Parser struct {
	ev        Events
	st        pstate
	osc       bytes.Buffer
	csi       bytes.Buffer
	capturing bool
	altScreen bool
}

func NewParser(ev Events) *Parser {
	return &Parser{ev: ev}
}

const maxSeqLen = 1 << 16

func (p *Parser) Feed(chunk []byte) {
	out := make([]byte, 0, len(chunk))
	flushOut := func() {
		if len(out) > 0 && p.ev.Output != nil {
			p.ev.Output(out)
		}
		out = out[:0]
	}
	for i := 0; i < len(chunk); i++ {
		b := chunk[i]
		switch p.st {
		case sNorm:
			// Fast path: bulk-scan to the next ESC instead of walking
			// byte-by-byte. Plain output (the vast majority of bytes)
			// is copied in one append.
			j := bytes.IndexByte(chunk[i:], 0x1b)
			if j < 0 {
				if p.capturing && !p.altScreen {
					out = append(out, chunk[i:]...)
				}
				i = len(chunk) // done
				continue
			}
			if p.capturing && !p.altScreen {
				out = append(out, chunk[i:i+j]...)
			}
			i += j // chunk[i] is now the ESC; loop increment skips it
			p.st = sEsc
		case sEsc:
			switch b {
			case ']':
				p.st = sOsc
				p.osc.Reset()
			case '[':
				p.st = sCsi
				p.csi.Reset()
			default:
				// some other escape sequence; pass its bytes into capture
				if p.capturing && !p.altScreen {
					out = append(out, 0x1b, b)
				}
				p.st = sNorm
			}
		case sOsc:
			if b == 0x07 { // BEL terminator
				flushOut()
				p.handleOSC(p.osc.String(), &out)
				p.st = sNorm
			} else if b == 0x1b {
				p.st = sOscEsc
			} else {
				if p.osc.Len() < maxSeqLen {
					p.osc.WriteByte(b)
				}
			}
		case sOscEsc:
			if b == '\\' { // ST terminator
				flushOut()
				p.handleOSC(p.osc.String(), &out)
				p.st = sNorm
			} else {
				// stray ESC inside OSC payload; keep going
				p.osc.WriteByte(0x1b)
				p.osc.WriteByte(b)
				p.st = sOsc
			}
		case sCsi:
			p.csi.WriteByte(b)
			if b >= 0x40 && b <= 0x7e { // final byte
				seq := p.csi.String()
				p.handleCSI(seq)
				if p.capturing && !p.altScreen {
					out = append(out, 0x1b, '[')
					out = append(out, seq...)
				}
				p.st = sNorm
			} else if p.csi.Len() > maxSeqLen {
				p.st = sNorm
			}
		}
	}
	flushOut()
}

func (p *Parser) handleCSI(seq string) {
	// alt-screen tracking: CSI ?1049h/l (also 47, 1047)
	if len(seq) < 2 || seq[0] != '?' {
		return
	}
	body := seq[1 : len(seq)-1]
	final := seq[len(seq)-1]
	for _, param := range strings.Split(body, ";") {
		if param == "1049" || param == "1047" || param == "47" {
			switch final {
			case 'h':
				p.altScreen = true
			case 'l':
				p.altScreen = false
			}
		}
	}
}

func (p *Parser) handleOSC(payload string, out *[]byte) {
	switch {
	case strings.HasPrefix(payload, "133;"):
		rest := payload[4:]
		switch {
		case rest == "C" || strings.HasPrefix(rest, "C;"):
			p.capturing = true
			p.altScreen = false
			if p.ev.OutStart != nil {
				p.ev.OutStart()
			}
			// Some emitters (fish ≥4.0, following the kitty shell-
			// integration protocol) attach the command line itself as a
			// percent-encoded C-mark parameter. Surface it as a hint so
			// sessions without our snippet still get command text.
			if strings.HasPrefix(rest, "C;") && p.ev.CmdHint != nil {
				for _, param := range strings.Split(rest[2:], ";") {
					if enc, found := strings.CutPrefix(param, "cmdline_url="); found {
						p.ev.CmdHint(percentDecode(enc))
					}
				}
			}
		case rest == "D" || strings.HasPrefix(rest, "D;"):
			if p.capturing {
				p.capturing = false
				code, ok := 0, false
				if strings.HasPrefix(rest, "D;") {
					if n, err := strconv.Atoi(strings.SplitN(rest[2:], ";", 2)[0]); err == nil {
						code, ok = n, true
					}
				}
				if p.ev.OutEnd != nil {
					p.ev.OutEnd(code, ok)
				}
			}
		}
		// A and B marks are prompt boundaries; nothing to capture there.
	case strings.HasPrefix(payload, "6973;cmd="):
		if raw, err := base64.StdEncoding.DecodeString(payload[len("6973;cmd="):]); err == nil {
			if p.ev.CmdText != nil {
				p.ev.CmdText(string(raw))
			}
		}
	case payload == "6973;rec=off" || payload == "6973;rec=on":
		if p.ev.Toggle != nil {
			p.ev.Toggle(strings.HasSuffix(payload, "=on"))
		}
	case strings.HasPrefix(payload, "7;"):
		if p.ev.Cwd != nil {
			p.ev.Cwd(parseOSC7(payload[2:]))
		}
	default:
		// Not ours: if we're capturing, put the sequence back into the
		// captured output so replay stays faithful (e.g. title changes,
		// hyperlinks).
		if p.capturing && !p.altScreen {
			*out = append(*out, 0x1b, ']')
			*out = append(*out, payload...)
			*out = append(*out, 0x07)
		}
	}
}

// parseOSC7 turns "file://host/path" into a plain path.
func parseOSC7(s string) string {
	s = strings.TrimPrefix(s, "file://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i:]
	}
	return percentDecode(s)
}

// percentDecode undoes %XX percent-encoding, leaving malformed escapes as-is.
func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
