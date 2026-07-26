package record

import (
	"strings"
	"testing"
)

func TestEchoPlainTyping(t *testing.T) {
	// simplest case: characters echoed one at a time, Enter at the end
	got := ReconstructEcho([]byte("ls -la\r\n"), 80)
	if got != "ls -la" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoUTF8(t *testing.T) {
	got := ReconstructEcho([]byte("echo día ✓\r\n"), 80)
	if got != "echo día ✓" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoBackspaceRubout(t *testing.T) {
	// readline erases with BS SP BS
	got := ReconstructEcho([]byte("cat foX\b \bo\r\n"), 80)
	if got != "cat foo" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoMidLineInsertRedraw(t *testing.T) {
	// user types "gt status", arrows back over "t status", inserts "i":
	// readline reprints the tail and backspaces the cursor into place
	seq := "gt status" +
		strings.Repeat("\b", 8) + // cursor left to after "g"
		"it status" + // reprint with insertion
		strings.Repeat("\b", 8) // cursor back
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "git status" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoMidLineInsertICH(t *testing.T) {
	// terminals with insert-char capability: CSI @ shifts the tail right
	seq := "gt status" +
		strings.Repeat("\b", 8) +
		"\x1b[1@i"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "git status" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoDeleteCharDCH(t *testing.T) {
	seq := "gitt log" +
		strings.Repeat("\b", 5) + // cursor to second 't'
		"\x1b[1P" // delete it
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "git log" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoKillLineCtrlU(t *testing.T) {
	// ^U: cursor to col 0, erase to end, retype
	seq := "wrong cmd\r\x1b[Kmake test"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "make test" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoSGRIgnored(t *testing.T) {
	// zsh syntax highlighting recolors as you type
	seq := "\x1b[32mls\x1b[0m \x1b[4m/tmp\x1b[24m"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "ls /tmp" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoAutowrapLongLine(t *testing.T) {
	// line longer than the terminal width: autowrap joins seamlessly
	long := "echo " + strings.Repeat("a", 100)
	got := ReconstructEcho([]byte(long+"\r\n"), 40)
	if got != long {
		t.Fatalf("got %q", got)
	}
}

func TestEchoMultilinePS2(t *testing.T) {
	// continuation line: Enter echoes \r\n, shell prints PS2 "> "
	seq := "for f in *; do\r\n> echo $f; done"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "for f in *; do\n> echo $f; done" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoRightPromptTrimmed(t *testing.T) {
	// zsh RPROMPT: drawn at the right edge via cursor jump, so the cells
	// between input and decoration are never written
	w := 40
	rp := "14:32"
	seq := "\x1b[" + itoa(w-len(rp)) + "G" + rp + "\r" + "ls -la"
	got := ReconstructEcho([]byte(seq+"\r\n"), w)
	if got != "ls -la" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoInteriorSpacingKept(t *testing.T) {
	// typed spaces are written cells — never confused with a decoration
	// gap, even when the text reaches the right edge
	w := 20
	cmd := "echo 'a" + strings.Repeat(" ", 10) + "b'"
	got := ReconstructEcho([]byte(cmd+"\r\n"), w)
	if got != cmd {
		t.Fatalf("got %q", got)
	}
}

func TestEchoAltScreenFrozen(t *testing.T) {
	// fzf Ctrl-R with the full-screen UI: everything inside the alternate
	// screen is invisible; readline redraws the picked command after
	fzfNoise := "\x1b[?1049h" + "garbage \x1b[7mUI\x1b[0m bytes\r\nmore" + "\x1b[?1049l"
	seq := fzfNoise + "git push"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "git push" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoAbsoluteAddressingBails(t *testing.T) {
	// CUP/scroll regions can't be mapped into a prompt-relative grid:
	// refuse to guess
	for _, seq := range []string{
		"ls\x1b[2;5Hoops",
		"ls\x1b[5droof",
		"ls\x1b[2;10r",
		"ls\x1b[3S",
	} {
		if got := ReconstructEcho([]byte(seq), 80); got != "" {
			t.Fatalf("seq %q: expected bail, got %q", seq, got)
		}
	}
}

func TestEchoEraseToEndAfterShorterRetype(t *testing.T) {
	// history-search replaces a long line with a shorter one:
	// CR, retype, erase leftovers
	seq := "very long previous command" + "\r" + "ls\x1b[K"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "ls" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoOSCAndDCSSkipped(t *testing.T) {
	seq := "l\x1b]2;title\x07s\x1bP+q544e\x1b\\ -l"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "ls -l" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoCharsetDesignationConsumed(t *testing.T) {
	// ESC ( B must not leak a literal 'B'
	got := ReconstructEcho([]byte("\x1b(Bls\r\n"), 80)
	if got != "ls" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoEmpty(t *testing.T) {
	if got := ReconstructEcho(nil, 80); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := ReconstructEcho([]byte("\r\n"), 80); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := ReconstructEcho([]byte("\x1b[0m\x1b]2;x\x07"), 80); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoClearScreenKeepsLaterInput(t *testing.T) {
	// CSI 2J without cursor addressing (rare but legal): grid resets,
	// later typing survives
	seq := "junk\x1b[2J\rmake"
	got := ReconstructEcho([]byte(seq+"\r\n"), 80)
	if got != "make" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoRowCapBails(t *testing.T) {
	seq := strings.Repeat("x\r\n", echoMaxRows+10)
	if got := ReconstructEcho([]byte(seq), 80); got != "" {
		t.Fatalf("expected bail on row cap, got %q", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestEchoCtrlCAbortDetected(t *testing.T) {
	// a line killed at the prompt: bare ^C echo, no newline — never ran
	for _, seq := range []string{"^C", "half typed^C", "x\x1b[?2004l^C"} {
		if got := ReconstructEcho([]byte(seq), 80); got != "" {
			t.Fatalf("seq %q: expected abort, got %q", seq, got)
		}
	}
	// literal "^C" typed at the end of a real command: the echoed Enter
	// newline follows, so it is kept
	if got := ReconstructEcho([]byte("grep ^C\r\n"), 80); got != "grep ^C" {
		t.Fatalf("got %q", got)
	}
}

func TestEchoCtrlCAbortWithPromptCycleNoise(t *testing.T) {
	// the real VS Code bash phantom region: ^C, bracketed-paste off,
	// then the re-fired prompt cycle's CR/LF noise
	region := "^C\x1b[?2004l\r\x1b[?2004h\x1b[?2004l\r\r\n"
	if got := ReconstructEcho([]byte(region), 80); got != "" {
		t.Fatalf("expected abort, got %q", got)
	}
	region = "half typed^C\x1b[?2004l\r\r\n"
	if got := ReconstructEcho([]byte(region), 80); got != "" {
		t.Fatalf("expected abort, got %q", got)
	}
}
