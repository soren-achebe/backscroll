package record

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

type collected struct {
	cmds  []string
	hints []string
	cwds  []string
	outs  []string
	exits []int
	buf   bytes.Buffer
	open  bool
}

func collector() (*Parser, *collected) {
	c := &collected{}
	p := NewParser(Events{
		CmdText: func(s string) { c.cmds = append(c.cmds, s) },
		CmdHint: func(s string) { c.hints = append(c.hints, s) },
		Cwd:     func(s string) { c.cwds = append(c.cwds, s) },
		OutStart: func() {
			c.buf.Reset()
			c.open = true
		},
		Output: func(b []byte) {
			if c.open {
				c.buf.Write(b)
			}
		},
		OutEnd: func(code int, ok bool) {
			c.outs = append(c.outs, c.buf.String())
			c.exits = append(c.exits, code)
			c.open = false
		},
	})
	return p, c
}

func osc(payload string) string { return "\x1b]" + payload + "\x07" }

func mark(cmd string) string {
	return osc("6973;cmd="+base64.StdEncoding.EncodeToString([]byte(cmd))) + osc("133;C")
}

func TestBasicSegmentation(t *testing.T) {
	p, c := collector()
	stream := "prompt$ " + mark("echo hi") + "hi\r\n" + osc("133;D;0") +
		osc("7;file://host/home/x") + osc("133;A") + "prompt$ "
	p.Feed([]byte(stream))
	if len(c.cmds) != 1 || c.cmds[0] != "echo hi" {
		t.Fatalf("cmds = %v", c.cmds)
	}
	if len(c.outs) != 1 || c.outs[0] != "hi\r\n" {
		t.Fatalf("outs = %q", c.outs)
	}
	if c.exits[0] != 0 {
		t.Fatalf("exit = %v", c.exits)
	}
	if len(c.cwds) != 1 || c.cwds[0] != "/home/x" {
		t.Fatalf("cwds = %v", c.cwds)
	}
}

func TestChunkedAcrossBoundaries(t *testing.T) {
	stream := mark("x") + "some output\r\n" + osc("133;D;3")
	for chunk := 1; chunk <= 7; chunk++ {
		p, c := collector()
		b := []byte(stream)
		for i := 0; i < len(b); i += chunk {
			end := i + chunk
			if end > len(b) {
				end = len(b)
			}
			p.Feed(b[i:end])
		}
		if len(c.outs) != 1 || c.outs[0] != "some output\r\n" || c.exits[0] != 3 {
			t.Fatalf("chunk=%d outs=%q exits=%v", chunk, c.outs, c.exits)
		}
	}
}

func TestAltScreenExcluded(t *testing.T) {
	p, c := collector()
	stream := mark("vim") +
		"before" +
		"\x1b[?1049h" + "ALTSCREEN GARBAGE" + "\x1b[?1049l" +
		"after" + osc("133;D;0")
	p.Feed([]byte(stream))
	if len(c.outs) != 1 {
		t.Fatalf("outs = %v", c.outs)
	}
	if bytes.Contains([]byte(c.outs[0]), []byte("GARBAGE")) {
		t.Fatalf("alt-screen content captured: %q", c.outs[0])
	}
	if !bytes.Contains([]byte(c.outs[0]), []byte("before")) || !bytes.Contains([]byte(c.outs[0]), []byte("after")) {
		t.Fatalf("normal content missing: %q", c.outs[0])
	}
}

func TestDWithoutCIsIgnored(t *testing.T) {
	p, c := collector()
	p.Feed([]byte(osc("133;D;0") + osc("133;A") + "prompt$ "))
	if len(c.outs) != 0 {
		t.Fatalf("unexpected segment: %v", c.outs)
	}
}

func TestForeignOSCPreservedInCapture(t *testing.T) {
	p, c := collector()
	title := osc("0;my title")
	p.Feed([]byte(mark("x") + "a" + title + "b" + osc("133;D;0")))
	want := "a" + title + "b"
	if c.outs[0] != want {
		t.Fatalf("got %q want %q", c.outs[0], want)
	}
}

func TestSTTerminator(t *testing.T) {
	p, c := collector()
	stream := "\x1b]6973;cmd=" + base64.StdEncoding.EncodeToString([]byte("ls")) + "\x1b\\" +
		"\x1b]133;C\x1b\\" + "out" + "\x1b]133;D;0\x1b\\"
	p.Feed([]byte(stream))
	if len(c.cmds) != 1 || c.cmds[0] != "ls" || c.outs[0] != "out" {
		t.Fatalf("cmds=%v outs=%v", c.cmds, c.outs)
	}
}

func TestCapBuf(t *testing.T) {
	b := newCapBuf(4, 4)
	b.Write([]byte("0123456789abcdef"))
	got := string(b.Bytes())
	if !b.Truncated() {
		t.Fatal("expected truncation")
	}
	if want := "0123"; got[:4] != want {
		t.Fatalf("head = %q", got[:4])
	}
	if want := "cdef"; got[len(got)-4:] != want {
		t.Fatalf("tail = %q", got[len(got)-4:])
	}
	small := newCapBuf(4, 4)
	small.Write([]byte("hi"))
	if small.Truncated() || string(small.Bytes()) != "hi" {
		t.Fatalf("small = %q", small.Bytes())
	}
}

func TestManyCommands(t *testing.T) {
	p, c := collector()
	var stream bytes.Buffer
	for i := 0; i < 100; i++ {
		stream.WriteString(mark(fmt.Sprintf("cmd%d", i)))
		stream.WriteString(fmt.Sprintf("output %d\r\n", i))
		stream.WriteString(osc("133;D;0"))
	}
	p.Feed(stream.Bytes())
	if len(c.outs) != 100 || len(c.cmds) != 100 {
		t.Fatalf("got %d outs %d cmds", len(c.outs), len(c.cmds))
	}
	if c.cmds[42] != "cmd42" || c.outs[42] != "output 42\r\n" {
		t.Fatalf("cmd42=%q out42=%q", c.cmds[42], c.outs[42])
	}
}

func TestToggleOSC(t *testing.T) {
	var states []bool
	p := NewParser(Events{Toggle: func(on bool) { states = append(states, on) }})
	p.Feed([]byte("\x1b]6973;rec=off\x07some text\x1b]6973;rec=on\x07"))
	if len(states) != 2 || states[0] != false || states[1] != true {
		t.Fatalf("got %v, want [false true]", states)
	}
}

// TestCapBufRandom cross-checks the ring-buffer tail against a naive
// reference (keep everything, slice the end) across many random write
// patterns, including single bytes, exact-cap, and larger-than-cap chunks.
func TestCapBufRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 500; trial++ {
		headCap := 1 + rng.Intn(16)
		tailCap := 1 + rng.Intn(16)
		b := newCapBuf(headCap, tailCap)
		var all []byte
		writes := 1 + rng.Intn(20)
		for w := 0; w < writes; w++ {
			n := rng.Intn(3 * tailCap)
			chunk := make([]byte, n)
			for i := range chunk {
				chunk[i] = byte('a' + rng.Intn(26))
			}
			b.Write(chunk)
			all = append(all, chunk...)
		}
		wantHead := all
		if len(wantHead) > headCap {
			wantHead = all[:headCap]
		}
		rest := all[len(wantHead):]
		wantTail := rest
		if len(wantTail) > tailCap {
			wantTail = rest[len(rest)-tailCap:]
		}
		got := b.Bytes()
		if !bytes.HasPrefix(got, wantHead) || !bytes.HasSuffix(got, wantTail) {
			t.Fatalf("trial %d (head=%d tail=%d): got %q want head %q tail %q",
				trial, headCap, tailCap, got, wantHead, wantTail)
		}
		if b.Truncated() != (len(all) > headCap+tailCap) {
			t.Fatalf("trial %d: Truncated=%v len=%d", trial, b.Truncated(), len(all))
		}
	}
}

func TestIsShellExit(t *testing.T) {
	yes := []string{"exit", "logout", "exit 0", "exit 130", " exit ", "exit  1"}
	no := []string{"", "exits", "exit now", "exit 1 2", "logout 1", "git exit", "exit -1", "echo exit"}
	for _, c := range yes {
		if !isShellExit(c) {
			t.Errorf("isShellExit(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if isShellExit(c) {
			t.Errorf("isShellExit(%q) = true, want false", c)
		}
	}
}

// fish ≥4.0 emits its own OSC 133 marks with the command line attached to
// the C mark as a percent-encoded cmdline_url parameter (kitty shell
// integration protocol). We surface it as a CmdHint.
func TestCmdlineURLHint(t *testing.T) {
	p, c := collector()
	p.Feed([]byte(osc("133;C;cmdline_url=echo%20hello%3B%20false") + "out" + osc("133;D;0")))
	if len(c.hints) != 1 || c.hints[0] != "echo hello; false" {
		t.Fatalf("hints = %q", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != "out" {
		t.Fatalf("outs = %q", c.outs)
	}
	// plain C marks and other params produce no hint
	p.Feed([]byte(osc("133;C") + osc("133;D;0")))
	p.Feed([]byte(osc("133;C;special_key=1") + osc("133;D;0")))
	if len(c.hints) != 1 {
		t.Fatalf("unexpected extra hints: %q", c.hints)
	}
	// multiple params: hint found regardless of position
	p.Feed([]byte(osc("133;C;special_key=1;cmdline_url=ls%20-la") + osc("133;D;0")))
	if len(c.hints) != 2 || c.hints[1] != "ls -la" {
		t.Fatalf("hints = %q", c.hints)
	}
	// malformed percent escapes pass through undecoded rather than panicking
	p.Feed([]byte(osc("133;C;cmdline_url=100%zz%2") + osc("133;D;0")))
	if c.hints[2] != "100%zz%2" {
		t.Fatalf("malformed = %q", c.hints[2])
	}
}

// Double emission: a terminal-native C mark (with cmdline_url) followed by
// our snippet's 6973;cmd + plain C, then one output span and two D marks —
// exactly what fish ≥4.0 plus our integration produces. The parser must
// report one command span, both texts, and ignore the trailing D.
func TestNativePlusSnippetDoubleEmission(t *testing.T) {
	p, c := collector()
	p.Feed([]byte(
		osc("133;A") + osc("133;B") +
			osc("133;C;cmdline_url=false") +
			mark("false") + // 6973;cmd=... + our own 133;C
			osc("133;D;1") + osc("133;D;1") +
			osc("133;A")))
	if len(c.outs) != 1 || len(c.exits) != 1 || c.exits[0] != 1 {
		t.Fatalf("outs=%q exits=%v", c.outs, c.exits)
	}
	if len(c.hints) != 1 || c.hints[0] != "false" || len(c.cmds) != 1 || c.cmds[0] != "false" {
		t.Fatalf("hints=%q cmds=%q", c.hints, c.cmds)
	}
	if c.outs[0] != "" {
		t.Fatalf("marks leaked into output: %q", c.outs[0])
	}
}

// --- OSC 633 (VS Code shell integration) ---

func TestOSC633Segmentation(t *testing.T) {
	p, c := collector()
	// Realistic VS Code bash stream: A/B prompt marks, E (cmdline+nonce),
	// C, output, D;status, P;Cwd.
	stream := osc("633;A") + "prompt$ " + osc("633;B") +
		osc("633;E;echo hi;a1b2") + osc("633;C") + "hi\r\n" +
		osc("633;D;0") + osc("633;P;Cwd=/home/x") + osc("633;A")
	p.Feed([]byte(stream))
	if len(c.hints) != 1 || c.hints[0] != "echo hi" {
		t.Fatalf("hints = %v", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != "hi\r\n" {
		t.Fatalf("outs = %q", c.outs)
	}
	if c.exits[0] != 0 {
		t.Fatalf("exit = %v", c.exits)
	}
	if len(c.cwds) != 1 || c.cwds[0] != "/home/x" {
		t.Fatalf("cwds = %v", c.cwds)
	}
}

func TestOSC633EUnescape(t *testing.T) {
	cases := []struct{ in, want string }{
		{`echo hi`, "echo hi"},
		{`echo a\x3b b`, "echo a; b"},           // escaped semicolon
		{`printf '%s\\n' x`, `printf '%s\n' x`}, // escaped backslash
		{`line1\x0aline2`, "line1\nline2"},      // multiline command
		{`tab\x09sep`, "tab\tsep"},              // control byte
		{`trailing\`, `trailing\`},              // malformed: lone backslash
		{`bad\xZZesc`, `bad\xZZesc`},            // malformed: non-hex
	}
	for _, tc := range cases {
		p, c := collector()
		p.Feed([]byte(osc("633;E;" + tc.in + ";nonce")))
		if len(c.hints) != 1 || c.hints[0] != tc.want {
			t.Errorf("E;%q: hints = %q, want [%q]", tc.in, c.hints, tc.want)
		}
	}
	// No nonce at all: whole params section is the command.
	p, c := collector()
	p.Feed([]byte(osc("633;E;justcmd")))
	if len(c.hints) != 1 || c.hints[0] != "justcmd" {
		t.Fatalf("no-nonce hints = %v", c.hints)
	}
}

func TestOSC633ConsumedNotSpliced(t *testing.T) {
	// 633 sequences arriving mid-capture (e.g. a nested integrated shell)
	// must never end up in the recorded output.
	p, c := collector()
	stream := mark("bash") + "before" +
		osc("633;P;PromptType=starship") + osc("633;B") + osc("633;F") +
		"after" + osc("133;D;0")
	p.Feed([]byte(stream))
	if len(c.outs) != 1 || c.outs[0] != "beforeafter" {
		t.Fatalf("outs = %q", c.outs)
	}
}

func TestOSC633SnippetPrecedence(t *testing.T) {
	// Both our snippet (6973+133) and VS Code's integration (633) active:
	// authoritative 6973 text arrives alongside the 633 E hint; dup C/D
	// marks must not produce extra segments.
	p, c := collector()
	stream := osc("633;E;echo hi;n") + osc("633;C") + // vsc preexec first
		mark("echo hi") + // our preexec: 6973 + 133;C (resets buf, pre-output)
		"hi\r\n" + osc("133;D;0") + osc("633;D;0")
	p.Feed([]byte(stream))
	if len(c.cmds) != 1 || c.cmds[0] != "echo hi" {
		t.Fatalf("cmds = %v", c.cmds)
	}
	if len(c.hints) != 1 {
		t.Fatalf("hints = %v", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != "hi\r\n" {
		t.Fatalf("outs = %q (dup D must not double-flush)", c.outs)
	}
}

func TestOSC633EmptyPromptDIgnored(t *testing.T) {
	// VS Code emits a bare 633;D (no status) for an empty-prompt Enter;
	// with no preceding C it must be ignored.
	p, c := collector()
	p.Feed([]byte(osc("633;A") + "$ " + osc("633;B") + osc("633;D") + osc("633;A")))
	if len(c.outs) != 0 {
		t.Fatalf("outs = %q", c.outs)
	}
}

// kitty's shell integration (bash + zsh) attaches the command line to the
// C mark shell-quoted: OSC 133;C;cmdline=%q. The value can contain raw
// semicolons (escaped as \; or inside $'...'), so it must be taken as the
// whole tail of the payload, not split on ';'.
func TestKittyCmdlineHint(t *testing.T) {
	p, c := collector()
	// bash printf %q form
	p.Feed([]byte(osc(`133;C;cmdline=echo\ \"hi\ there\"`) + "out" + osc("133;D;0")))
	if len(c.hints) != 1 || c.hints[0] != `echo "hi there"` {
		t.Fatalf("hints = %q", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != "out" {
		t.Fatalf("outs = %q", c.outs)
	}
	// raw semicolon in the command: whole-tail parse, no param splitting
	p.Feed([]byte(osc(`133;C;cmdline=echo\ a\;b`) + osc("133;D;0")))
	if len(c.hints) != 2 || c.hints[1] != "echo a;b" {
		t.Fatalf("hints = %q", c.hints)
	}
	// bash $'...' form with control chars
	p.Feed([]byte(osc(`133;C;cmdline=$'line1\nline2'`) + osc("133;D;1")))
	if len(c.hints) != 3 || c.hints[2] != "line1\nline2" {
		t.Fatalf("hints = %q", c.hints)
	}
	// zsh mixed-segment form
	p.Feed([]byte(osc(`133;C;cmdline=printf$'\t'x`) + osc("133;D;0")))
	if len(c.hints) != 4 || c.hints[3] != "printf\tx" {
		t.Fatalf("hints = %q", c.hints)
	}
	// empty cmdline produces no hint
	p.Feed([]byte(osc("133;C;cmdline=") + osc("133;D;0")))
	if len(c.hints) != 4 {
		t.Fatalf("empty cmdline produced a hint: %q", c.hints)
	}
	if len(c.exits) != 5 {
		t.Fatalf("exits = %v", c.exits)
	}
}

// WezTerm's shell integration reports the command line as a base64 user
// var (OSC 1337 SetUserVar=WEZTERM_PROG) right after its plain 133;C.
// The var must become a hint, be consumed (never stored in output), and
// other OSC 1337 traffic must still pass through untouched.
func TestWeztermProgHint(t *testing.T) {
	p, c := collector()
	b64 := base64.StdEncoding.EncodeToString([]byte("cargo build --release"))
	p.Feed([]byte(osc("133;C;") + osc("1337;SetUserVar=WEZTERM_PROG="+b64) + "out" + osc("133;D;0")))
	if len(c.hints) != 1 || c.hints[0] != "cargo build --release" {
		t.Fatalf("hints = %q", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != "out" {
		t.Fatalf("SetUserVar leaked into output: %q", c.outs)
	}

	// `echo -n | base64` wraps at 76 cols: embedded newlines must be stripped
	long := "echo " + strings.Repeat("x", 100)
	wrapped := ""
	for i, ch := range base64.StdEncoding.EncodeToString([]byte(long)) {
		if i > 0 && i%76 == 0 {
			wrapped += "\n"
		}
		wrapped += string(ch)
	}
	if !strings.Contains(wrapped, "\n") {
		t.Fatal("test bug: wrapped b64 has no newline")
	}
	p.Feed([]byte(osc("133;C;") + osc("1337;SetUserVar=WEZTERM_PROG="+wrapped) + osc("133;D;0")))
	if len(c.hints) != 2 || c.hints[1] != long {
		t.Fatalf("wrapped hint = %q", c.hints)
	}

	// empty value (precmd clear) produces no hint
	p.Feed([]byte(osc("133;C;") + osc("1337;SetUserVar=WEZTERM_PROG=") + osc("133;D;0")))
	if len(c.hints) != 2 {
		t.Fatalf("empty PROG produced a hint: %q", c.hints)
	}

	// other user vars and iTerm2-style File= payloads pass through into capture
	other := osc("1337;SetUserVar=WEZTERM_USER=YWxpY2U=") + osc("1337;File=name=eC5wbmc=:AAAA")
	p.Feed([]byte(osc("133;C;") + other + osc("133;D;0")))
	if len(c.outs) != 4 || c.outs[3] != other {
		t.Fatalf("foreign 1337 not preserved: %q", c.outs[3])
	}
	if len(c.hints) != 2 {
		t.Fatalf("foreign 1337 produced a hint: %q", c.hints)
	}
}

func TestParseOSC7Schemes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file://host/home/u/dir", "/home/u/dir"},
		{"file:///home/u/dir", "/home/u/dir"},
		{"file://host/with%20space", "/with space"},
		{"kitty-shell-cwd://host/home/u/dir", "/home/u/dir"},
		{"/plain/path", "/plain/path"},
	}
	for _, c := range cases {
		if got := parseOSC7(c.in); got != c.want {
			t.Errorf("parseOSC7(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// wezterm.sh's __wezterm_set_user_var uses an UNQUOTED `echo -n | base64`
// substitution; GNU base64 wraps at 76 cols, the shell word-splits the
// lines, and printf re-uses its format — so long command lines arrive as
// a 76-char first chunk plus garbage SetUserVar=<line>=<line> sequences.
// The parser must reassemble them (and consume the shrapnel).
func wezSplit(cmd string) []string {
	b64 := base64.StdEncoding.EncodeToString([]byte(cmd))
	var lines []string
	for len(b64) > 76 {
		lines, b64 = append(lines, b64[:76]), b64[76:]
	}
	lines = append(lines, b64)
	oscs := []string{osc("1337;SetUserVar=WEZTERM_PROG=" + lines[0])}
	rest := lines[1:]
	for len(rest) > 0 {
		a := rest[0]
		b := ""
		if len(rest) > 1 {
			b = rest[1]
			rest = rest[2:]
		} else {
			rest = rest[1:]
		}
		oscs = append(oscs, osc("1337;SetUserVar="+a+"="+b))
	}
	return oscs
}

func TestWeztermProgWordSplit(t *testing.T) {
	cases := []string{
		"echo " + strings.Repeat("x", 120),  // 2 continuation lines (one pair)
		"echo " + strings.Repeat("y", 215),  // odd lines -> trailing SetUserVar=<L>=
		"echo " + strings.Repeat("z", 119),  // final line carries '=' padding
		strings.Repeat("a", 57),             // exactly 76 b64 chars: no continuation at all
	}
	for _, cmd := range cases {
		p, c := collector()
		stream := osc("133;C;")
		for _, o := range wezSplit(cmd) {
			stream += o
		}
		stream += "out" + osc("133;D;0")
		p.Feed([]byte(stream))
		if len(c.hints) != 1 || c.hints[0] != cmd {
			t.Fatalf("cmd len %d: hints = %d %.40q", len(cmd), len(c.hints), c.hints)
		}
		if len(c.outs) != 1 || c.outs[0] != "out" {
			t.Fatalf("cmd len %d: shrapnel leaked into output: %q", len(cmd), c.outs)
		}
	}
}

// A pending chunk must be flushed (not swallowed) when the next OSC is a
// real user var, and that var must still pass through into capture.
func TestWeztermProgPendingFlushedByRealVar(t *testing.T) {
	p, c := collector()
	cmd := strings.Repeat("a", 57) // exactly one full 76-char b64 line
	b64 := base64.StdEncoding.EncodeToString([]byte(cmd))
	userVar := osc("1337;SetUserVar=WEZTERM_USER=YWxpY2U=")
	p.Feed([]byte(osc("133;C;") + osc("1337;SetUserVar=WEZTERM_PROG="+b64) +
		userVar + "out" + osc("133;D;0")))
	if len(c.hints) != 1 || c.hints[0] != cmd {
		t.Fatalf("hints = %q", c.hints)
	}
	if len(c.outs) != 1 || c.outs[0] != userVar+"out" {
		t.Fatalf("outs = %q", c.outs)
	}
}

// Raw newlines inside a single payload (what a fixed/quoted wezterm.sh
// would send, post-ONLCR) must also decode.
func TestWeztermProgHintCRLF(t *testing.T) {
	long := "echo " + strings.Repeat("x", 120)
	b64 := base64.StdEncoding.EncodeToString([]byte(long))
	wrapped := ""
	for i, ch := range b64 {
		if i > 0 && i%76 == 0 {
			wrapped += "\r\n"
		}
		wrapped += string(ch)
	}
	var got string
	p := NewParser(Events{CmdHint: func(s string) { got = s }})
	p.Feed([]byte(osc("133;C;") + osc("1337;SetUserVar=WEZTERM_PROG="+wrapped) + "out" + osc("133;D;0")))
	if got != long {
		t.Fatalf("got len=%d want len=%d", len(got), len(long))
	}
}
