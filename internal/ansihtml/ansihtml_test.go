package ansihtml

import (
	"strings"
	"testing"
)

func TestPlain(t *testing.T) {
	got := Render([]byte("hello world\nline two"))
	want := "hello world\nline two"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEscaping(t *testing.T) {
	got := Render([]byte(`<script>&"`))
	want := `&lt;script&gt;&amp;"`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestBasicColor(t *testing.T) {
	got := Render([]byte("\x1b[31mred\x1b[0m plain"))
	want := `<span class="f1">red</span> plain`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestBoldBrightAndBg(t *testing.T) {
	got := Render([]byte("\x1b[1;92;44mX\x1b[m"))
	want := `<span class="f10 b4 bo">X</span>`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func Test256AndTruecolor(t *testing.T) {
	got := Render([]byte("\x1b[38;5;196ma\x1b[0m\x1b[48;2;1;2;3mb\x1b[0m\x1b[38;5;3mc\x1b[0m"))
	want := `<span style="color:#ff0000">a</span><span style="background:#010203">b</span><span class="f3">c</span>`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestColonSubparams(t *testing.T) {
	got := Render([]byte("\x1b[38:5:196ma\x1b[0m"))
	want := `<span style="color:#ff0000">a</span>`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestReverse(t *testing.T) {
	// plain reverse -> rv class; colored reverse swaps fg/bg
	if got, want := Render([]byte("\x1b[7mX\x1b[0m")), `<span class="rv">X</span>`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := Render([]byte("\x1b[7;31mX\x1b[0m")), `<span class="b1">X</span>`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCROverwrite(t *testing.T) {
	got := Render([]byte("progress 10%\rprogress 99%\rdone\nnext"))
	want := "done\nnext"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCRLFIsNewline(t *testing.T) {
	got := Render([]byte("a\r\nb"))
	if got != "a\nb" {
		t.Fatalf("got %q", got)
	}
}

func TestBackspaceAcrossSegs(t *testing.T) {
	got := Render([]byte("ab\x1b[31mc\x1b[0m\x08\x08z"))
	want := "az"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOSCAndCSIDropped(t *testing.T) {
	got := Render([]byte("\x1b]0;title\x07a\x1b[2Kb\x1b[10;20Hc"))
	want := "abc"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMalformedExtColor(t *testing.T) {
	// bad 38 param: rest of that SGR is dropped but text survives
	got := Render([]byte("\x1b[38;9mX"))
	if got != "X" {
		t.Fatalf("got %q", got)
	}
	got = Render([]byte("\x1b[38;5;999mY"))
	if got != "Y" {
		t.Fatalf("got %q", got)
	}
}

func TestStyleMergesAdjacent(t *testing.T) {
	got := Render([]byte("\x1b[31ma\x1b[31mb\x1b[0m"))
	want := `<span class="f1">ab</span>`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTruncatedEscapeAtEnd(t *testing.T) {
	got := Render([]byte("ok\x1b["))
	if got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestPalette256Corners(t *testing.T) {
	if palette256(16) != "#000000" {
		t.Fatal(palette256(16))
	}
	if palette256(231) != "#ffffff" {
		t.Fatal(palette256(231))
	}
	if palette256(232) != "#080808" {
		t.Fatal(palette256(232))
	}
	if palette256(255) != "#eeeeee" {
		t.Fatal(palette256(255))
	}
}

func TestNoUnclosedSpans(t *testing.T) {
	got := Render([]byte("\x1b[31mred with no reset\nand more"))
	if strings.Count(got, "<span") != strings.Count(got, "</span>") {
		t.Fatalf("unbalanced spans: %q", got)
	}
}
