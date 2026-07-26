package record

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
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
