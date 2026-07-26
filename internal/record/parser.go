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
	Echo     func(b []byte)              // raw input-echo bytes between B and C (see ReconstructEcho)
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
	wezProg   []byte // partial base64 of a word-split WEZTERM_PROG (see handleOSC)

	// Input-echo capture. Plain-133 emitters never report the command
	// text; the echoed keystrokes between the B (input starts) and C
	// (execution starts) marks are the only trace of it. The prompt
	// bytes (from A / 133;P to B) are captured too — reconstruction
	// must replay them to know where the cursor really was when typing
	// began — with phase sentinels spliced in at the mark boundaries.
	// The raw region is handed to ev.Echo at C time.
	echoPhase byte // 0 = off, cellPrompt, cellInput
	echoInput bool // an input phase was entered (a B mark was seen)
	echo      bytes.Buffer
	echoOver  bool // region exceeded echoCap: discard, don't truncate
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
				} else if p.echoPhase != 0 {
					p.echoAdd(chunk[i:])
				}
				i = len(chunk) // done
				continue
			}
			if p.capturing && !p.altScreen {
				out = append(out, chunk[i:i+j]...)
			} else if p.echoPhase != 0 {
				p.echoAdd(chunk[i : i+j])
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
				} else if p.echoPhase != 0 {
					p.echoAdd([]byte{0x1b, b})
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
				} else if p.echoPhase != 0 {
					p.echoAdd([]byte{0x1b, '['})
					p.echoAdd([]byte(seq))
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

func (p *Parser) echoAdd(b []byte) {
	if p.echoOver {
		return
	}
	if p.echo.Len()+len(b) > echoCap {
		// A region this big is a paste gone wrong or UI noise, and a
		// truncated echo reconstructs to the wrong command — discard it
		// entirely rather than keep a misleading prefix.
		p.echoOver = true
		p.echo.Reset()
		return
	}
	p.echo.Write(b)
}

func (p *Parser) echoReset() {
	p.echoPhase = 0
	p.echoInput = false
	p.echoOver = false
	p.echo.Reset()
}

// echoPromptStart begins a fresh capture region at a prompt boundary
// (an A mark, or a 133;P primary-prompt mark — which, unlike A, is
// embedded in PS1 and so re-fires on prompt redraws like Ctrl-L,
// discarding the redraw noise that just polluted the region).
func (p *Parser) echoPromptStart() {
	p.echoReset()
	p.echoPhase = cellPrompt
	p.echoAdd(echoSentPrompt)
}

// markB handles an input-start mark (OSC 133;B / 633;B): user keystrokes
// echo from here until the C mark. Normally a prompt phase is already
// open (A or 133;P preceded it) and the region continues; a B with *no*
// prompt phase is either a bare-B emitter (no A: start fresh) or a
// prompt redraw on such an emitter (B re-fires from PS1: the discarded
// buffer is exactly the redraw noise).
func (p *Parser) markB() {
	if p.echoPhase != cellPrompt {
		p.echoReset()
	}
	p.echoPhase = cellInput
	p.echoInput = true
	p.echoAdd(echoSentInput)
}

// markC handles a pre-execution mark (OSC 133;C / 633;C): output begins.
func (p *Parser) markC() {
	p.capturing = true
	p.altScreen = false
	if p.echoInput && !p.echoOver && p.ev.Echo != nil {
		b := make([]byte, p.echo.Len())
		copy(b, p.echo.Bytes())
		p.ev.Echo(b)
	}
	p.echoReset()
	if p.ev.OutStart != nil {
		p.ev.OutStart()
	}
}

// markD handles a command-finished mark (OSC 133;D / 633;D), rest is the
// payload after "133;"/"633;" (i.e. "D" or "D;<code>[;...]").
func (p *Parser) markD(rest string) {
	p.echoReset() // safety: a D mark always ends any input region
	if !p.capturing {
		return
	}
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

func (p *Parser) handleOSC(payload string, out *[]byte) {
	// A pending word-split WEZTERM_PROG (see the 1337 case below) is
	// continued by the very next OSCs or ended by anything else. Flushing
	// before normal handling means the reassembled hint is in place
	// before a following 133;D triggers the command flush.
	if len(p.wezProg) > 0 {
		if frag, ok := wezContinuation(payload); ok {
			p.wezProg = append(p.wezProg, frag...)
			return
		}
		p.emitWezProg(string(p.wezProg))
		p.wezProg = p.wezProg[:0]
	}
	switch {
	case strings.HasPrefix(payload, "133;"):
		rest := payload[4:]
		switch {
		case rest == "C" || strings.HasPrefix(rest, "C;"):
			p.markC()
			// Some emitters (fish ≥4.0, following the kitty shell-
			// integration protocol) attach the command line itself as a
			// percent-encoded C-mark parameter. Surface it as a hint so
			// sessions without our snippet still get command text.
			if strings.HasPrefix(rest, "C;") && p.ev.CmdHint != nil {
				params := rest[2:]
				if q, found := strings.CutPrefix(params, "cmdline="); found {
					// kitty's bash/zsh integrations attach the command
					// line shell-quoted (printf %q). %q output can
					// contain raw semicolons (`\;`, or inside $'...'),
					// so the whole tail is the value — kitty emits no
					// further params after it.
					if cmd := shellUnquote(q); cmd != "" {
						p.ev.CmdHint(cmd)
					}
				} else {
					for _, param := range strings.Split(params, ";") {
						if enc, found := strings.CutPrefix(param, "cmdline_url="); found {
							p.ev.CmdHint(percentDecode(enc))
						}
					}
				}
			}
		case rest == "D" || strings.HasPrefix(rest, "D;"):
			p.markD(rest)
		case rest == "A" || strings.HasPrefix(rest, "A;"):
			// iTerm2 wraps PS2 continuation prompts in 133;A;k=s
			// (Semantic Prompt "secondary", where kitty uses P;k=s):
			// the B mark that follows *continues* the current input
			// region across the continuation line. A plain A is a new
			// prompt cycle: any un-executed input echo is stale.
			if strings.HasPrefix(rest, "A;") &&
				strings.Contains(";"+rest[2:]+";", ";k=s;") && p.echoInput {
				p.echoPhase = cellPrompt
				p.echoAdd(echoSentPrompt)
			} else {
				p.echoPromptStart()
			}
		case rest == "B" || strings.HasPrefix(rest, "B;"):
			p.markB()
		case strings.HasPrefix(rest, "P;"):
			// kitty prompt-kind mark: k=s flags a secondary (PS2)
			// prompt, whose B mark *continues* the current input
			// region across the continuation line; anything else is a
			// primary prompt starting a fresh region
			if strings.Contains(";"+rest[2:]+";", ";k=s;") && p.echoInput {
				p.echoPhase = cellPrompt
				p.echoAdd(echoSentPrompt)
			} else {
				p.echoPromptStart()
			}
		}
	case strings.HasPrefix(payload, "633;"):
		// VS Code's shell-integration protocol (OSC 633) — same A/B/C/D
		// shape as OSC 133, plus E (the literal command line) and
		// P;Cwd=<path>. Emitted by VS Code's shellIntegration-{bash,zsh,
		// fish}.sh, which users install manually in rc files for tmux/SSH
		// setups — so a shell inside `backscroll run` can be emitting
		// these with no backscroll snippet at all. All 633 sequences are
		// consumed (never spliced into recorded output): they are
		// host-terminal metadata, and replaying them would confuse the
		// host's own shell-integration state.
		rest := payload[4:]
		switch {
		case rest == "C" || strings.HasPrefix(rest, "C;"):
			p.markC()
		case rest == "D" || strings.HasPrefix(rest, "D;"):
			p.markD(rest)
		case rest == "A" || strings.HasPrefix(rest, "A;"):
			p.echoPromptStart()
		case rest == "B" || strings.HasPrefix(rest, "B;"):
			p.markB()
		case strings.HasPrefix(rest, "E;"):
			// 633;E;<cmdline>;<nonce> — nonce is optional; semicolons
			// inside the command are escaped as \x3b, so splitting on the
			// last raw ';' is safe.
			if p.ev.CmdHint != nil {
				params := rest[2:]
				cmd := params
				if i := strings.LastIndexByte(params, ';'); i >= 0 {
					cmd = params[:i]
				}
				p.ev.CmdHint(vscUnescape(cmd))
			}
		case strings.HasPrefix(rest, "P;Cwd="):
			if p.ev.Cwd != nil {
				p.ev.Cwd(vscUnescape(rest[len("P;Cwd="):]))
			}
		}
		// A/B/F/G/P;*/Env* etc.: prompt boundaries and terminal metadata;
		// nothing to capture.
	case strings.HasPrefix(payload, "1337;CurrentDir="):
		// iTerm2's shell integration reports the working directory as a
		// raw path (not an OSC 7 file:// URL) after every command.
		// Consumed, not stored: replaying it from `show --raw` would
		// clobber the live terminal's cwd state.
		if p.ev.Cwd != nil {
			p.ev.Cwd(payload[len("1337;CurrentDir="):])
		}
	case strings.HasPrefix(payload, "1337;RemoteHost="),
		strings.HasPrefix(payload, "1337;ShellIntegrationVersion="):
		// iTerm2 integration metadata directed at the host terminal;
		// stateful (RemoteHost drives iTerm2's ssh/profile switching),
		// so keep it out of stored output for the same replay reason.
		// All other 1337 (File= inline images, ...) passes through.
	case strings.HasPrefix(payload, "1337;SetUserVar=WEZTERM_PROG="):
		// WezTerm's shell integration reports the command line at preexec
		// as a base64 user var (after its plain `133;C;` mark). It's the
		// only command-text source in a manually-installed wezterm.sh
		// setup, so surface it as a hint. Consumed, not stored: it is
		// host-terminal state, and replaying it would clobber the user's
		// live WEZTERM_PROG. An empty value (sent at precmd to clear the
		// var) is ignored. All other OSC 1337 (SetUserVar for other
		// vars, iTerm2 File= inline images, ...) passes through
		// untouched below.
		//
		// Word-splitting gotcha: __wezterm_set_user_var encodes with an
		// UNQUOTED `echo -n ... | base64` substitution. GNU base64 wraps
		// at 76 columns, the shell word-splits the lines, and printf
		// re-uses its format string for the extras — so any value longer
		// than 57 bytes arrives as SetUserVar=WEZTERM_PROG=<first 76
		// chars> followed by garbage sequences SetUserVar=<line>=<line>
		// pairing up the remaining lines. A bare 76-char first chunk
		// therefore means "possibly split": buffer it and reassemble
		// from the continuations (they are back-to-back writes from one
		// printf; anything else ends the sequence). Also strip embedded
		// whitespace so a future quoted-substitution wezterm.sh (raw
		// newlines in one payload) decodes too.
		enc := stripSpace(payload[len("1337;SetUserVar=WEZTERM_PROG="):])
		if len(enc) == 76 {
			p.wezProg = append(p.wezProg[:0], enc...)
		} else {
			p.emitWezProg(enc)
		}
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
// emitWezProg decodes a (possibly reassembled) WEZTERM_PROG value and
// surfaces it as a command hint.
func (p *Parser) emitWezProg(enc string) {
	if p.ev.CmdHint == nil || enc == "" {
		return
	}
	if raw, err := base64.StdEncoding.DecodeString(enc); err == nil && len(raw) > 0 {
		p.ev.CmdHint(string(raw))
	}
}

// wezContinuation reports whether an OSC payload is a continuation of a
// word-split SetUserVar value: SetUserVar=<b64 line>=<b64 line>, where
// printf paired up two leftover lines (the second may be empty, and only
// the trailing fragment may carry '=' padding). Returns the concatenated
// base64 fragments.
func wezContinuation(payload string) (string, bool) {
	rest, found := strings.CutPrefix(payload, "1337;SetUserVar=")
	if !found {
		return "", false
	}
	i := strings.IndexByte(rest, '=')
	if i <= 0 {
		return "", false
	}
	a, b := rest[:i], rest[i+1:]
	if !isB64(a, false) || !isB64(b, true) {
		return "", false
	}
	return a + b, true
}

// isB64 reports whether s consists only of base64 alphabet characters,
// optionally followed by '=' padding. Real user-var names (WEZTERM_USER,
// ...) contain '_' and never match.
func isB64(s string, allowPad bool) bool {
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' {
			continue
		}
		break
	}
	if allowPad {
		for ; i < len(s); i++ {
			if s[i] != '=' {
				return false
			}
		}
	}
	return i == len(s)
}

func parseOSC7(s string) string {
	// Strip scheme + authority, keep the path. Emitters differ on the
	// scheme: file:// (wezterm, VS Code, our snippets) vs
	// kitty-shell-cwd:// (kitty's shell integration).
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
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

// vscUnescape undoes VS Code's OSC 633 value escaping: `\\` for backslash
// and `\xHH` for semicolons (`\x3b`) and control bytes (e.g. `\x0a` in
// multiline commands). Malformed escapes are left as-is.
func vscUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\\' {
				b.WriteByte('\\')
				i++
				continue
			}
			if s[i+1] == 'x' && i+3 < len(s) {
				if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(n))
					i += 3
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
